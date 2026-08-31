package zlib

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/96.0.4664.110 Safari/537.36"

type Client struct {
	baseMu     sync.RWMutex
	base       string
	email      string
	password   string
	http       *http.Client
	languages  []string
	extensions []string
	order      string

	userID  string
	userKey string
}

func New(base, email, password string, languages, extensions []string, order string) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		base:     strings.TrimRight(normalizeBase(base), "/"),
		email:    email,
		password: password,
		http: &http.Client{
			Timeout: 10 * time.Minute,
			Jar:     jar,
		},
		languages:  languages,
		extensions: extensions,
		order:      order,
	}
}

// Base returns the current base URL the client is pointed at.
func (c *Client) Base() string {
	c.baseMu.RLock()
	defer c.baseMu.RUnlock()
	return c.base
}

// SetBase swaps the base URL (used when discovery finds a working mirror).
func (c *Client) SetBase(base string) {
	base = strings.TrimRight(normalizeBase(base), "/")
	// A new mirror means the old session cookie is not valid there.
	c.http.Jar = nil
	if jar, err := cookiejar.New(nil); err == nil {
		c.http.Jar = jar
	}
	c.baseMu.Lock()
	c.base = base
	c.baseMu.Unlock()
	c.userID = ""
	c.userKey = ""
}

// HTTP returns the underlying client, useful for the mirror pool.
func (c *Client) HTTP() *http.Client {
	return c.http
}

// currentBase returns the current base URL under the read lock.
func (c *Client) currentBase() string {
	c.baseMu.RLock()
	defer c.baseMu.RUnlock()
	return c.base
}

type Book struct {
	ID          int    `json:"id"`
	Hash        string `json:"hash"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	Extension   string `json:"extension"`
	Size        string `json:"size"`
	Publisher   string `json:"publisher"`
	Language    string `json:"language"`
	Description string `json:"description"`
	Year        *int   `json:"year"`
}

func (b Book) DisplayName() string {
	if b.Title != "" {
		return b.Title
	}
	return b.Name
}

type loginResponse struct {
	Success json.RawMessage `json:"success"`
	User    struct {
		ID           int    `json:"id"`
		RemixUserKey string `json:"remix_userkey"`
	} `json:"user"`
}

// loginSuccess interprets the API's "success" field, which is sent as the number 1 on the
// mirror API (and could be true on other endpoints). Anything that is not an affirmative
// value counts as failure.
func loginSuccess(raw json.RawMessage) bool {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return n == 1
	}
	return false
}

func (c *Client) Login() error {
	form := url.Values{}
	form.Set("email", c.email)
	form.Set("password", c.password)

	status, body, err := c.postForm(c.currentBase()+"/eapi/user/login", form)
	if err != nil {
		return err
	}
	if looksLikeBotChallenge([]byte(body)) {
		return &BotChallengeError{Base: c.currentBase()}
	}
	var lr loginResponse
	if err := json.Unmarshal([]byte(body), &lr); err != nil {
		if status != http.StatusOK {
			return fmt.Errorf("HTTP %d: %s", status, strings.TrimSpace(body))
		}
		return err
	}
	if !loginSuccess(lr.Success) || lr.User.ID == 0 || lr.User.RemixUserKey == "" {
		return fmt.Errorf("credenciais rejeitadas ou resposta invalida: HTTP %d %s", status, strings.TrimSpace(body))
	}
	c.userID = fmt.Sprintf("%d", lr.User.ID)
	c.userKey = lr.User.RemixUserKey
	return nil
}

type searchResponse struct {
	Books      []Book `json:"books"`
	ExactMatch struct {
		Books []Book `json:"books"`
	} `json:"exactMatch"`
}

func (c *Client) Search(query string) ([]Book, error) {
	if c.userID == "" || c.userKey == "" {
		return nil, fmt.Errorf("nao autenticado")
	}
	form := url.Values{}
	form.Set("message", query)
	form.Set("page", "1")
	form.Set("limit", "20")
	for i, lang := range c.languages {
		form.Set(fmt.Sprintf("languages[%d]", i), lang)
	}
	for i, ext := range c.extensions {
		form.Set(fmt.Sprintf("extensions[%d]", i), strings.ToUpper(ext))
	}
	if c.order != "" {
		form.Set("order", c.order)
	}

	status, body, err := c.postForm(c.currentBase()+"/eapi/book/search", form)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		if looksLikeBotChallenge([]byte(body)) {
			return nil, &BotChallengeError{Base: c.currentBase()}
		}
		return nil, fmt.Errorf("HTTP %d: %s", status, strings.TrimSpace(body))
	}
	var sr searchResponse
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		return nil, err
	}
	if len(sr.Books) > 0 {
		return sr.Books, nil
	}
	return sr.ExactMatch.Books, nil
}

type profileResponse struct {
	Success json.RawMessage `json:"success"`
	User    struct {
		DownloadsToday int `json:"downloads_today"`
		Limit          int `json:"limit"`
	} `json:"user"`
}

func (c *Client) RemainingDownloads() (int, error) {
	req, err := http.NewRequest(http.MethodGet, c.currentBase()+"/eapi/user/profile", nil)
	if err != nil {
		return 0, err
	}
	c.setHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		if looksLikeBotChallenge(body) {
			return 0, &BotChallengeError{Base: c.currentBase()}
		}
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var pr profileResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return 0, err
	}
	if !loginSuccess(pr.Success) {
		return 0, fmt.Errorf("profile falhou: %s", strings.TrimSpace(string(body)))
	}
	if pr.User.Limit <= 0 {
		return int(^uint(0) >> 1), nil
	}
	rem := pr.User.Limit - pr.User.DownloadsToday
	if rem < 0 {
		rem = 0
	}
	return rem, nil
}

func (c *Client) GetDownloadLink(id int, hash string) (string, error) {
	endpoint := fmt.Sprintf("%s/eapi/book/%d/%s/file", c.currentBase(), id, hash)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	c.setHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		if looksLikeBotChallenge(body) {
			return "", &BotChallengeError{Base: c.currentBase()}
		}
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if htmlResponse(resp.Header.Get("Content-Type")) {
		return "", fmt.Errorf("quota de downloads diaria esgotada (resposta HTML)")
	}
	var data struct {
		Success json.RawMessage `json:"success"`
		File    struct {
			DownloadLink string `json:"downloadLink"`
		} `json:"file"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", err
	}
	if !loginSuccess(data.Success) {
		return "", fmt.Errorf("API erro ao obter link: %s", strings.TrimSpace(string(body)))
	}
	if data.File.DownloadLink == "" {
		return "", fmt.Errorf("resposta sem downloadLink")
	}
	return data.File.DownloadLink, nil
}

