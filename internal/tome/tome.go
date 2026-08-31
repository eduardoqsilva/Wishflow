package tome

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const userAgent = "tome-wishlist-downloader/0.1"

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Minute},
	}
}

type Wish struct {
	ID          int      `json:"id"`
	Kind        string   `json:"kind"`
	Status      string   `json:"status"`
	Title       string   `json:"title"`
	Author      *string  `json:"author"`
	Series      *string  `json:"series"`
	SeriesIndex *float64 `json:"series_index"`
	Note        *string  `json:"note"`
}

func (c *Client) ListWishlist(status string) ([]Wish, error) {
	endpoint := c.baseURL + "/api/wishlist"
	if status != "" {
		endpoint += "?status=" + status
	}
	var wishes []Wish
	if err := c.getJSON(endpoint, &wishes); err != nil {
		return nil, err
	}
	return wishes, nil
}

type BookType struct {
	ID        int    `json:"id"`
	Slug      string `json:"slug"`
	Label     string `json:"label"`
	Icon      string `json:"icon"`
	Color     string `json:"color"`
	SortOrder int    `json:"sort_order"`
	LibraryID *int   `json:"library_id"`
}

func (c *Client) ListBookTypes() ([]BookType, error) {
	var types []BookType
	if err := c.getJSON(c.baseURL+"/api/book-types", &types); err != nil {
		return nil, err
	}
	return types, nil
}

func (c *Client) EnsureBookType(slug, label string) (*BookType, error) {
	types, err := c.ListBookTypes()
	if err != nil {
		return nil, err
	}
	if slug != "" {
		for i := range types {
			if types[i].Slug == slug {
				return &types[i], nil
			}
		}
	}
	if label != "" {
		for i := range types {
			if strings.EqualFold(types[i].Label, label) {
				return &types[i], nil
			}
		}
	}
	return c.CreateBookType(label, slug)
}

func (c *Client) CreateBookType(label, slug string) (*BookType, error) {
	payload := map[string]string{"label": label, "slug": slug}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/book-types", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var bt BookType
	if err := json.Unmarshal(body, &bt); err != nil {
		return nil, err
	}
	return &bt, nil
}

type BookDetail struct {
	ID             int    `json:"id"`
	Title          string `json:"title"`
	BookTypeID     *int   `json:"book_type_id"`
	ContentHash    string `json:"content_hash"`
	MatchedWishIDs []int  `json:"matched_wish_ids"`
}

// FulfillWish marca uma wish como concluída via endpoint admin, vinculando o livro enviado.
// Usa a permissão de admin do token para fechar o ciclo após um upload bem-sucedido.
func (c *Client) FulfillWish(wishID, bookID int) error {
	payload := map[string]int{"book_id": bookID}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/api/admin/wishlist/%d/fulfill", c.baseURL, wishID)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *Client) UploadFile(path string, bookTypeID *int) (*BookDetail, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return nil, err
	}
	if bookTypeID != nil {
		if err := mw.WriteField("book_type_id", strconv.Itoa(*bookTypeID)); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/books/upload", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var det BookDetail
	if err := json.Unmarshal(raw, &det); err != nil {
		return nil, err
	}
	return &det, nil
}

func (c *Client) getJSON(endpoint string, out interface{}) error {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

func (c *Client) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", userAgent)
}
