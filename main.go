package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"tome-wishlist-downloader/internal/config"
	"tome-wishlist-downloader/internal/tome"
	"tome-wishlist-downloader/internal/zlib"
)

func main() {
	cfg, err := config.Load(".env")
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	tm := tome.New(cfg.TomeURL, cfg.TomeAPIToken)
	fmt.Printf("tome-wishlist-downloader iniciado - Tome=%s, intervalo=%v\n", cfg.TomeURL, cfg.PollInterval)

	var zc *zlib.Client
	var pool *zlib.MirrorPool
	var bt *tome.BookType
	if cfg.ZlibEmail == "" || cfg.ZlibPassword == "" {
		fmt.Println("ZLIB_EMAIL/ZLIB_PASSWORD vazios - modo so-listagem (sem zlib)")
	} else {
		zc = zlib.New("", cfg.ZlibEmail, cfg.ZlibPassword, cfg.ZlibLanguages, unionFormats(cfg), cfg.ZlibOrder)
		pool = zlib.NewMirrorPool(cfg.ZlibMirrors, zc.HTTP()).Build()

		if err := authenticate(pool, zc); err != nil {
			log.Fatalf("zlib: nao foi possivel autenticar em nenhum mirror: %v", err)
		}
		fmt.Printf("zlib: autenticado em %s\n", zc.Base())

		bt, err = tm.EnsureBookType(cfg.BookTypeSlug, cfg.BookTypeLabel)
		if err != nil {
			log.Fatalf("tipo de livro %q: %v (crie o tipo %q manualmente se o token nao for admin)", cfg.BookTypeSlug, err, cfg.BookTypeLabel)
		}
		fmt.Printf("tome: tipo de livro %q (id=%d)\n", bt.Slug, bt.ID)
	}

	for {
		runOnce(cfg, tm, zc, pool, bt)
		time.Sleep(cfg.PollInterval)
	}
}

// authenticate signs the client into a working mirror. It tries the configured base first,
// then runs discovery over the candidate pool (config/dynamic/seeds), swapping to whatever
// host answers without a bot check. Returns an error only if no mirror was reachable.
func authenticate(pool *zlib.MirrorPool, zc *zlib.Client) error {
	attempted := make(map[string]bool)

	// loginHost points the client at base and signs in. Returns true on success, and
	// reports via challenged whether the host answered a bot check.
	loginHost := func(base string) (ok, challenged bool) {
		if base == "" || attempted[base] {
			return false, false
		}
		attempted[base] = true
		zc.SetBase(base)
		err := zc.Login()
		if err == nil {
			return true, false
		}
		if _, isBot := err.(*zlib.BotChallengeError); isBot {
			pool.MarkBlocked(base)
			return false, true
		}
		return false, false
	}

	// Probe once to learn every mirror that answers /eapi/info/ok, then try to log in
	// across them in order. A login rejection on one host is not the end: the account may
	// simply not be registered there, so the next working mirror is tried.
	working, err := pool.WorkingMirrors()
	if err != nil {
		return fmt.Errorf("nenhum mirror acessivel: %v", err)
	}
	log.Printf("zlib: %d mirror(es) acessiveis encontrados", len(working))
	for _, base := range working {
		if ok, _ := loginHost(base); ok {
			return nil
		}
		// Stop once every distinct candidate has been attempted.
		if len(attempted) >= len(pool.Candidates()) {
			break
		}
	}
	return fmt.Errorf("autenticacao falhou em todos os mirrors testados")
}

