package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/module195/torrent-indexer/schema"
)

func Test_AddSimilarityCheckIMDbQuery(t *testing.T) {
	torrents := []schema.IndexedTorrent{
		{Title: "Outro Filme 2023", IMDB: "https://www.imdb.com/title/tt1234567/"},
		{Title: "Oppenheimer 2023 1080p", IMDB: "https://www.imdb.com/title/tt15398776/"},
		{Title: "Oppenheimer 2023 720p", IMDB: "https://www.imdb.com/title/tt153987760/"},
	}
	r := httptest.NewRequest(http.MethodGet, "/indexers/betor?q=tt15398776", nil)
	out := AddSimilarityCheck(nil, r, torrents)
	if len(out) != 3 {
		t.Fatalf("IMDb queries are not filtered: got %d results", len(out))
	}
	if out[0].Title != "Oppenheimer 2023 1080p" || out[0].Similarity != 1 {
		t.Fatalf("exact IMDb match should score 1 and come first: %+v", out[0])
	}
	for _, it := range out[1:] {
		if it.Similarity != 0 {
			t.Errorf("other IDs should score 0: %+v", it)
		}
	}
}

func Test_indexerStatsHealthRetry(t *testing.T) {
	now := time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC)
	s := NewIndexerStats()
	s.now = func() time.Time { return now }
	status := func() IndexerStatus {
		for _, st := range s.Snapshot() {
			if st.Name == "rede_torrent" {
				return st
			}
		}
		t.Fatal("rede_torrent missing from snapshot")
		return IndexerStatus{}
	}

	for range unhealthyAfter {
		s.record("rede_torrent", http.StatusBadGateway, time.Second)
	}
	if st := status(); st.Healthy || st.LastFailureAt != "2026-10-24T20:00:00Z" {
		t.Fatalf("after failures: %+v", st)
	}
	now = now.Add(healthRetryAfter - time.Second)
	if status().Healthy {
		t.Fatal("still inside the retry interval")
	}
	now = now.Add(time.Second)
	if !status().Healthy {
		t.Fatal("should be offered for a retry after healthRetryAfter")
	}
	s.record("rede_torrent", http.StatusBadGateway, time.Second)
	if status().Healthy {
		t.Fatal("a failed retry keeps it out for another interval")
	}
	now = now.Add(healthRetryAfter)
	s.record("rede_torrent", http.StatusOK, time.Second)
	if st := status(); !st.Healthy || st.ConsecutiveFailures != 0 {
		t.Fatalf("a success resets the count: %+v", st)
	}
}

func Test_responseCacheTTL(t *testing.T) {
	const full = 10 * time.Minute
	cases := []struct {
		name string
		res  capturedResponse
		ttl  time.Duration
		ok   bool
	}{
		{"results", capturedResponse{status: 200, body: []byte(`{"results":[{}],"count":1,"indexed_count":4}`)}, full, true},
		{"all filtered", capturedResponse{status: 200, body: []byte(`{"results":[],"count":0,"indexed_count":4}`)}, emptyResultTTL, true},
		{"empty page", capturedResponse{status: 200, body: []byte(`{"results":[],"count":0}`)}, 0, false},
		{"error", capturedResponse{status: 500, body: []byte(`{"count":3}`)}, 0, false},
		{"not json", capturedResponse{status: 200, body: []byte(`<html>`)}, 0, false},
	}
	for _, c := range cases {
		ttl, ok := responseCacheTTL(c.res, full)
		if ttl != c.ttl || ok != c.ok {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", c.name, ttl, ok, c.ttl, c.ok)
		}
	}
	if ttl, _ := responseCacheTTL(capturedResponse{status: 200, body: []byte(`{"count":0,"indexed_count":1}`)}, time.Minute); ttl != time.Minute {
		t.Errorf("short TTL must not exceed the configured one: %v", ttl)
	}
}
