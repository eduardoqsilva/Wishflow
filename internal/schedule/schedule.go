// Package schedule mantem um monitoramento de longo prazo das wishes cujos livros
// ainda nao estao disponiveis para download no Z-Library. Quando uma wish falha apos
// as tentativas variadas, ela e agendada no SQLite com um proximo instante de tentativa
// (backoff exponencial) e fica fora do fluxo normal ate vencer. Um cache em memoria
// evita consultar o banco a cada ciclo.
package schedule

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// PendingWish e um registro de uma wish agendada para monitoramento.
type PendingWish struct {
	WishID        int
	Title         string
	Author        string
	NextAttemptAt int64 // unix seconds
	Attempts      int   // quantas vezes ja foi re-agendada (0 = primeira)
	LastError     string
	UpdatedAt     int64
}

// Store encapsula o SQLite e um cache em memoria que espelha o banco.
type Store struct {
	mu sync.RWMutex
	db *sql.DB

	cache      map[int]PendingWish // wish_id -> registro
	ready      bool
	lastLoaded time.Time
}

// Options configura o comportamento de escalonamento do agendamento.
type Options struct {
	DBPath       string
	BaseInterval time.Duration // intervalo inicial (ex. 24h)
	MaxInterval  time.Duration // teto do backoff (ex. 8d)
	Jitter       time.Duration // fator aleatorio adicionado (ex. 2h)
}

func New(path string) (*Store, error) {
	return NewWithOptions(Options{DBPath: path})
}

func NewWithOptions(opt Options) (*Store, error) {
	if opt.DBPath == "" {
		return nil, errors.New("schedule: caminho do banco vazio")
	}
	db, err := sql.Open("sqlite", opt.DBPath)
	if err != nil {
		return nil, fmt.Errorf("schedule: abrir sqlite: %w", err)
	}
	// Essas pragmas tornam a conexao simples/pura (sem CGO) mais robusta.
	db.SetMaxOpenConns(1)

	s := &Store{
		db:    db,
		cache: make(map[int]PendingWish),
	}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	s.Refresh()
	return s, nil
}

func (s *Store) init() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS pending_wishes (
			wish_id        INTEGER PRIMARY KEY,
			title          TEXT NOT NULL DEFAULT '',
			author         TEXT NOT NULL DEFAULT '',
			next_attempt_at INTEGER NOT NULL,
			attempts       INTEGER NOT NULL DEFAULT 0,
			last_error     TEXT NOT NULL DEFAULT '',
			updated_at     INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_pending_next ON pending_wishes(next_attempt_at);
	`)
	return err
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Refresh recarrega o cache a partir do SQLite. Chamar periodicamente (ex. a cada N ciclos)
// e apos cada escrita para manter o cache em sincronia.
func (s *Store) Refresh() {
	rows, err := s.db.Query(
		`SELECT wish_id, title, author, next_attempt_at, attempts, last_error, updated_at
		 FROM pending_wishes`)
	if err != nil {
		return
	}
	defer rows.Close()

	next := make(map[int]PendingWish)
	for rows.Next() {
		var pw PendingWish
		if err := rows.Scan(&pw.WishID, &pw.Title, &pw.Author, &pw.NextAttemptAt, &pw.Attempts, &pw.LastError, &pw.UpdatedAt); err != nil {
			continue
		}
		next[pw.WishID] = pw
	}

	s.mu.Lock()
	s.cache = next
	s.ready = true
	s.lastLoaded = time.Now()
	s.mu.Unlock()
}

// All devolve uma copia de todos os registros agendados (do cache).
func (s *Store) All() []PendingWish {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PendingWish, 0, len(s.cache))
	for _, pw := range s.cache {
		out = append(out, pw)
	}
	return out
}

// Get retorna o registro agendado de uma wish (do cache) e se ele existe.
func (s *Store) Get(wishID int) (PendingWish, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pw, ok := s.cache[wishID]
	return pw, ok
}

// Exists informa se a wish esta agendada no cache.
func (s *Store) Exists(wishID int) bool {
	_, ok := s.Get(wishID)
	return ok
}

// ScheduledUntil retorna (dueAt, ok) — quando a wish agendada vence, ou false se nao
// estiver agendada no cache.
func (s *Store) ScheduledUntil(wishID int) (time.Time, bool) {
	pw, ok := s.Get(wishID)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(pw.NextAttemptAt, 0), true
}

// Due retorna as wishes agendadas cujo prazo ja venceu (prontas para nova tentativa).
func (s *Store) Due(now time.Time) []PendingWish {
	nowUnix := now.Unix()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []PendingWish
	for _, pw := range s.cache {
		if pw.NextAttemptAt <= nowUnix {
			out = append(out, pw)
		}
	}
	return out
}

// Upsert insere ou atualiza o agendamento de uma wish e espelha no cache.
func (s *Store) Upsert(wishID int, title, author string, nextAttempt time.Time, lastErr string, attempts int) error {
	attemptsNew := attempts
	updated := time.Now().Unix()
	_, err := s.db.Exec(
		`INSERT INTO pending_wishes (wish_id, title, author, next_attempt_at, attempts, last_error, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(wish_id) DO UPDATE SET
		   title=excluded.title, author=excluded.author, next_attempt_at=excluded.next_attempt_at,
		   attempts=excluded.attempts, last_error=excluded.last_error, updated_at=excluded.updated_at`,
		wishID, title, author, nextAttempt.Unix(), attemptsNew, lastErr, updated)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.cache[wishID] = PendingWish{
		WishID:        wishID,
		Title:         title,
		Author:        author,
		NextAttemptAt: nextAttempt.Unix(),
		Attempts:      attemptsNew,
		LastError:     lastErr,
		UpdatedAt:     updated,
	}
	s.mu.Unlock()
	return nil
}

// Requeue re-agenda uma wish existente somando o backoff exponencial ao instante atual.
// Retorna o novo instante agendado.
func (s *Store) Requeue(wishID int, base, max, jitter time.Duration, lastErr string) (time.Time, error) {
	pw, ok := s.Get(wishID)
	if !ok {
		return time.Time{}, fmt.Errorf("schedule: wish %d nao esta agendada", wishID)
	}
	next := NextBackoff(time.Now(), pw.Attempts+1, base, max, jitter)
	err := s.Upsert(pw.WishID, pw.Title, pw.Author, next, lastErr, pw.Attempts+1)
	return next, err
}

// Delete remove uma wish do banco e do cache (usado quando a wish sumiu, foi cumprida
// ou recusada no Tome).
func (s *Store) Delete(wishID int) error {
	if _, err := s.db.Exec(`DELETE FROM pending_wishes WHERE wish_id = ?`, wishID); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.cache, wishID)
	s.mu.Unlock()
	return nil
}

// NextBackoff calcula o proximo instante de tentativa com backoff exponencial:
// base * 2^(attempts), limitado a max, acrescido de um jitter aleatorio em [0, jitter).
func NextBackoff(now time.Time, attempts int, base, max, jitter time.Duration) time.Time {
	interval := base
	for i := 1; i < attempts; i++ {
		interval *= 2
		if interval >= max {
			interval = max
			break
		}
	}
	if jitter > 0 {
		d := time.Duration(int64(jitter)) // jitter e um teto aleatorio manual
		if d > 0 {
			interval += time.Duration(now.UnixNano()%int64(d)) % d
		}
	}
	return now.Add(interval)
}
