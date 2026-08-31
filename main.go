package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"tome-wishlist-downloader/internal/config"
	"tome-wishlist-downloader/internal/schedule"
	"tome-wishlist-downloader/internal/tome"
	"tome-wishlist-downloader/internal/zlib"
)

// refreshEvery ciclos antes de recarregar o cache do scheduler a partir do SQLite.
const scheduleRefreshEvery = 10

// wishOutcome descreve o resultado do processamento de uma wish dentro de um ciclo.
type wishOutcome int

const (
	wishFailed    wishOutcome = iota // erro transitorio; pode tentar de novo
	wishSuccess                      // livro baixado e enviado (fulfilled)
	wishRefused                      // sem match aceitavel; marcada dismissed
	wishScheduled                    // sem link apos tentativas; agendada p/ monitoramento
	wishSkipped                      // agendada e ainda nao venceu; ignorada neste ciclo
	wishQuota                        // quota diaria esgotada; pausa todo o processamento
)

func main() {
	cfg, err := config.Load(".env")
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	tm := tome.New(cfg.TomeURL, cfg.TomeAPIToken)
	fmt.Printf("tome-wishlist-downloader iniciado - Tome=%s, intervalo=%v\n", cfg.TomeURL, cfg.PollInterval)

	sched, err := schedule.New(cfg.ScheduleDB)
	if err != nil {
		log.Fatalf("schedule: %v", err)
	}
	defer sched.Close()
	fmt.Printf("schedule: monitoramento em %s (base=%v, teto=%v)\n", cfg.ScheduleDB, cfg.ScheduleBaseInterval, cfg.ScheduleMaxInterval)

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

	for cycle := 1; ; cycle++ {
		runOnce(cfg, tm, zc, pool, bt, sched, cycle)
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

func runOnce(cfg *config.Config, tm *tome.Client, zc *zlib.Client, pool *zlib.MirrorPool, bt *tome.BookType, sched *schedule.Store, cycle int) {
	// Recarrega o cache do scheduler periodicamente para espelhar o SQLite.
	if cycle%scheduleRefreshEvery == 1 {
		sched.Refresh()
	}

	now := time.Now()

	// Pausa por quota: se a conta atingiu o limite diario, nao checa nada (nem a wishlist)
	// ate o contador resetar. O prazo fica persistido no banco e sobrevive a reload/update.
	if limit, due, ok := sched.Quota(); ok && due.After(now) {
		fmt.Printf("[%s] quota esgotada (%d downloads/dia) - processamento pausado, retomando apos %s\n",
			now.Format("2006-01-02 15:04:05"), limit, due.Format("2006-01-02 15:04"))
		return
	}
	// Pausa venceu (24h): limpa e segue o fluxo normal.
	if _, _, ok := sched.Quota(); ok {
		sched.ClearQuota()
	}

	wishes, err := tm.ListWishlist(cfg.WishlistStatus)
	if err != nil {
		log.Printf("erro listando wishlist: %v", err)
		return
	}

	fmt.Printf("[%s] wishlist (%d itens)\n", now.Format("2006-01-02 15:04:05"), len(wishes))

	// Reconcilia o monitoramento: apaga do banco/cache qualquer wish que deixou de existir
	// (sumiu) ou mudou de status no Tome (fulfilled/dismissed). Tambem identifica quais
	// wishlist items existem de fato para comparar com o scheduler.
	reconcileSchedule(cfg, tm, sched)

	if zc == nil {
		return
	}

	remaining, err := zc.RemainingDownloads()
	if err != nil {
		log.Printf("zlib: falha ao ler quota: %v", err)
	} else {
		fmt.Printf("zlib: downloads restantes hoje: %d\n", remaining)
		if remaining <= 0 {
			// Quota esgotada confirmada pelo profile: pausa o processamento por um ciclo
			// de reset diario e nao processa mais nada neste ciclo.
			until := now.Add(cfg.QuotaPause)
			if serr := sched.SetQuota(cfg.ZlibQuotaLimit, until); serr != nil {
				log.Printf("zlib: falha ao registrar pausa de quota: %v", serr)
			}
			fmt.Printf("quota diaria de downloads esgotada - pausando ate %s\n", until.Format("2006-01-02 15:04"))
			return
		}
	}

	processed := 0
	for _, w := range wishes {
		// Pula wish que esta agendada e ainda nao venceu (monitoramento em curso).
		if due, ok := sched.ScheduledUntil(w.ID); ok && due.After(now) {
			fmt.Printf("  #%d em monitoramento - proxima tentativa %s\n", w.ID, due.Format("2006-01-02 15:04"))
			continue
		}

		if processed >= cfg.MaxDownloadsPerRun {
			break
		}

		out := processWish(cfg, tm, zc, bt, sched, w, now)
		if _, isBot := out.err.(*zlib.BotChallengeError); isBot && pool != nil {
			log.Printf("wish #%d: mirror %s bloqueado por bot check, trocando de mirror...", w.ID, zc.Base())
			pool.MarkBlocked(zc.Base())
			if aerr := authenticate(pool, zc); aerr == nil {
				out = processWish(cfg, tm, zc, bt, sched, w, now)
			} else {
				log.Printf("wish #%d: nao foi possivel trocar de mirror: %v", w.ID, aerr)
			}
		}

		switch out.kind {
		case wishSuccess, wishRefused, wishScheduled:
			// A wish foi resolvida (enviada/recusada) ou posta em monitoramento;
			// conta como processada neste ciclo.
			processed++
			if out.kind != wishScheduled {
				// Se terminou (fulfilled/recusada), nao deve continuar agendada.
				if err := sched.Delete(w.ID); err != nil {
					log.Printf("wish #%d: falha ao limpar agendamento: %v", w.ID, err)
				}
			}
		case wishSkipped:
			// Nao conta como processada.
		case wishQuota:
			// Quota esgotada durante o processamento: pausa tudo ate o reset diario e
			// para de processar o restante deste ciclo.
			until := time.Now().Add(cfg.QuotaPause)
			if serr := sched.SetQuota(cfg.ZlibQuotaLimit, until); serr != nil {
				log.Printf("wish #%d: falha ao registrar pausa de quota: %v", w.ID, serr)
			}
			fmt.Printf("wish #%d: quota esgotada - pausando todo o processamento ate %s\n", w.ID, until.Format("2006-01-02 15:04"))
			return
		case wishFailed:
			if out.err != nil {
				log.Printf("wish #%d (%s): %v", w.ID, w.Title, out.err)
			}
			// Nao conta como processada: o erro e transitorio, tenta de novo no ciclo.
		}
	}
}

// reconcileSchedule apaga agendamentos cujas wishes nao existem mais ou mudaram de status
// no Tome. Lista os tres estados conhecidos e remove do scheduler os ids ausentes.
func reconcileSchedule(cfg *config.Config, tm *tome.Client, sched *schedule.Store) {
	// Sem agendamentos, nao ha o que reconciliar (evita 3 chamadas HTTP por ciclo).
	if len(sched.All()) == 0 {
		return
	}
	existing := make(map[int]bool)
	for _, status := range []string{"open", "fulfilled", "dismissed"} {
		list, err := tm.ListWishlist(status)
		if err != nil {
			// Falha ao listar um status; nao corre risco de apagar por engano, segue.
			continue
		}
		for _, w := range list {
			existing[w.ID] = true
		}
	}

	for _, pw := range sched.All() {
		if !existing[pw.WishID] {
			log.Printf("schedule: wish #%d deixou de existir no Tome; removendo do monitoramento", pw.WishID)
			if err := sched.Delete(pw.WishID); err != nil {
				log.Printf("schedule: falha ao remover wish #%d: %v", pw.WishID, err)
			}
		}
	}
}

// wishResult e o retorno de processWish para o runOnce tomar decisoes de contagem/reconciliacao.
type wishResult struct {
	kind wishOutcome
	err  error
}

// processWish executa o fluxo de uma wish: ate 3 tentativas variadas de busca/match procurando
// um livro com link baixavel. Se nenhum tiver link apos as 3 tentativas, agenda o monitoramento.
// A flexibilizacao acontece apenas na QUERY/estrategia (autor, idioma), nunca no criterio de
// match: um livro nao relacionado ao pedido continua sendo rejeitado.
func processWish(cfg *config.Config, tm *tome.Client, zc *zlib.Client, bt *tome.BookType, sched *schedule.Store, w tome.Wish, now time.Time) wishResult {
	author := strVal(w.Author)
	mainLang := ""
	if len(cfg.ZlibLanguages) > 0 {
		mainLang = strings.ToLower(strings.TrimSpace(cfg.ZlibLanguages[0]))
	}

	// As 3 tentativas variadas. Cada uma busca de um jeito (query/idioma) e, para os
	// candidatos aceitos, testa o link de download em cascata ate achar um baixavel.
	var lastErr string
	for attempt := 1; attempt <= 3; attempt++ {
		books, err := searchAttempt(zc, w, author, mainLang, attempt)
		if err != nil {
			// Erro transitorio (rede/bot/quota): mantem a wish para o proximo ciclo.
			return wishResult{kind: wishFailed, err: err}
		}

		// Ordem BRUTA que o zlib devolve (antes de qualquer ranking/filtro) - p/ calibracao.
		log.Printf("wish #%d:   [bruto %d/3] %d resultado(s) na ordem da API:", w.ID, attempt, len(books))
		for i, b := range books {
			log.Printf("wish #%d:   bruto[%d] id=[%d] \"%s\" (%s) ano=%v autor=%q idioma=%q | library=%s/book/%d/%s",
				w.ID, i, b.ID, b.DisplayName(), b.Extension, yearVal(b.Year), b.Author, b.Language, zc.Base(), b.ID, b.Hash)
		}

		ranked := zlib.RankCandidates(books, w.Title, author, cfg.FormatPreference, mainLang)
		ranked = filterByStrategy(ranked, w.Title, author, mainLang, attempt)

		log.Printf("wish #%d: tentativa %d/3 - %d candidato(s) aceitos (de %d retornados pelo zlib)", w.ID, attempt, len(ranked), len(books))
		for i, b := range ranked {
			log.Printf("wish #%d:   rank[%d] id=[%d] \"%s\" (%s) ano=%v autor=%q idioma=%q | library=%s/book/%d/%s",
				w.ID, i, b.ID, b.DisplayName(), b.Extension, yearVal(b.Year), b.Author, b.Language, zc.Base(), b.ID, b.Hash)
		}
		if len(ranked) == 0 {
			continue
		}

		// Testa os candidatos em cascata ate encontrar um com link disponivel.
		n := cfg.ScheduleCascadeN
		if n > len(ranked) {
			n = len(ranked)
		}
		log.Printf("wish #%d:   testando ateh %d candidato(s) em cascata", w.ID, n)
		for i := 0; i < n; i++ {
			b := ranked[i]
			log.Printf("wish #%d:   candidato [%d] (rank %d/%d) \"%s\" (%s) - checando link...", w.ID, b.ID, i+1, n, b.DisplayName(), b.Extension)
			link, err := zc.GetDownloadLink(b.ID, b.Hash)
			if err != nil {
				if errors.Is(err, zlib.ErrNoDownload) {
					log.Printf("wish #%d:   candidato sem link de download, tentando proximo...", w.ID)
					continue
				}
				if errors.Is(err, zlib.ErrQuota) {
					return wishResult{kind: wishQuota, err: err}
				}
				// Erro transitorio no link (rede/bot).
				return wishResult{kind: wishFailed, err: err}
			}
			// Tem link: baixa e envia. O resultado e terminal para esta wish (sucesso, ou
			// erro transitorio que mantem a wish para o proximo ciclo).
			log.Printf("wish #%d: match escolhido: \"%s\" (formato=%s, tamanho=%s, idioma=%s)", w.ID, b.DisplayName(), b.Extension, b.Size, b.Language)
			return downloadAndUpload(cfg, tm, zc, bt, w, b, link)
		}
	}

	// Chegou aqui sem sucesso: nenhum candidato com link (ou todos agendados). Agenda o
	// monitoramento em vez de reprocessar infinitamente o mesmo livro.
	if pw, ok := sched.Get(w.ID); ok {
		// Ja agendada e venceu: re-agenda com backoff maior.
		next, err := sched.Requeue(w.ID, cfg.ScheduleBaseInterval, cfg.ScheduleMaxInterval, cfg.ScheduleJitter, lastErr)
		if err != nil {
			log.Printf("wish #%d: falha ao re-agendar monitoramento: %v", w.ID, err)
			return wishResult{kind: wishFailed, err: err}
		}
		attempts := pw.Attempts + 1
		fmt.Printf("wish #%d: ainda sem link apos %d tentativas; re-agendada para %s (attempt %d)\n",
			w.ID, attempts, next.Format("2006-01-02 15:04"), attempts)
		return wishResult{kind: wishScheduled}
	}

	next := schedule.NextBackoff(now, 1, cfg.ScheduleBaseInterval, cfg.ScheduleMaxInterval, cfg.ScheduleJitter)
	if err := sched.Upsert(w.ID, w.Title, author, next, lastErr, 1); err != nil {
		log.Printf("wish #%d: falha ao agendar monitoramento: %v", w.ID, err)
		return wishResult{kind: wishFailed, err: err}
	}
	fmt.Printf("wish #%d: sem link apos 3 tentativas; em monitoramento - proxima tentativa %s (24h)\n",
		w.ID, next.Format("2006-01-02 15:04"))
	return wishResult{kind: wishScheduled}
}

// searchAttempt executa a busca correspondente a tentativa informada, variando query/idioma.
func searchAttempt(zc *zlib.Client, w tome.Wish, author string, mainLang string, attempt int) ([]zlib.Book, error) {
	switch attempt {
	case 1:
		// Original: titulo + autor, idiomas do .env.
		q := strings.TrimSpace(w.Title + " " + author)
		log.Printf("wish #%d: busca %d/3 \"%s\" (idiomas do .env)", w.ID, attempt, q)
		return zc.Search(q)
	case 2:
		// Busca SO PELO TITULO + idioma principal: espelha a busca manual que costuma
		// achar a edicao correta mesmo quando o autor-combinado a esconde. O filtro por
		// estrategia (filterByStrategy) ainda exige autor correspondente + titulo forte.
		q := strings.TrimSpace(w.Title)
		log.Printf("wish #%d: busca %d/3 apenas p/ titulo \"%s\" (idioma principal=%s)", w.ID, attempt, q, mainLang)
		return zc.SearchLanguages(q, mainLangList(mainLang))
	case 3:
		// Foca titulo original + autor, ignorando idioma (pode haver versao pt com
		// idioma incorreto no registro).
		q := strings.TrimSpace(w.Title + " " + author)
		log.Printf("wish #%d: busca %d/3 \"%s\" (todos os idiomas)", w.ID, attempt, q)
		return zc.SearchLanguages(q, nil)
	}
	return nil, fmt.Errorf("tentativa invalida")
}

// filterByStrategy aplica o criterio de aceite moderado adicional de cada tentativa sobre a
// lista ja filtrada pelo piso de match do RankCandidates. A flexibilizacao nunca aceita um
// item claramente errado: so ajusta quao forte deve ser a relacao titulo/autor.
func filterByStrategy(ranked []zlib.Book, wishTitle, wishAuthor, mainLang string, attempt int) []zlib.Book {
	if len(ranked) == 0 {
		return ranked
	}
	var out []zlib.Book
	for _, b := range ranked {
		switch attempt {
		case 1:
			// Estrito: aceita o que o RankCandidates ja aceitou.
			out = append(out, b)
		case 2:
			// Busca foi so pelo titulo, entao exige reforco: autor corresponde E titulo
			// com relacao minima (>=0.4). Evita livro de outro autor que compartilha
			// apenas um titulo generico.
			if zlib.AuthorMatches(b.Author, wishAuthor) && zlib.TitleSimilarity(b.DisplayName(), wishTitle) >= 0.4 {
				out = append(out, b)
			}
		case 3:
			// Autor + titulo forte; ignora o idioma (o idioma pode estar incorreto).
			if zlib.AuthorMatches(b.Author, wishAuthor) && zlib.TitleSimilarity(b.DisplayName(), wishTitle) >= 0.5 {
				out = append(out, b)
			}
		}
	}
	return out
}

func mainLangList(mainLang string) []string {
	if mainLang == "" {
		return nil
	}
	return []string{mainLang}
}

// downloadAndUpload baixa o livro escolhido (link ja obtido) e envia ao Tome. O retorno e
// sempre terminal para a wish: sucesso (livro cumprido) ou erro transitorio/falha de upload
// que mantem a wish para o proximo ciclo.
func downloadAndUpload(cfg *config.Config, tm *tome.Client, zc *zlib.Client, bt *tome.BookType, w tome.Wish, b zlib.Book, link string) wishResult {
	dest := filepath.Join(cfg.DownloadDir, fileName(&b))
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		if err := zc.Download(link, dest); err != nil {
			log.Printf("wish #%d: livro NAO baixado - erro ao baixar \"%s\" para %s: %v", w.ID, b.DisplayName(), dest, err)
			return wishResult{kind: wishFailed, err: err}
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
			return wishResult{kind: wishSuccess}
		}
		if attempt < cfg.UploadMaxRetries {
			log.Printf("wish #%d: upload falhou (tentativa %d/%d): %v; nova tentativa em %v", w.ID, attempt, cfg.UploadMaxRetries, err, cfg.UploadRetryInterval)
			time.Sleep(cfg.UploadRetryInterval)
		} else {
			log.Printf("wish #%d: upload falhou apos %d tentativas, arquivo mantido p/ proximo ciclo: %v", w.ID, cfg.UploadMaxRetries, err)
		}
	}
	// Falha no upload: mantem para o proximo ciclo (nao pune como sem link).
	return wishResult{kind: wishFailed, err: fmt.Errorf("upload falhou apos %d tentativas", cfg.UploadMaxRetries)}
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

func yearVal(y *int) any {
	if y == nil {
		return ""
	}
	return *y
}
