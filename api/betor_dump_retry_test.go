package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/module195/torrent-indexer/cache"
	meilisearch "github.com/module195/torrent-indexer/search"
)

// A sync that fails (Meilisearch still starting) is retried after a short
// wait instead of the next interval (8 h by default).
func TestBetorDumpSyncRetriesAfterFailure(t *testing.T) {
	var fetches atomic.Int32
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		_, _ = io.WriteString(w, `[{"torrent_name":"Filme 2021","magnet_xt":"urn:btih:8AE99C15F568CBB58714F94008F397B78A218E5A"}]`)
	}))
	defer src.Close()
	meili := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer meili.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	(&BetorDumpSync{URL: src.URL, Interval: time.Hour, RetryDelays: []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}, Search: meilisearch.NewSearchIndexer(meili.URL, "", "torrents")}).Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for fetches.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	// the first run plus one per retry delay, then it waits for the interval
	if n := fetches.Load(); n != 3 {
		t.Fatalf("fetches = %d, want 3", n)
	}
}

// The lock of a failed sync is released, so a restart or a retry is not
// skipped for the lock's 4-hour TTL.
func TestBetorDumpFailedSyncReleasesLock(t *testing.T) {
	if !redisReachable() {
		t.Skip("no Redis")
	}
	r := cache.NewRedis()
	ctx := context.Background()
	_ = r.Del(ctx, betorDumpLockKey)
	t.Cleanup(func() { _ = r.Del(context.Background(), betorDumpLockKey) })

	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer src.Close()
	s := &BetorDumpSync{URL: src.URL, Interval: 8 * time.Hour, Search: meilisearch.NewSearchIndexer("http://127.0.0.1:1", "", "torrents"), Redis: r}
	if s.runLocked(ctx) {
		t.Fatal("runLocked reported success for a failed sync")
	}
	if ok, err := r.Exists(ctx, betorDumpLockKey); err != nil || ok {
		t.Fatalf("lock still held after a failed sync (exists=%v, err=%v)", ok, err)
	}
}
