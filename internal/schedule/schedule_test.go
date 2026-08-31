package schedule

import (
	"testing"
	"time"
)

func TestStoreInMemory(t *testing.T) {
	s, err := NewWithOptions(Options{DBPath: ":memory:"})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	defer s.Close()

	now := time.Now()

	if err := s.Upsert(10, "Cosmos", "Carl Sagan", now.Add(24*time.Hour), "sem link", 1); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if !s.Exists(10) {
		t.Fatalf("esperado Exists(10)=true")
	}
	if _, ok := s.Get(10); !ok {
		t.Fatalf("esperado Get(10) ok")
	}
	if d := s.Due(now); len(d) != 0 {
		t.Fatalf("nada deve estar vencido agora, got %d", len(d))
	}

	// Vence depois de 24h.
	if d := s.Due(now.Add(25 * time.Hour)); len(d) != 1 {
		t.Fatalf("esperado 1 vencido, got %d", len(d))
	}

	// Requeue soma 1 tentativa e avanca o prazo.
	pw, _ := s.Get(10)
	if pw.Attempts != 1 {
		t.Fatalf("attempts esperado 1, got %d", pw.Attempts)
	}
	next, err := s.Requeue(10, 24*time.Hour, 192*time.Hour, 0, "sem link")
	if err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	if !next.After(now.Add(24 * time.Hour)) {
		t.Fatalf("Requeue deveria avançar mais de 24h: %v", next)
	}
	pw2, _ := s.Get(10)
	if pw2.Attempts != 2 {
		t.Fatalf("attempts esperado 2, got %d", pw2.Attempts)
	}

	// Delete remove do banco e do cache.
	if err := s.Delete(10); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Exists(10) {
		t.Fatalf("esperado Exists(10)=false apos delete")
	}
	if d := s.Due(now.Add(200 * time.Hour)); len(d) != 0 {
		t.Fatalf("apos delete nao deve haver vencidos, got %d", len(d))
	}
}

func TestNextBackoffCapAtMax(t *testing.T) {
	base := 24 * time.Hour
	max := 192 * time.Hour
	now := time.Now()

	if d := NextBackoff(now, 1, base, max, 0).Sub(now); d != 24*time.Hour {
		t.Fatalf("attempt 1: esperado 24h, got %v", d)
	}
	if d := NextBackoff(now, 2, base, max, 0).Sub(now); d != 48*time.Hour {
		t.Fatalf("attempt 2: esperado 48h, got %v", d)
	}
	if d := NextBackoff(now, 3, base, max, 0).Sub(now); d != 96*time.Hour {
		t.Fatalf("attempt 3: esperado 96h, got %v", d)
	}
	// A partir de 4 ja estoura 192h (8 dias) -> cap.
	if d := NextBackoff(now, 4, base, max, 0).Sub(now); d != 192*time.Hour {
		t.Fatalf("attempt 4: esperado cap 192h, got %v", d)
	}
	if d := NextBackoff(now, 10, base, max, 0).Sub(now); d != 192*time.Hour {
		t.Fatalf("attempt 10: esperado cap 192h, got %v", d)
	}
}
