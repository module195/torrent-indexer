package meilisearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
)

func TestSearchTorrentRetriesWithLastStrategy(t *testing.T) {
	var strategies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		strategy, _ := body["matchingStrategy"].(string)
		strategies = append(strategies, strategy)
		hits := []map[string]string{}
		if strategy == "last" {
			hits = append(hits, map[string]string{"title": "The.Boys.S04.1080p"})
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"hits": hits})
	}))
	defer srv.Close()

	si := NewSearchIndexer(srv.URL, "", "torrents")
	got, err := si.SearchTorrent(context.Background(), "the boys 4 temporada", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !slices.Equal(strategies, []string{"all", "last"}) {
		t.Errorf("got %d hits with strategies %v, want 1 hit with [all last]", len(got), strategies)
	}

}

// The IMDb search filters on imdb_id newest first and merges in the old text
// search (documents indexed before imdb_id existed), without repeats.
func TestSearchTorrentByIMDBMergesNewestFirst(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		hits := []map[string]string{{"title": "S01E01 antigo", "info_hash": "aa"}, {"title": "S03E10 sem imdb_id", "info_hash": "cc"}}
		if body["filter"] != nil {
			hits = []map[string]string{{"title": "S12E05 novo", "info_hash": "bb"}, {"title": "S01E01 antigo", "info_hash": "AA"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"hits": hits})
	}))
	defer srv.Close()

	got, err := NewSearchIndexer(srv.URL, "", "torrents").SearchTorrentByIMDB(context.Background(), "tt1190634", 10)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, it := range got {
		titles = append(titles, it.Title)
	}
	if want := []string{"S12E05 novo", "S01E01 antigo", "S03E10 sem imdb_id"}; !slices.Equal(titles, want) {
		t.Errorf("got %v, want %v", titles, want)
	}
	var sawFilter, sawText bool
	for _, b := range bodies {
		if b["filter"] == `imdb_id = "tt1190634"` && fmt.Sprint(b["sort"]) == "[date_ts:desc]" {
			sawFilter = true
		}
		if b["q"] == `"tt1190634"` && b["matchingStrategy"] == "all" {
			sawText = true
		}
	}
	if !sawFilter || !sawText {
		t.Errorf("requests %v, want the imdb_id filter sorted by date_ts and the quoted text search", bodies)
	}
}

// Before the settings are applied the filter search fails (imdb_id not
// filterable); the text search alone still answers.
func TestSearchTorrentByIMDBWithoutSettings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["filter"] != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"invalid_search_filter"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"hits": []map[string]string{{"title": "x", "info_hash": "aa"}}})
	}))
	defer srv.Close()

	got, err := NewSearchIndexer(srv.URL, "", "torrents").SearchTorrentByIMDB(context.Background(), "tt1190634", 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestEnsureSettings(t *testing.T) {
	var method, path string
	var body map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	if err := NewSearchIndexer(srv.URL, "", "torrents").EnsureSettings(context.Background()); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPatch || path != "/indexes/torrents/settings" ||
		!slices.Equal(body["filterableAttributes"], []string{"imdb_id"}) ||
		!slices.Equal(body["sortableAttributes"], []string{"date_ts", "seed_count"}) {
		t.Errorf("sent %s %s %v", method, path, body)
	}
}
