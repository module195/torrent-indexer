package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/module195/torrent-indexer/schema"
	meilisearch "github.com/module195/torrent-indexer/search"
)

func Test_normalizeQueryKeepsAccents(t *testing.T) {
	cases := map[string]string{
		"a casa do dragão":         "a casa do dragão",
		"ação+e+reação":            "ação e reação",
		"Homem-Aranha: Sem Volta!": "Homem-Aranha Sem Volta",
		"  O   Conto   da Aia  ":   "O Conto da Aia",
		"tt3581920":                "tt3581920",
	}
	for in, want := range cases {
		if got := normalizeQuery(in); got != want {
			t.Errorf("normalizeQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func Test_filterByIMDBID(t *testing.T) {
	in := []schema.IndexedTorrent{
		{Title: "exact", IMDB: "https://www.imdb.com/title/tt3581920/", Similarity: 0.1},
		{Title: "pt", IMDB: "https://www.imdb.com/pt/title/TT3581920/"},
		{Title: "longer", IMDB: "https://www.imdb.com/title/tt35819201/"},
		{Title: "other", IMDB: "https://www.imdb.com/title/tt0133093/"},
		{Title: "empty"},
	}
	got := filterByIMDBID(in, "tt3581920")
	if titles := titlesOf(got); !slices.Equal(titles, []string{"exact", "pt"}) {
		t.Fatalf("got %v", titles)
	}
	for _, it := range got {
		if it.Similarity != 1 {
			t.Errorf("%s: similarity %v, want 1", it.Title, it.Similarity)
		}
	}
}

func Test_mergeByBestSimilarity(t *testing.T) {
	en := []schema.IndexedTorrent{
		{Title: "low", InfoHash: "1111", Similarity: 0.2},
		{Title: "dup-en", InfoHash: "AAAA", Similarity: 0.5},
		{Title: "tie-few-seeds", InfoHash: "bbbb", Similarity: 0.6, SeedCount: 2},
		{Title: "no-hash", Similarity: 0.45},
	}
	pt := []schema.IndexedTorrent{
		{Title: "dup-pt", InfoHash: "aaaa", Similarity: 0.8},
		{Title: "tie-many-seeds", InfoHash: "cccc", Similarity: 0.6, SeedCount: 9},
		{Title: "no-hash", Similarity: 0.45},
	}
	want := []string{"dup-pt", "tie-many-seeds", "tie-few-seeds", "no-hash", "no-hash"}
	// the old map-based merge returned a different order on each call
	for range 20 {
		if got := titlesOf(mergeByBestSimilarity(en, pt, 0.4)); !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

type meiliRequest struct {
	Q          string   `json:"q"`
	Attributes []string `json:"attributesToSearchOn"`
}

// fakeMeili serves hits for every search and records the requests it got.
func fakeMeili(t *testing.T, hits []schema.IndexedTorrent) (*MeilisearchHandler, func() []meiliRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []meiliRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req meiliRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad search body: %v", err)
		}
		mu.Lock()
		reqs = append(reqs, req)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"hits": hits})
	}))
	t.Cleanup(srv.Close)
	return NewMeilisearchHandler(meilisearch.NewSearchIndexer(srv.URL, "", "torrents")), func() []meiliRequest {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(reqs)
	}
}

type searchPayload struct {
	Results []schema.IndexedTorrent `json:"results"`
	Count   int                     `json:"count"`
}

func runSearch(t *testing.T, h *MeilisearchHandler, q string) searchPayload {
	t.Helper()
	rec := httptest.NewRecorder()
	h.SearchTorrentHandler(rec, httptest.NewRequest(http.MethodGet, "/search?q="+url.QueryEscape(q), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var p searchPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("invalid JSON %q: %v", rec.Body.String(), err)
	}
	return p
}

func Test_searchHandlerIMDbID(t *testing.T) {
	h, requests := fakeMeili(t, []schema.IndexedTorrent{
		{Title: "A Casa do Dragão", InfoHash: "a1", IMDB: "https://www.imdb.com/title/tt11198330/"},
		{Title: "Outra", InfoHash: "b2", IMDB: "https://www.imdb.com/title/tt111983301/"},
	})

	p := runSearch(t, h, " TT11198330 ")

	reqs := requests()
	if len(reqs) != 1 {
		t.Fatalf("%d searches, want 1", len(reqs))
	}
	if reqs[0].Q != `"tt11198330"` || !slices.Equal(reqs[0].Attributes, []string{"imdb"}) {
		t.Errorf("search sent %+v, want the quoted ID on imdb only", reqs[0])
	}
	if p.Count != 1 || p.Results[0].Title != "A Casa do Dragão" || p.Results[0].Similarity != 1 {
		t.Errorf("got %+v", p)
	}
}

func Test_searchHandlerKeepsAccents(t *testing.T) {
	h, requests := fakeMeili(t, []schema.IndexedTorrent{
		{Title: "A Casa do Dragão 1ª Temporada", InfoHash: "a1"},
	})

	p := runSearch(t, h, "a casa do dragão")

	reqs := requests()
	if len(reqs) != 1 {
		t.Fatalf("%d searches, want 1", len(reqs))
	}
	if reqs[0].Q != "a casa do dragão" || !slices.Equal(reqs[0].Attributes, []string{"title", "original_title"}) {
		t.Errorf("search sent %+v", reqs[0])
	}
	if p.Count != 1 {
		t.Errorf("count %d, want 1", p.Count)
	}
}

func titlesOf(torrents []schema.IndexedTorrent) []string {
	out := make([]string, 0, len(torrents))
	for _, it := range torrents {
		out = append(out, it.Title)
	}
	return out
}
