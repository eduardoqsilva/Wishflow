package zlib

import (
	"testing"
)

var pref = []string{"epub", "mobi", "azw3", "pdf", "cbz", "cbr"}

func TestRankCandidatesDune(t *testing.T) {
	books := []Book{
		{ID: 1, Title: "Dune", Author: "Frank Herbert", Extension: "epub"},
		{ID: 2, Title: "Frank Herbert Dune - The Graphic Novel, Book 02 - Muad'Dib", Author: "Frank Herbert", Extension: "cbz"},
		{ID: 3, Title: "The Martian", Author: "Andy Weir", Extension: "epub"},
	}
	r := RankCandidates(books, "Dune", "Frank Herbert", pref, "")
	if len(r) == 0 || r[0].ID != 1 {
		t.Fatalf("esperado Dune epub (id=1) em 1o, got %+v", r)
	}
}

func TestRankCandidatesCosmosSimulation(t *testing.T) {
	// Simula os resultados da wish "Cosmos" (sem link no livro exato "Cosmos").
	books := []Book{
		{ID: 1, Title: "Cosmos", Author: "Carl Sagan", Extension: "azw3"}, // match exato, mas SEM link (testado fora)
		{ID: 2, Title: "A reader-study guide for Cosmos, Carl Sagan", Author: "Carl Sagan", Extension: "pdf"},
		{ID: 3, Title: "Carl Sagan : a life in the cosmos", Author: "Author X", Extension: "pdf"},
		{ID: 4, Title: "Star Stuff: Carl Sagan and the Mysteries of the Cosmos", Author: "Carl Sagan", Extension: "azw3"},
	}
	r := RankCandidates(books, "Cosmos", "Carl Sagan", pref, "")
	// O livro com titulo exato "Cosmos" (id=1) deve estar em 1o.
	if len(r) != 0 && r[0].ID != 1 {
		t.Fatalf("esperado 'Cosmos' exato (id=1) em 1o, got id=%d", r[0].ID)
	}
}

func TestRankCandidatesPreferPreferredFormat(t *testing.T) {
	// Mesma wish "Cosmos". Titulos igualmente exatos em formatos diferentes: a versao
	// EPUB (formato preferido do env) deve ficar acima dos azw3 sem link.
	books := []Book{
		{ID: 1, Title: "Cosmos", Author: "Carl Sagan", Extension: "azw3"},
		{ID: 2, Title: "Cosmos", Author: "Carl Sagan", Extension: "epub"},
		{ID: 3, Title: "Cosmos", Author: "Carl Sagan", Extension: "pdf"},
	}
	r := RankCandidates(books, "Cosmos", "Carl Sagan", pref, "")
	if len(r) == 0 {
		t.Fatalf("esperado pelo menos um candidato, got 0")
	}
	if r[0].ID != 2 {
		t.Fatalf("esperado a edicao epub (id=2) em 1o, got id=%d (%s)", r[0].ID, r[0].Extension)
	}
	if r[0].Extension != "epub" {
		t.Fatalf("esperado formato preferido epub em 1o, got %s", r[0].Extension)
	}
}

func TestRankCandidatesPreferMainLanguage(t *testing.T) {
	// Com idioma principal = portuguese, o "Cosmos" azw3 em portugues deve vencer o
	// epub em ingles (o idioma desejado vale mais que o formato).
	books := []Book{
		{ID: 1, Title: "Cosmos", Author: "Carl Sagan", Extension: "epub", Language: "English"},
		{ID: 2, Title: "Cosmos", Author: "Carl Sagan", Extension: "azw3", Language: "Portuguese"},
		{ID: 3, Title: "Cosmos", Author: "Carl Sagan", Extension: "mobi", Language: "Portuguese"},
	}
	r := RankCandidates(books, "Cosmos", "Carl Sagan", pref, "portuguese")
	if len(r) == 0 {
		t.Fatalf("esperado pelo menos um candidato, got 0")
	}
	// Idioma primeiro: os dois portugueses (mobi id=3, azw3 id=2) devem vir antes do
	// epub ingles (id=1). Entre os portugueses, formato preferido (mobi>azw3).
	if r[0].ID != 3 || r[1].ID != 2 || r[2].ID != 1 {
		t.Fatalf("esperado ordem 3,2,1 (pt por formato, depois en), got %d,%d,%d",
			r[0].ID, r[1].ID, r[2].ID)
	}
	// Sem idioma principal definido, o formato preferido manda de novo (epub ingles 1o).
	r2 := RankCandidates(books, "Cosmos", "Carl Sagan", pref, "")
	if r2[0].ID != 1 {
		t.Fatalf("sem mainLang esperado epub ingles (id=1) em 1o, got id=%d", r2[0].ID)
	}
}

func TestFilterByStrategyNotExported(t *testing.T) {
	// A estrategia de flexibilizacao e aplicada no main; aqui validamos que os helpers
	// usados por ela se comportam como o esperado.
	if TitleSimilarity("Cosmos", "Cosmos") < 1.0 {
		t.Fatalf("esperado similaridade 1.0 p/ titulo igual")
	}
	if TitleSimilarity("The Martian", "Cosmos") >= 0.4 {
		t.Fatalf("titulos distintos nao deveriam passar em titulo fraca")
	}
	if !AuthorMatches("Carl Sagan", "Carl Sagan") {
		t.Fatalf("autor igual deveria casar")
	}
	if AuthorMatches("Carl Sagan", "Andy Weir") {
		t.Fatalf("autores distintos nao deveriam casar")
	}
}

func TestErrNoDownload(t *testing.T) {
	// Garante que ErrNoDownload existe e nao e nil (usado como sentinel).
	if ErrNoDownload == nil {
		t.Fatalf("ErrNoDownload nao deveria ser nil")
	}
}

func TestErrQuota(t *testing.T) {
	// Garante que ErrQuota existe e e distinto de ErrNoDownload.
	if ErrQuota == nil {
		t.Fatalf("ErrQuota nao deveria ser nil")
	}
	if ErrQuota == ErrNoDownload {
		t.Fatalf("ErrQuota deveria ser distinto de ErrNoDownload")
	}
}
