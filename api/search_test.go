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
	Limit      int      `json:"limit"`
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
	return runSearchParams(t, h, url.Values{"q": {q}})
}

func runSearchParams(t *testing.T, h *MeilisearchHandler, params url.Values) searchPayload {
	t.Helper()
	rec := httptest.NewRecorder()
	h.SearchTorrentHandler(rec, httptest.NewRequest(http.MethodGet, "/search?"+params.Encode(), nil))
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

func Test_searchHandlerAppliesFilters(t *testing.T) {
	h, _ := fakeMeili(t, []schema.IndexedTorrent{
		{Title: "Breaking Bad 1ª Temporada", InfoHash: "a1", Year: "2008", IMDB: "https://www.imdb.com/title/tt0903747/"},
		{Title: "El Camino A Breaking Bad Movie", InfoHash: "b2", Year: "2019", IMDB: "https://www.imdb.com/title/tt9243946/"},
	})

	cases := []struct {
		params url.Values
		want   []string
	}{
		{url.Values{"q": {"breaking bad"}, "year": {"2019"}}, []string{"El Camino A Breaking Bad Movie"}},
		{url.Values{"q": {"breaking bad"}, "imdb": {"tt0903747"}}, []string{"Breaking Bad 1ª Temporada"}},
		{url.Values{"q": {"breaking bad"}, "season": {"1"}}, []string{"Breaking Bad 1ª Temporada"}},
		// the IMDb branch is filtered too
		{url.Values{"q": {"tt0903747"}, "year": {"2019"}}, []string{}},
	}
	for _, c := range cases {
		p := runSearchParams(t, h, c.params)
		if titles := titlesOf(p.Results); !slices.Equal(titles, c.want) || p.Count != len(c.want) {
			t.Errorf("%s: got %v (count %d), want %v", c.params.Encode(), titles, p.Count, c.want)
		}
	}
}

func Test_searchHandlerSorting(t *testing.T) {
	h, _ := fakeMeili(t, []schema.IndexedTorrent{
		{Title: "Breaking Bad 1ª Temporada", InfoHash: "a1", SeedCount: 1},
		{Title: "Breaking Bad 2ª Temporada", InfoHash: "b2", SeedCount: 9},
	})

	// same similarity: more seeds first
	if got := titlesOf(runSearch(t, h, "breaking bad").Results); !slices.Equal(got, []string{"Breaking Bad 2ª Temporada", "Breaking Bad 1ª Temporada"}) {
		t.Errorf("default order %v", got)
	}
	p := runSearchParams(t, h, url.Values{"q": {"breaking bad"}, "sortBy": {"seed_count"}, "sortDirection": {"asc"}})
	if got := titlesOf(p.Results); !slices.Equal(got, []string{"Breaking Bad 1ª Temporada", "Breaking Bad 2ª Temporada"}) {
		t.Errorf("sortBy=seed_count asc: %v", got)
	}
}

func Test_searchHandlerFilterResultsFalseWithTranslation(t *testing.T) {
	h, requests := fakeMeili(t, []schema.IndexedTorrent{
		{Title: "Homem-Aranha Sem Volta Para Casa", InfoHash: "a1"},
		{Title: "Zzz Qqq", InfoHash: "z9"},
	})

	p := runSearchParams(t, h, url.Values{"q": {"spider man"}, "filter_results": {"false"}})

	if n := len(requests()); n != 2 {
		t.Fatalf("%d searches, want the EN one and the PT translation", n)
	}
	if titles := titlesOf(p.Results); !slices.Equal(titles, []string{"Homem-Aranha Sem Volta Para Casa", "Zzz Qqq"}) {
		t.Errorf("got %v, want everything, best first", titles)
	}
}

func Test_searchHandlerPunctuatedEpisodeQuery(t *testing.T) {
	h, _ := fakeMeili(t, []schema.IndexedTorrent{
		{Title: "Star Wars Andor S02E03 1080p WEB-DL", OriginalTitle: "Andor: Uma História Star Wars 2ª Temporada", InfoHash: "a1"},
	})

	// the colon used to stay in the query the similarity was computed with,
	// and the release scored 0.33, under the 0.45 an episode query needs
	if p := runSearch(t, h, "Star Wars: Andor S02E03"); p.Count != 1 {
		t.Errorf("got %v, want the S02E03 release", titlesOf(p.Results))
	}
}

func Test_searchHandlerIMDbFetchesAllThenLimits(t *testing.T) {
	imdb := "https://www.imdb.com/title/tt0903747/"
	h, requests := fakeMeili(t, []schema.IndexedTorrent{
		{Title: "Breaking Bad S01E01", InfoHash: "a1", IMDB: imdb, SeedCount: 1},
		{Title: "Breaking Bad S05E16", InfoHash: "b2", IMDB: imdb, SeedCount: 50},
	})

	p := runSearchParams(t, h, url.Values{"q": {"tt0903747"}, "limit": {"1"}})

	if reqs := requests(); len(reqs) != 1 || reqs[0].Limit != meilisearch.MaxSearchHits {
		t.Fatalf("searches %+v, want one asking for %d hits", reqs, meilisearch.MaxSearchHits)
	}
	// Meilisearch returns IMDb hits oldest first: the limit applies after sorting
	if titles := titlesOf(p.Results); !slices.Equal(titles, []string{"Breaking Bad S05E16"}) {
		t.Errorf("got %v, want the best seeded release", titles)
	}
}

func Test_translateToPTWholeWords(t *testing.T) {
	for in, want := range map[string]string{
		"The Elephant Man":         "The Elephant Man",
		"Iron Mantis":              "Iron Mantis",
		"Jokers Wild":              "Jokers Wild",
		"spider man":               "homem aranha",
		"Spider Man Brand New Day": "homem aranha um novo dia",
		"the ant man 2015":         "the homem formiga 2015",
	} {
		if got := translateToPT(in); got != want {
			t.Errorf("translateToPT(%q) = %q, want %q", in, got, want)
		}
	}
}

func Test_SendToSearchIndexerUpserts(t *testing.T) {
	type call struct {
		method string
		docs   []map[string]any
	}
	calls := make(chan call, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var docs []map[string]any
		_ = json.NewDecoder(r.Body).Decode(&docs)
		calls <- call{r.Method, docs}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	i := &Indexer{search: meilisearch.NewSearchIndexer(srv.URL, "", "torrents")}
	SendToSearchIndexer(i, nil, []schema.IndexedTorrent{
		{Title: "Sem IMDb", InfoHash: "a1", Similarity: 0.7},
		{Title: "Sem hash"},
	})

	c := <-calls
	if c.method != http.MethodPut {
		t.Errorf("method %s, want PUT (POST replaces the document and drops the dump's IMDb)", c.method)
	}
	if len(c.docs) != 1 || c.docs[0]["id"] != "a1" {
		t.Fatalf("docs %v, want only the one with a hash", c.docs)
	}
	// no counts from the tracker scrape: keep the stored ones
	for _, key := range []string{"imdb", "similarity", "seed_count", "leech_count"} {
		if _, ok := c.docs[0][key]; ok {
			t.Errorf("doc carries %q: %v", key, c.docs[0])
		}
	}
}

func titlesOf(torrents []schema.IndexedTorrent) []string {
	out := make([]string, 0, len(torrents))
	for _, it := range torrents {
		out = append(out, it.Title)
	}
	return out
}
