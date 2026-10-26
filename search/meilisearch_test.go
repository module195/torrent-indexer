package meilisearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
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

	strategies = nil
	if _, err := si.SearchTorrentByIMDB(context.Background(), "tt1190634", 10); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(strategies, []string{"all"}) {
		t.Errorf("IMDb search used strategies %v, want only [all]", strategies)
	}
}