func (c *Client) Download(link, dest string) error {
	if c.userID == "" || c.userKey == "" {
		return fmt.Errorf("nao autenticado")
	}
	req, err := http.NewRequest(http.MethodGet, link, nil)
	if err != nil {
		return err
	}
	c.setHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if htmlResponse(resp.Header.Get("Content-Type")) {
		// It may be a quota/HMTL page or a bot challenge; scan the head to tell them apart.
		head := make([]byte, 4096)
		n, _ := io.ReadFull(resp.Body, head)
		if looksLikeBotChallenge(head[:n]) {
			return &BotChallengeError{Base: c.currentBase()}
		}
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("quota de downloads diaria esgotada (resposta HTML)")
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(tmp)
		return closeErr
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (c *Client) postForm(url string, form url.Values) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, "", err
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", c.currentBase()+"/")
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	if c.userID != "" && c.userKey != "" {
		req.Header.Set("Cookie", "remix_userid="+c.userID+"; remix_userkey="+c.userKey)
	}
}

func htmlResponse(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/html")
}

func SelectBestMatch(books []Book, wishTitle, wishAuthor string, preference, absolute []string) *Book {
	if len(books) == 0 {
		return nil
	}

	absSet := make(map[string]bool, len(absolute))
	for _, a := range absolute {
		absSet[strings.ToLower(strings.TrimSpace(a))] = true
	}
	prefOrder := make(map[string]int, len(preference))
	for i, f := range preference {
		prefOrder[strings.ToLower(strings.TrimSpace(f))] = len(preference) - i
	}

	candidates := books
	if usesAbsolute := hasAny(books, absSet); usesAbsolute {
		var filtered []Book
		for _, b := range books {
			if absSet[strings.ToLower(b.Extension)] {
				filtered = append(filtered, b)
			}
		}
		candidates = filtered
	}

	wishTitle = normalize(wishTitle)
	wishAuthor = normalize(wishAuthor)

	sort.SliceStable(candidates, func(i, j int) bool {
		ri := candidateRank(candidates[i], prefOrder, wishTitle, wishAuthor)
		rj := candidateRank(candidates[j], prefOrder, wishTitle, wishAuthor)
		if ri.format != rj.format {
			return ri.format > rj.format
		}
		if ri.score != rj.score {
			return ri.score > rj.score
		}
		return candidates[i].DisplayName() < candidates[j].DisplayName()
	})
	return &candidates[0]
}

type grade struct {
	format int
	score  float64
}

func candidateRank(b Book, prefOrder map[string]int, wishTitle, wishAuthor string) grade {
	format := prefOrder[strings.ToLower(b.Extension)]

	title := normalize(b.DisplayName())
	score := 0.0
	if wishTitle != "" && title != "" {
		if title == wishTitle {
			score += 2
		} else if strings.Contains(title, wishTitle) || strings.Contains(wishTitle, title) {
			score += 1
		} else {
			score += overlap(title, wishTitle)
		}
	}
	authorNorm := normalize(b.Author)
	if wishAuthor != "" && authorNorm != "" {
		if authorNorm == wishAuthor {
			score += 1
		} else if strings.Contains(authorNorm, wishAuthor) || strings.Contains(wishAuthor, authorNorm) {
			score += 0.5
		}
	}
	return grade{format: format, score: score}
}

func hasAny(books []Book, want map[string]bool) bool {
	for _, b := range books {
		if want[strings.ToLower(b.Extension)] {
			return true
		}
	}
	return false
}

func normalize(s string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

func overlap(a, b string) float64 {
	ta := strings.Fields(a)
	tb := strings.Fields(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	seen := make(map[string]bool, len(ta))
	for _, t := range ta {
		seen[t] = true
	}
	common := 0
	for _, t := range tb {
		if seen[t] {
			common++
		}
	}
	return float64(common) / float64(len(tb))
}
