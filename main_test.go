package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	handler "github.com/module195/torrent-indexer/api"
	"github.com/module195/torrent-indexer/schema"
	meilisearch "github.com/module195/torrent-indexer/search"
)

// An oversized q is refused before any handler runs; a normal one passes.
func Test_limitQueryMiddleware(t *testing.T) {
	long := strings.Repeat("a", maxQueryBytes+1)
	cases := []struct {
		path    string
		through bool
	}{
		{"/search?q=duna", true},
		{"/search?q=" + strings.Repeat("a", maxQueryBytes), true},
		{"/search?q=" + long, false},
		{"/indexers/rede_torrent?q=" + long, false},
		{"/indexers/multi?q=duna&q=" + long, false},
		{"/sources?v=2", true},
	}
	for _, c := range cases {
		reached := false
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
		rec := httptest.NewRecorder()
		limitQueryMiddleware(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if reached != c.through {
			t.Errorf("%.60s: reached handler = %v, want %v", c.path, reached, c.through)
		}
		if !c.through && rec.Code != http.StatusBadRequest {
			t.Errorf("%.60s: status = %d, want 400", c.path, rec.Code)
		}
	}
}

func Test_skipIMDBQueryMiddleware(t *testing.T) {
	cases := []struct {
		path    string
		through bool
	}{
		{"/search?q=tt11198330", true},
		{"/indexers/betor?q=tt11198330", true},
		{"/indexers/bludv_ru?q=tt11198330", true},
		{"/indexers/multi?q=tt11198330", true},
		{"/indexers/rede_torrent?q=duna", true},
		{"/indexers/rede_torrent?q=tt11198330", false},
	}
	for _, c := range cases {
		reached := false
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
		rec := httptest.NewRecorder()
		skipIMDBQueryMiddleware(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if reached != c.through {
			t.Errorf("%s: reached handler = %v, want %v", c.path, reached, c.through)
		}
		if !c.through && rec.Header().Get("X-Cache") != "BYPASS" {
			t.Errorf("%s: X-Cache = %q, want BYPASS", c.path, rec.Header().Get("X-Cache"))
		}
	}
}

// Test_searchIMDbThroughMiddleware sends an IMDb ID to /search with the
// middleware in front, as main wires it, and expects the Meilisearch hit back.
func Test_searchIMDbThroughMiddleware(t *testing.T) {
	meili := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"hits": []schema.IndexedTorrent{
			{Title: "A Casa do Dragão", InfoHash: "a1", IMDB: "https://www.imdb.com/title/tt11198330/"},
		}})
	}))
	defer meili.Close()
	search := handler.NewMeilisearchHandler(meilisearch.NewSearchIndexer(meili.URL, "", "torrents"))
	mux := http.NewServeMux()
	mux.HandleFunc("/search", search.SearchTorrentHandler)

	rec := httptest.NewRecorder()
	skipIMDBQueryMiddleware(mux).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/search?q=tt11198330", nil))

	var got struct {
		Count   int                     `json:"count"`
		Results []schema.IndexedTorrent `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON %q: %v", rec.Body.String(), err)
	}
	if got.Count != 1 || got.Results[0].Title != "A Casa do Dragão" {
		t.Errorf("got %+v, want the Meilisearch hit", got)
	}
}