func runOnce(cfg *config.Config, tm *tome.Client, zc *zlib.Client, pool *zlib.MirrorPool, bt *tome.BookType) {
	wishes, err := tm.ListWishlist(cfg.WishlistStatus)
	if err != nil {
		log.Printf("erro listando wishlist: %v", err)
		return
	}

	now := time.Now().Format("2006-01-02 15:04:05")
	fmt.Printf("[%s] wishlist (%d itens)\n", now, len(wishes))
	if len(wishes) == 0 {
		fmt.Println("  (vazia)")
		return
	}
	for _, w := range wishes {
		fmt.Printf("  #%d [%s] %s (autor: %s | serie: %s)\n", w.ID, w.Status, w.Title, strVal(w.Author), strVal(w.Series))
	}

	if zc == nil {
		return
	}

	remaining, err := zc.RemainingDownloads()
	if err != nil {
		log.Printf("zlib: falha ao ler quota: %v", err)
	} else {
		fmt.Printf("zlib: downloads restantes hoje: %d\n", remaining)
		if remaining <= 0 {
			fmt.Println("quota diaria de downloads esgotada - pulando ciclo")
			return
		}
	}

	processed := 0
	for _, w := range wishes {
		if processed >= cfg.MaxDownloadsPerRun {
			break
		}
		// A bot-challenged mirror is swapped for a fresh one mid-cycle: mark it
		// blocked, re-authenticate against another host, then retry this wish once.
		err := processWish(cfg, tm, zc, bt, w)
		if _, ok := err.(*zlib.BotChallengeError); ok && pool != nil {
			log.Printf("wish #%d: mirror %s bloqueado por bot check, trocando de mirror...", w.ID, zc.Base())
			pool.MarkBlocked(zc.Base())
			if aerr := authenticate(pool, zc); aerr == nil {
				err = processWish(cfg, tm, zc, bt, w)
			} else {
				log.Printf("wish #%d: nao foi possivel trocar de mirror: %v", w.ID, aerr)
			}
		}
		if err != nil {
			log.Printf("wish #%d (%s): %v", w.ID, w.Title, err)
			continue
		}
		processed++
	}
}

func processWish(cfg *config.Config, tm *tome.Client, zc *zlib.Client, bt *tome.BookType, w tome.Wish) error {
	query := w.Title
	if w.Author != nil && *w.Author != "" {
		query += " " + *w.Author
	}
	log.Printf("wish #%d: buscando \"%s\" no zlib...", w.ID, query)

	books, err := zc.Search(query)
	if err != nil {
		log.Printf("wish #%d: livro NAO baixado - erro ao buscar \"%s\" no zlib: %v", w.ID, query, err)
		return err
	}
	log.Printf("wish #%d: zlib retornou %d resultado(s) p/ \"%s\"", w.ID, len(books), query)
	for i, b := range books {
		if i >= 5 {
			break
		}
		log.Printf("wish #%d:   [%d] \"%s\" (%s, %s)", w.ID, b.ID, b.DisplayName(), b.Extension, b.Size)
	}

	best := zlib.SelectBestMatch(books, w.Title, strVal(w.Author), cfg.FormatPreference)
	if best == nil {
		// Nenhum resultado satisfaz o pedido. Em vez de ficar tentando em loop, recusamos
		// esta wish: marcamos como dismissed para retira-la do fluxo (nao baixamos nada).
		log.Printf("wish #%d: livro RECUSADO - nenhum resultado satisfaz o pedido \"%s\" (titulo=%q, autor=%q)", w.ID, query, w.Title, strVal(w.Author))
		if err := tm.DismissWish(w.ID); err != nil {
			log.Printf("wish #%d: livro recusado, mas falha ao marcar como dismissed no Tome: %v", w.ID, err)
			return nil
		}
		fmt.Printf("wish #%d: marcada como dismissed (recusada) no Tome - nao sera processada novamente\n", w.ID)
		return nil
	}
	log.Printf("wish #%d: match escolhido: \"%s\" (formato=%s, tamanho=%s, idioma=%s)", w.ID, best.DisplayName(), best.Extension, best.Size, best.Language)

	dest := filepath.Join(cfg.DownloadDir, fileName(best))
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		link, err := zc.GetDownloadLink(best.ID, best.Hash)
		if err != nil {
			log.Printf("wish #%d: livro NAO baixado - erro ao obter link de download de \"%s\" (%s): %v", w.ID, best.DisplayName(), best.Extension, err)
			return err
		}
		if err := zc.Download(link, dest); err != nil {
			log.Printf("wish #%d: livro NAO baixado - erro ao baixar \"%s\" para %s: %v", w.ID, best.DisplayName(), dest, err)
			return err
		}
		log.Printf("wish #%d: baixado para %s", w.ID, dest)
	} else if err == nil {
		log.Printf("wish #%d: arquivo ja existe em disco, reusando %s", w.ID, dest)
	}

	bookTypeID := &bt.ID
	for attempt := 1; attempt <= cfg.UploadMaxRetries; attempt++ {
		det, err := tm.UploadFile(dest, bookTypeID)
		if err == nil {
			if err := os.Remove(dest); err != nil {
				log.Printf("wish #%d: upload ok (book id=%d), mas falha ao apagar local: %v", w.ID, det.ID, err)
			} else {
				log.Printf("wish #%d: upload ok (book id=%d) e arquivo local removido", w.ID, det.ID)
			}
			markFulfilled(tm, cfg.WishlistStatus, w, det)
			return nil
		}
		if attempt < cfg.UploadMaxRetries {
			log.Printf("wish #%d: upload falhou (tentativa %d/%d): %v; nova tentativa em %v", w.ID, attempt, cfg.UploadMaxRetries, err, cfg.UploadRetryInterval)
			time.Sleep(cfg.UploadRetryInterval)
		} else {
			log.Printf("wish #%d: upload falhou apos %d tentativas, arquivo mantido p/ proximo ciclo: %v", w.ID, cfg.UploadMaxRetries, err)
		}
	}
	return nil
}

