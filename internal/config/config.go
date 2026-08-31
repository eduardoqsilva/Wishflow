package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	TomeURL             string
	TomeAPIToken        string
	PollInterval        time.Duration
	WishlistStatus      string
	BookTypeSlug        string
	BookTypeLabel       string
	UploadRetryInterval time.Duration
	UploadMaxRetries    int

	ZlibBaseURL        string
	ZlibMirrors        []string
	ZlibEmail          string
	ZlibPassword       string
	ZlibLanguages      []string
	ZlibOrder          string
	FormatPreference   []string
	AbsoluteFormats    []string
	DownloadDir        string
	MaxDownloadsPerRun int
}

func Load(path string) (*Config, error) {
	loadEnv(path)

	cfg := &Config{
		TomeURL:            strings.TrimRight(getEnv("TOME_URL", ""), "/"),
		TomeAPIToken:       getEnv("TOME_API_TOKEN", ""),
		WishlistStatus:     getEnv("TOME_WISHLIST_STATUS", "open"),
		BookTypeSlug:       getEnv("TOME_BOOK_TYPE_SLUG", "wishlist"),
		BookTypeLabel:      getEnv("TOME_BOOK_TYPE_LABEL", "Wishlist"),
		ZlibBaseURL:        strings.TrimRight(getEnv("ZLIB_BASE_URL", ""), "/"),
		ZlibMirrors:        append(parseList(getEnv("ZLIB_BASE_URL", "")), parseList(getEnv("ZLIB_MIRRORS", ""))...),
		ZlibEmail:          getEnv("ZLIB_EMAIL", ""),
		ZlibPassword:       getEnv("ZLIB_PASSWORD", ""),
		ZlibLanguages:      parseList(getEnv("ZLIB_LANGUAGES", "")),
		ZlibOrder:          getEnv("ZLIB_ORDER", "bestmatch"),
		FormatPreference:   parseList(getEnv("ZLIB_FORMAT_PREFERENCE", "epub,mobi,azw3,pdf")),
		AbsoluteFormats:    parseList(getEnv("ZLIB_ABSOLUTE_FORMATS", "cbz,cbr")),
		DownloadDir:        getEnv("ZLIB_DOWNLOAD_DIR", "./downloads"),
		MaxDownloadsPerRun: atoiDefault(getEnv("ZLIB_MAX_DOWNLOADS_PER_RUN", "1"), 1),
	}

	cfg.PollInterval = seconds(getEnv("TOME_POLL_INTERVAL", "60"))
	cfg.UploadRetryInterval = seconds(getEnv("TOME_UPLOAD_RETRY_INTERVAL", "60"))
	cfg.UploadMaxRetries = atoiDefault(getEnv("TOME_UPLOAD_MAX_RETRIES", "3"), 3)

	if cfg.TomeURL == "" || cfg.TomeAPIToken == "" {
		return nil, fmt.Errorf("TOME_URL e TOME_API_TOKEN sao obrigatorios (defina no .env)")
	}
	if cfg.UploadMaxRetries < 1 {
		cfg.UploadMaxRetries = 3
	}
	if cfg.MaxDownloadsPerRun < 0 {
		cfg.MaxDownloadsPerRun = 1
	}

	return cfg, nil
}

func loadEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.Trim(strings.TrimSpace(line[idx+1:]), `"'`)
		if _, ok := os.LookupEnv(key); !ok {
			os.Setenv(key, value)
		}
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func seconds(s string) time.Duration {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 60 * time.Second
	}
	return time.Duration(n) * time.Second
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return def
	}
	return n
}
