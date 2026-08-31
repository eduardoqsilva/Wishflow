package zlib

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
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

// ErrNoDownload indica que um livro especifico nao possui link de download disponivel
// (resposta sem downloadLink ou success falso). Diferente de erros transitorios (rede,
// bot check, quota), este e permanente para aquele registro: tentar outro candidato ou
// agendar a wish, em vez de reprocessar o mesmo livro infinitamente.
var ErrNoDownload = fmt.Errorf("resposta sem downloadLink")

// ErrQuota indica que a conta atingiu o limite diario de downloads do zlib. E transitorio
// (reset diario) e afeta TODOS os livros naquele momento: nao vale tentar outros candidatos,
// e sim pausar o processamento ate o contador resetar.
var ErrQuota = fmt.Errorf("quota diaria de downloads esgotada")

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
	return c.SearchLanguages(query, c.languages)
}

// SearchLanguages busca books aplicando os idiomas informados na chamada (em vez dos fixos
// do cliente). Uma lista vazia omite o filtro de idiomas (todos). Usada para variar o
// metodo de busca entre as tentativas de uma wish.
func (c *Client) SearchLanguages(query string, languages []string) ([]Book, error) {
	if c.userID == "" || c.userKey == "" {
		return nil, fmt.Errorf("nao autenticado")
	}
	form := url.Values{}
	form.Set("message", query)
	form.Set("page", "1")
	form.Set("limit", "20")
	for i, lang := range languages {
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
	log.Printf("zlib: GetDownloadLink id=%d - API: %s | library: %s/book/%d/%s", id, endpoint, c.currentBase(), id, hash)
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
	ct := resp.Header.Get("Content-Type")
	log.Printf("zlib: GetDownloadLink id=%d - HTTP %d, content-type=%q", id, resp.StatusCode, ct)
	if resp.StatusCode != http.StatusOK {
		if looksLikeBotChallenge(body) {
			log.Printf("zlib: GetDownloadLink id=%d - BOT CHECK (resposta nao-JSON)", id)
			return "", &BotChallengeError{Base: c.currentBase()}
		}
		log.Printf("zlib: GetDownloadLink id=%d - corpo: %s", id, strings.TrimSpace(string(body)))
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if htmlResponse(ct) {
		log.Printf("zlib: GetDownloadLink id=%d - QUOTA/HTML: %s", id, strings.TrimSpace(string(body)))
		return "", fmt.Errorf("quota de downloads diaria esgotada (resposta HTML)")
	}
	var data struct {
		Success json.RawMessage `json:"success"`
		File    struct {
			DownloadLink  string `json:"downloadLink"`
			AllowDownload *bool  `json:"allowDownload"`
		} `json:"file"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		log.Printf("zlib: GetDownloadLink id=%d - JSON invalido: %s", id, strings.TrimSpace(string(body)))
		return "", err
	}
	if !loginSuccess(data.Success) {
		log.Printf("zlib: GetDownloadLink id=%d - success=falso, corpo: %s", id, strings.TrimSpace(string(body)))
		return "", fmt.Errorf("API erro ao obter link: %s", strings.TrimSpace(string(body)))
	}
	// A API devolve success=1 mas allowDownload=false + disallowDownloadMessage quando a
	// conta atingiu o limite diario. Isso nao e "livro sem link": afeta todos os livros e
	// so acaba no reset diario, entao tratamos como quota (transitorio).
	if data.File.AllowDownload != nil && !*data.File.AllowDownload {
		log.Printf("zlib: GetDownloadLink id=%d - QUOTA: allowDownload=false (limite diario atingido)", id)
		return "", ErrQuota
	}
	if data.File.DownloadLink == "" {
		log.Printf("zlib: GetDownloadLink id=%d - sucesso porem downloadLink VAZIO (sem download). corpo: %s", id, strings.TrimSpace(string(body)))
		return "", ErrNoDownload
	}
	log.Printf("zlib: GetDownloadLink id=%d - TEM downloadLink", id)
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

// SelectBestMatch devolve o melhor candidato aceito, ou nil. Mantido por compatibilidade.
func SelectBestMatch(books []Book, wishTitle, wishAuthor string, preference []string) *Book {
	r := RankCandidates(books, wishTitle, wishAuthor, preference, "")
	if len(r) == 0 {
		return nil
	}
	return &r[0]
}

// RankCandidates devolve, em ordem de qualidade, os candidatos que passam no piso de
// aceite (livros plausivelmente relacionados ao pedido). A flexibilizacao usada entre as
// tentativas de uma wish acontece na QUERY/estrategia de busca, nunca aqui: o piso de
// match permanece o mesmo para nao aceitar um item errado.
//
// mainLang (opcional, ex. "portuguese") e usado como criterio de desempate forte dentro
// de candidatos com titulo equivalente: edicoes no idioma principal sobem acima das de
// outros idiomas (epub ingles perde para azw3 portugues, por exemplo).
func RankCandidates(books []Book, wishTitle, wishAuthor string, preference []string, mainLang string) []Book {
	if len(books) == 0 {
		return nil
	}

	prefOrder := make(map[string]int, len(preference))
	for i, f := range preference {
		prefOrder[strings.ToLower(strings.TrimSpace(f))] = len(preference) - i
	}

	wishTitleN := normalize(wishTitle)
	wishAuthorN := normalize(wishAuthor)
	mainLangN := strings.ToLower(strings.TrimSpace(mainLang))

	type cand struct {
		book        Book
		titleSim    float64
		titleInc    bool
		authorSim   float64
		formatScore int
		langScore   int
	}

	scored := make([]cand, 0, len(books))
	for _, b := range books {
		titleSim, titleInc := similar(normalize(b.DisplayName()), wishTitleN)
		authorSim := 0.0
		if wishAuthorN != "" {
			authorSim, _ = similar(normalize(b.Author), wishAuthorN)
		}
		langScore := 0
		if mainLangN != "" && strings.ToLower(strings.TrimSpace(b.Language)) == mainLangN {
			langScore = 1
		}
		scored = append(scored, cand{
			book:        b,
			titleSim:    titleSim,
			titleInc:    titleInc,
			authorSim:   authorSim,
			formatScore: prefOrder[strings.ToLower(b.Extension)],
			langScore:   langScore,
		})
	}

	// Piso de aceite: descarta candidatos claramente nao relacionados ao pedido.
	candidates := scored[:0]
	for _, c := range scored {
		if acceptable(c.titleSim, c.titleInc, c.authorSim, wishTitleN == "", wishAuthorN == "") {
			candidates = append(candidates, c)
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.titleSim != b.titleSim {
			return a.titleSim > b.titleSim
		}
		// Dentro de titulos equivalentes, prioriza o idioma principal (.env primeiro
		// idioma). Assim a edicao no idioma desejado vence outras edicoes do mesmo titulo.
		if a.langScore != b.langScore {
			return a.langScore > b.langScore
		}
		// Em seguida o formato preferido (.env ZLIB_FORMAT_PREFERENCE, ex. epub > azw3).
		if a.formatScore != b.formatScore {
			return a.formatScore > b.formatScore
		}
		if a.authorSim != b.authorSim {
			return a.authorSim > b.authorSim
		}
		return a.book.DisplayName() < b.book.DisplayName()
	})

	out := make([]Book, len(candidates))
	for i, c := range candidates {
		out[i] = c.book
	}
	return out
}

// acceptable decide se um candidato com as similaridades dadas ao pedido pode ser um match
// plausivel. noWishTitle/noWishAuthor indicam quando o pedido nao possui aquele campo.
func acceptable(titleSim float64, titleInc bool, authorSim float64, noWishTitle, noWishAuthor bool) bool {
	if noWishTitle {
		// Sem titulo para comparar, exige autor razoavel quando houver.
		return noWishAuthor || titleSim > 0 || authorSim >= 0.5
	}
	if titleInc || titleSim >= 0.6 {
		// Titulo fortemente relacionado (exato, contido ou quase igual).
		return true
	}
	if titleSim >= 0.35 && (noWishAuthor || authorSim >= 0.5) {
		// Titulo parcial (pequenos erros de escrita) reforcado pelo autor.
		return true
	}
	return false
}

// similar calcula uma semelhanca [0,1] tolerante a pequenos erros de escrita usando
// bigramas de caracteres. O segundo retorno indica se um dos textos esta contido no outro.
func similar(a, b string) (float64, bool) {
	if a == "" && b == "" {
		return 1.0, true
	}
	if a == "" || b == "" {
		return 0.0, false
	}
	contained := strings.Contains(a, b) || strings.Contains(b, a)
	if a == b {
		return 1.0, true
	}
	return jaccard(bigrams(a), bigrams(b)), contained
}

func bigrams(s string) map[string]bool {
	m := make(map[string]bool)
	r := []rune(s)
	for i := 0; i+1 < len(r); i++ {
		m[string(r[i:i+2])] = true
	}
	return m
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
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

// TitleSimilarity expoe a semelhanca de titulo [0,1] tolerante a pequenos erros.
func TitleSimilarity(a, b string) float64 {
	sim, _ := similar(normalize(a), normalize(b))
	return sim
}

// AuthorMatches indica se o autor do candidato corresponde razoavelmente ao autor da wish.
func AuthorMatches(bookAuthor, wishAuthor string) bool {
	if wishAuthor == "" {
		return true
	}
	sim, _ := similar(normalize(bookAuthor), normalize(wishAuthor))
	return sim >= 0.5 || sim > 0 && (strings.Contains(normalize(wishAuthor), normalize(bookAuthor)) || strings.Contains(normalize(bookAuthor), normalize(wishAuthor)))
}