// markFulfilled fecha o ciclo da wishlist: para cada wish que o livro enviado casou, marca
// como fulfilled via endpoint admin. O upload ja ocorreu; uma falha aqui e apenas registrada
// e nao desfaz o livro enviado.
//
// Se o Tome nao associou o livro enviado a wish atual (matched_wish_ids sem ela), fazemos uma
// segunda chamada a wishlist para conferir: se a wish sumiu, foi fechada por outro caminho e
// terminou; se continua aberta, marcamos como dismissed (recusada) para nao gerar ciclo
// infinito reprocessando um livro que o Tome nao aceita.
func markFulfilled(tm *tome.Client, status string, wish tome.Wish, det *tome.BookDetail) {
	matched := false
	for _, wishID := range det.MatchedWishIDs {
		if wishID == wish.ID {
			matched = true
		}
		if err := tm.FulfillWish(wishID, det.ID); err != nil {
			log.Printf("wish #%d: upload ok (book id=%d), mas falha ao marcar fulfilled: %v", wishID, det.ID, err)
			continue
		}
		fmt.Printf("wish #%d: marcada como fulfilled (book id=%d) no Tome\n", wishID, det.ID)
	}

	if matched {
		return
	}

	// O upload aconteceu mas nao foi associado a esta wish. Re-checamos a wishlist com uma
	// segunda chamada antes de decidir (a wish pode ter sido fechada por outro caminho).
	gone := false
	if open, err := tm.ListWishlist(status); err == nil {
		found := false
		for _, ow := range open {
			if ow.ID == wish.ID {
				found = true
				break
			}
		}
		gone = !found
	} else {
		log.Printf("wish #%d: upload ok (book id=%d), mas falha ao reverificar wishlist: %v", wish.ID, det.ID, err)
		return
	}

	if gone {
		log.Printf("wish #%d: apos re-checagem a wish sumiu da wishlist (book id=%d) - considerada fechada", wish.ID, det.ID)
		return
	}

	log.Printf("wish #%d: ATENCAO - livro enviado (book id=%d) NAO foi associado a esta wish e ela segue aberta. "+
		"Marcando como dismissed (recusada) para evitar ciclo infinito.", wish.ID, det.ID)
	if err := tm.DismissWish(wish.ID); err != nil {
		log.Printf("wish #%d: falha ao marcar como dismissed: %v", wish.ID, err)
		return
	}
	fmt.Printf("wish #%d: marcada como dismissed (recusada) - nao sera processada novamente\n", wish.ID)
}

func fileName(b *zlib.Book) string {
	base := sanitize(b.DisplayName())
	if base == "" {
		base = "book"
	}
	ext := strings.TrimPrefix(strings.ToLower(b.Extension), ".")
	return base + "." + ext
}

func sanitize(s string) string {
	var b strings.Builder
	lastSpace := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastSpace = false
		} else if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

func unionFormats(cfg *config.Config) []string {
	seen := make(map[string]bool)
	var out []string
	for _, f := range cfg.FormatPreference {
		if k := strings.ToLower(f); !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func strVal(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
