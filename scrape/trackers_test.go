package goscrape

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/module195/torrent-indexer/cache"
)

func resetTrackersMem(t *testing.T) {
	t.Helper()
	reset := func() {
		trackersMemMu.Lock()
		trackersMem = nil
		trackersMemExpires = time.Time{}
		trackersMemMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func useTrackerMirrors(t *testing.T, urls ...string) {
	t.Helper()
	old := trackersListURLs
	trackersListURLs = urls
	t.Cleanup(func() { trackersListURLs = old })
}

// When every mirror fails, the static list is used for a while instead of
// downloading again on every scrape.
func TestAdditionalTrackersFailureIsCached(t *testing.T) {
	t.Setenv("REDIS_HOST", "127.0.0.1")
	resetTrackersMem(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	useTrackerMirrors(t, srv.URL+"/a", srv.URL+"/b", srv.URL+"/c")

	r := cache.NewRedis()
	for range 3 {
		list := getAdditionalTrackers(context.Background(), r)
		if len(list) != len(staticAdditionalTrackers) {
			t.Fatalf("want the static list, got %d trackers", len(list))
		}
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("mirrors were hit %d times, want 3 (one pass)", got)
	}
}

// A mirror that never answers is abandoned after trackersListFetchTimeout.
func TestTrackersListFetchIsBounded(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)

	start := time.Now()
	if _, err := fetchTrackersList(context.Background(), srv.URL); err == nil {
		t.Fatal("want a timeout error")
	}
	if d := time.Since(start); d > trackersListFetchTimeout+time.Second {
		t.Fatalf("fetch took %v, want about %v", d, trackersListFetchTimeout)
	}
}

func TestTrackersListFetchParsesList(t *testing.T) {
	t.Setenv("REDIS_HOST", "127.0.0.1")
	resetTrackersMem(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("udp://a.example:1337/announce\n\nudp://b.example:6969/announce\n"))
	}))
	defer srv.Close()
	useTrackerMirrors(t, srv.URL)

	list := getAdditionalTrackers(context.Background(), cache.NewRedis())
	if len(list) != 2 || list[0] != "udp://a.example:1337/announce" {
		t.Fatalf("unexpected list %v", list)
	}
}
