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
	s.started = now.Add(-startupGrace)
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
	if st := status(); st.Healthy || st.RetryAfterS == nil || *st.RetryAfterS != int64((2*healthRetryAfter).Seconds()) {
		t.Fatalf("a failed retry doubles the interval: %+v", st)
	}
	now = now.Add(healthRetryAfter)
	if status().Healthy {
		t.Fatal("still inside the doubled interval")
	}
	now = now.Add(healthRetryAfter)
	if st := status(); !st.Healthy || st.RetryAfterS != nil {
		t.Fatalf("should be offered again after 2x healthRetryAfter: %+v", st)
	}
	s.record("rede_torrent", http.StatusOK, time.Second)
	if st := status(); !st.Healthy || st.ConsecutiveFailures != 0 {
		t.Fatalf("a success resets the count: %+v", st)
	}
}

func Test_indexerStatsRetryBackoff(t *testing.T) {
	want := map[int]time.Duration{
		unhealthyAfter:     healthRetryAfter,
		unhealthyAfter + 1: 2 * healthRetryAfter,
		unhealthyAfter + 3: 8 * healthRetryAfter,
		unhealthyAfter + 4: healthRetryMax,
		1000:               healthRetryMax,
	}
	for n, d := range want {
		if got := retryAfter(n); got != d {
			t.Errorf("retryAfter(%d) = %v, want %v", n, got, d)
		}
	}
}

func Test_indexerStatsStartupGrace(t *testing.T) {
	now := time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC)
	s := NewIndexerStats()
	s.now = func() time.Time { return now }
	s.started = now
	healthy := func() bool {
		for _, st := range s.Snapshot() {
			if st.Name == "rede_torrent" {
				return st.Healthy
			}
		}
		t.Fatal("rede_torrent missing from snapshot")
		return false
	}

	for range unhealthyAfter {
		s.record("rede_torrent", http.StatusBadGateway, time.Second)
	}
	if !healthy() {
		t.Fatal("failures right after start should not mark it unhealthy")
	}
	now = now.Add(startupGrace)
	if healthy() {
		t.Fatal("after the grace period the failures count again")
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

func Test_responseCacheTTLPerIndexer(t *testing.T) {
	if got := ttlEnvName("torrent-dos-filmes"); got != "RESPONSE_CACHE_TTL_TORRENT_DOS_FILMES" {
		t.Errorf("ttlEnvName: %s", got)
	}
	rc := &ResponseCache{ttl: 5 * time.Minute}
	t.Setenv("RESPONSE_CACHE_TTL_BETOR", "30m")
	t.Setenv("RESPONSE_CACHE_TTL_BLUDV_RU", "0")
	t.Setenv("RESPONSE_CACHE_TTL_REDE_TORRENT", "nope")
	cases := map[string]time.Duration{
		"betor":            30 * time.Minute,
		"bludv_ru":         0,
		"rede_torrent":     5 * time.Minute, // invalid falls back to the global TTL
		"comando_torrents": 5 * time.Minute,
	}
	for label, want := range cases {
		if got := rc.ttlFor(label); got != want {
			t.Errorf("ttlFor(%s) = %v, want %v", label, got, want)
		}
	}

	rec := httptest.NewRecorder()
	rc.Wrap("bludv_ru", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})(rec, httptest.NewRequest(http.MethodGet, "/indexers/bludv_ru?q=a", nil))
	if rec.Header().Get("X-Cache") != "BYPASS" {
		t.Errorf("TTL 0 should bypass the cache: %q", rec.Header().Get("X-Cache"))
	}
}

func Test_wantsFresh(t *testing.T) {
	cases := []struct {
		url, cacheControl string
		fresh             bool
		query             string
	}{
		{"/indexers/x?q=a&nocache=1", "", true, "q=a"},
		{"/indexers/x?nocache=true&q=a", "", true, "q=a"},
		{"/indexers/x?q=a&nocache", "", true, "q=a"},
		{"/indexers/x?q=a&nocache=0", "", false, "q=a"},
		{"/indexers/x?q=a", "no-cache", true, "q=a"},
		{"/indexers/x?q=a", "", false, "q=a"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, c.url, nil)
		if c.cacheControl != "" {
			r.Header.Set("Cache-Control", c.cacheControl)
		}
		if got := wantsFresh(r); got != c.fresh || r.URL.RawQuery != c.query {
			t.Errorf("%s: fresh=%v query=%q, want %v %q", c.url, got, r.URL.RawQuery, c.fresh, c.query)
		}
	}
}
