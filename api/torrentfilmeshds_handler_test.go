package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/module195/torrent-indexer/cache"
	"github.com/module195/torrent-indexer/monitoring"
	"github.com/module195/torrent-indexer/requester"
	"github.com/module195/torrent-indexer/schema"
	meilisearch "github.com/module195/torrent-indexer/search"
)

// fakeMeili counts document upserts.
func upsertCountingMeili(t *testing.T) (*meilisearch.SearchIndexer, *atomic.Int32) {
	t.Helper()
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts.Add(1)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	return meilisearch.NewSearchIndexer(srv.URL, "", "torrents"), &puts
}

func waitCount(c *atomic.Int32, want int32) int32 {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && c.Load() < want {
		time.Sleep(10 * time.Millisecond)
	}
	return c.Load()
}

func TestSendToSearchIndexerSkippedForReplays(t *testing.T) {
	si, puts := upsertCountingMeili(t)
	i := &Indexer{search: si}
	torrents := []schema.IndexedTorrent{{Title: "x", InfoHash: "dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c", SeedCount: 3}}
	r := httptest.NewRequest(http.MethodGet, "/indexers/torrentfilmeshds?q=x", nil)

	SendToSearchIndexer(i, withoutSearchIndexing(r), torrents)
	time.Sleep(100 * time.Millisecond)
	if got := puts.Load(); got != 0 {
		t.Fatalf("replay sent %d upserts, want 0", got)
	}
	SendToSearchIndexer(i, r, torrents)
	if got := waitCount(puts, 1); got != 1 {
		t.Fatalf("fresh result sent %d upserts, want 1", got)
	}
}

func newTfhdsTestIndexer(t *testing.T, site *httptest.Server) *Indexer {
	t.Helper()
	old := torrentFilmesHDs.URL
	torrentFilmesHDs.URL = site.URL + "/"
	t.Cleanup(func() { torrentFilmesHDs.URL = old })
	r := cache.NewRedis()
	req := requester.NewRequester(nil, r, 2*time.Second)
	// the test site is an httptest server on 127.0.0.1
	req.AllowPrivateHosts()
	return NewIndexers(IndexersConfig{}, r, monitoring.NewMetrics(), req, nil, nil)
}

// A site failure answers with the JSON error body every indexer uses.
func TestTorrentFilmesHDsErrorIsJSON(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("<html><body>manutenção</body></html>"))
	}))
	defer site.Close()
	i := newTfhdsTestIndexer(t, site)

	w := httptest.NewRecorder()
	i.HandlerTorrentFilmesHDsIndexer(w, httptest.NewRequest(http.MethodGet, "/indexers/torrentfilmeshds?q=erro-"+time.Now().Format("150405.000"), nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !strings.Contains(body["error"], "503") {
		t.Fatalf("body = %q (%v)", w.Body.String(), err)
	}
}

// With Redis: the raw result cache answers a repeat with HIT, and nocache=1
// skips it. Skipped where no Redis runs (CI).
func TestTorrentFilmesHDsNoCache(t *testing.T) {
	if err := cache.NewRedis().SetWithExpiration(context.Background(), "test:ping", []byte("1"), time.Second); err != nil {
		t.Skip("no Redis:", err)
	}
	var hits atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("<html><body><p>nada encontrado</p></body></html>"))
	}))
	defer site.Close()
	i := newTfhdsTestIndexer(t, site)

	q := "nocache-" + time.Now().Format("150405.000000")
	get := func(extra string) string {
		w := httptest.NewRecorder()
		i.HandlerTorrentFilmesHDsIndexer(w, httptest.NewRequest(http.MethodGet, "/indexers/torrentfilmeshds?q="+q+extra, nil))
		return w.Header().Get("X-Cache")
	}
	if got := get(""); got != "MISS" {
		t.Fatalf("first = %s, want MISS", got)
	}
	if got := get(""); got != "HIT" {
		t.Fatalf("second = %s, want HIT", got)
	}
	if got := get("&nocache=1"); got != "MISS" {
		t.Fatalf("nocache = %s, want MISS", got)
	}
}
