package zlib

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Bot-detection markers observed on Z-Library mirrors (DiamWall, Cloudflare, etc).
// Mirrors behind these answer API calls with an HTML interstitial instead of JSON;
// retrying the same mirror cannot clear it, so the only fix is a different mirror.
var challengeMarkers = []string{
	"DiamWall", // 1lib.sk, z-library.sk (517 / 513)
	"Verifying your browser",
	"Checking your browser",
	"Just a moment",
	"/cdn-cgi/mitigation/",
	"/cdn-cgi/challenge-platform/",
	"/.well-known/diamwall/",
	"__cf_chl",
	"Access Denied",
	"cf_chl_opt",
}

// looksLikeBotChallenge reports whether an HTTP body is a bot-check interstitial
// rather than the JSON the API expects. Bounded to the head of the body so a large
// page cannot turn every failed request into a full-text scan.
func looksLikeBotChallenge(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	head := body
	if len(head) > 4096 {
		head = head[:4096]
	}
	lower := strings.ToLower(string(head))
	if !strings.Contains(lower, "<") && !strings.Contains(lower, "denied") {
		// Only HTML pages (or explicit denials) are treated as a challenge; a JSON
		// error message about a bot check still carries one of the markers.
		if !strings.Contains(lower, "diamwall") && !strings.Contains(lower, "challenge") {
			return false
		}
	}
	for _, m := range challengeMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// BotChallengeError marks a mirror that answered with a bot-check page instead of
// the API. Callers can distinguish it from a plain network/HTTP error and, unlike a
// transient failure, fall through to a different mirror instead of retrying.
type BotChallengeError struct {
	Base string
	URL  string
}

func (e *BotChallengeError) Error() string {
	if e.Base == "" && e.URL != "" {
		e.Base = e.URL
	}
	return fmt.Sprintf("Z-Library server %s is refusing automated access (bot check)", e.Base)
}

// Canned seed list of known mirrors, drawn from the KOReader plugin's SEED_URLS plus
// assets/domains.json. These are the operator's public mirrors; a working one is
// picked by discovery rather than assumed.
var seedMirrors = []string{
	"https://z-lib.fo",
	"https://z-library.sk",
	"https://1lib.sk",
	"https://z-lib.fm",
	"https://library-oceania.sk",
	"https://library-latin.sk",
	"https://library-asia.sk",
	"https://lib-africa.sk",
	"https://z-library.do",
	"https://z-lib.gd",
	"https://z-lib.gl",
	"https://z-library.la",
	"https://z-library.hn",
	"https://proxy.zlibraryproxies.workers.dev",
	"https://z-lib.gs",
}

// CDN endpoints (mirroring the plugin's fetchDynamicDomains) that serve the operator's
// curated assets/domains.json. Tried in order; they stay reachable when Z-Library is not.
var domainSources = []string{
	"https://fastly.jsdelivr.net/gh/ZlibraryKO/zlibrary.koplugin@main/assets/domains.json",
	"https://cdn.jsdelivr.net/gh/ZlibraryKO/zlibrary.koplugin@main/assets/domains.json",
	"https://raw.githubusercontent.com/ZlibraryKO/zlibrary.koplugin/main/assets/domains.json",
}

// domainsResponse mirrors assets/domains.json: {"success":1,"domains":[{"domain":...}]}.
type domainsResponse struct {
	Success int `json:"success"`
	Domains []struct {
		Domain string `json:"domain"`
	} `json:"domains"`
}

// fetchDynamicDomains pulls the operator's mirror list from a reachable CDN.
// Returns an error if no CDN answers (caller falls back to seedMirrors).
func fetchDynamicDomains(_ *http.Client) ([]string, error) {
	var lastErr error
	seen := make(map[string]bool)
	var out []string
	// This CDN fetch is a one-off quick probe; give it a bounded timeout even if the
	// caller handed over a long-timeout session client.
	client := &http.Client{Timeout: 10 * time.Second}
	for _, src := range domainSources {
		req, err := http.NewRequest(http.MethodGet, src, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		var dr domainsResponse
		if err := json.Unmarshal(body, &dr); err != nil {
			lastErr = err
			continue
		}
		if dr.Success != 1 {
			lastErr = fmt.Errorf("bad response: success=%d", dr.Success)
			continue
		}
		for _, d := range dr.Domains {
			d := strings.TrimSpace(d.Domain)
			if d == "" || strings.HasSuffix(strings.ToLower(d), ".onion") {
				continue
			}
			u := "https://" + strings.Trim(d, "/")
			if !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no domains returned")
	}
	return nil, lastErr
}

// MirrorPool manages the candidate mirrors and the session's blocked set.
type MirrorPool struct {
	candidates []string
	blocked    map[string]time.Time
	mu         sync.Mutex
	hc         *http.Client
}

// blockTTL is how long a bot-challenged mirror stays skipped. Long, because a mirror
// behind a WAF tends to stay there; short enough that a recovered mirror is retried.
const blockTTL = 6 * 30 * 24 * time.Hour

func NewMirrorPool(base []string, hc *http.Client) *MirrorPool {
	seen := make(map[string]bool)
	var list []string
	for _, b := range base {
		if b = normalizeBase(b); b != "" && !seen[b] {
			seen[b] = true
			list = append(list, b)
		}
	}
	// Dynamic domains are fetched lazily at Build (startup), merged ahead of seeds
	// so cached/config mirrors are preferred; seeds fill the rest.
	return &MirrorPool{
		candidates: list,
		blocked:    make(map[string]time.Time),
		hc:         hc,
	}
}

// Build merges the dynamic list into the pool and returns the pool ready to probe.
func (p *MirrorPool) Build() *MirrorPool {
	p.mergeDynamic()
	p.mergeSeeds()
	return p
}

func (p *MirrorPool) mergeDynamic() {
	domains, err := fetchDynamicDomains(p.hc)
	if err != nil {
		fmt.Printf("zlib: nao foi possivel buscar lista dinamica de mirrors: %v\n", err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range domains {
		if !hasString(p.candidates, d) {
			p.candidates = append(p.candidates, d)
		}
	}
}

func (p *MirrorPool) mergeSeeds() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range seedMirrors {
		if !hasString(p.candidates, s) {
			p.candidates = append(p.candidates, s)
		}
	}
}

func (p *MirrorPool) markBlocked(base string) {
	base = normalizeBase(base)
	if base == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blocked[base] = time.Now()
}

// MarkBlocked records a host as bot-challenged so discovery skips it for blockTTL.
func (p *MirrorPool) MarkBlocked(base string) {
	p.markBlocked(base)
}

// Candidates returns the non-blocked candidate bases currently in the pool.
func (p *MirrorPool) Candidates() []string {
	return p.candidatesSnapshot()
}

func (p *MirrorPool) isBlocked(base string) bool {
	base = normalizeBase(base)
	if base == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ts, ok := p.blocked[base]
	if !ok {
		return false
	}
	if time.Since(ts) >= blockTTL {
		delete(p.blocked, base)
		return false
	}
	return true
}

func (p *MirrorPool) candidatesSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.candidates))
	for _, c := range p.candidates {
		if !p.isBlockedLocked(c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		// Everything is blocked; fall back to seeds rather than discovery with nothing.
		for _, c := range p.candidates {
			out = append(out, c)
		}
	}
	return out
}

func (p *MirrorPool) isBlockedLocked(base string) bool {
	base = normalizeBase(base)
	ts, ok := p.blocked[base]
	if !ok {
		return false
	}
	if time.Since(ts) >= blockTTL {
		delete(p.blocked, base)
		return false
	}
	return true
}

// healthCheck probes a single mirror with GET /eapi/info/ok and reports latency.
func (p *MirrorPool) healthCheck(base string) (time.Duration, error) {
	base = normalizeBase(base)
	u := base + "/eapi/info/ok"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	// Health checks are quick probes; they must not inherit the long download
	// timeout of the session client.
	client := &http.Client{Timeout: 12 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return elapsed, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return elapsed, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if looksLikeBotChallenge(body) {
		return elapsed, &BotChallengeError{Base: base, URL: u}
	}
	var ok struct {
		Success int `json:"success"`
	}
	if err := json.Unmarshal(body, &ok); err != nil {
		return elapsed, fmt.Errorf("invalid JSON: %v", err)
	}
	if ok.Success != 1 {
		return elapsed, fmt.Errorf("invalid api response")
	}
	return elapsed, nil
}

// WorkingMirrors probes all non-blocked candidates (short timeout each, in parallel),
// returns the working ones ordered by declaration (config/dynamic before seeds), and marks
// bot-challenged hosts as blocked so they are skipped on later sweeps. Returns an error only
// if no mirror answered.
func (p *MirrorPool) WorkingMirrors() ([]string, error) {
	hosts := p.candidatesSnapshot()
	if len(hosts) == 0 {
		return nil, fmt.Errorf("nenhum mirror candidato configurado")
	}

	type result struct {
		base  string
		err   error
		order int
	}
	results := make(chan result, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(h string, order int) {
			defer wg.Done()
			_, err := p.healthCheck(h)
			if err != nil {
				if _, ok := err.(*BotChallengeError); ok {
					p.markBlocked(h)
				}
				results <- result{base: h, err: err, order: order}
				return
			}
			results <- result{base: h, err: nil, order: order}
		}(h, i)
	}
	wg.Wait()
	close(results)

	type okResult struct {
		base  string
		order int
	}
	var working []okResult
	for r := range results {
		if r.err == nil {
			working = append(working, okResult{base: r.base, order: r.order})
		}
	}
	if len(working) == 0 {
		return nil, fmt.Errorf("nenhum mirror acessivel (todos responderam bot check ou falharam)")
	}
	// Sort by declaration order (config/dynamic before seeds).
	sort.SliceStable(working, func(i, j int) bool { return working[i].order < working[j].order })
	out := make([]string, 0, len(working))
	for _, w := range working {
		out = append(out, w.base)
	}
	return out, nil
}

// Discover returns the first working mirror (see WorkingMirrors).
func (p *MirrorPool) Discover() (string, error) {
	working, err := p.WorkingMirrors()
	if err != nil {
		return "", err
	}
	return working[0], nil
}

func normalizeBase(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "https://" + s
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return strings.TrimRight(s, "/")
	}
	return strings.TrimRight(s, "/")
}

func hasString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
