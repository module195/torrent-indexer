package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	meilisearch "github.com/module195/torrent-indexer/search"
)

// One guindex search sends up to 3 queries. A site that fails all of them in
// that one search used to take 3 failures (unhealthy for 5 minutes); now the
// queries of one /indexers/multi count a single failure per indexer.
func Test_multiCountsOneFailurePerIndexer(t *testing.T) {
	s := NewIndexerStats()
	s.started = time.Time{}
	h := NewMultiHandler(map[string]http.HandlerFunc{
		"bludv_ru": s.Track("bludv_ru", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}),
	})
	const target = "/indexers/multi?q=tt1234567&q=matrix&q=matrix+1999&deadline_ms=2000&indexers=bludv_ru"
	runMulti(t, h, target)
	if st := statusOf(t, s, "bludv_ru"); !st.Healthy || st.ConsecutiveFailures != 1 {
		t.Fatalf("after one search: %+v", st)
	}
	// three bad searches in a row still take it out
	runMulti(t, h, target)
	runMulti(t, h, target)
	if st := statusOf(t, s, "bludv_ru"); st.Healthy || st.ConsecutiveFailures != unhealthyAfter {
		t.Fatalf("after three searches: %+v", st)
	}
	// direct calls (no multi) still count one failure each
	direct := s.Track("starck-filmes", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	for range 2 {
		direct(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/indexers/starck-filmes?q=x", nil))
	}
	if st := statusOf(t, s, "starck-filmes"); st.ConsecutiveFailures != 2 {
		t.Fatalf("direct calls: %+v", st)
	}
}

// Late runs hold every slot of a site for up to 2 minutes; a call whose
// answer is already in the response cache must not wait behind them.
func Test_multiCachedCallSkipsLimiter(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	route := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "hit" {
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "HIT")
		_, _ = w.Write([]byte(`{"results":[],"count":0}`))
	}
	h := NewMultiHandler(map[string]http.HandlerFunc{"rede_torrent": route})
	h.CacheProbe = func(_ context.Context, _ string, q url.Values) bool { return q.Get("q") == "hit" }

	_, p := runMulti(t, h, "/indexers/multi?q=a&q=b&q=c&deadline_ms=500&indexers=rede_torrent")
	for _, c := range p.Calls {
		if c.State != "timeout" {
			t.Fatalf("slow call: %+v", c)
		}
	}
	_, p = runMulti(t, h, "/indexers/multi?q=hit&deadline_ms=1000&indexers=rede_torrent")
	if len(p.Calls) != 1 || p.Calls[0].State != "ok" || p.Calls[0].Cache != "HIT" {
		t.Fatalf("cached call behind busy slots: %+v", p.Calls)
	}
}

// A hanging Meilisearch made every /sources?v=2 wait for its own health
// probe (up to 5 s); guindex reads /sources with a 1.5 s timeout.
func Test_sourcesServesStaleMeiliHealthWithoutWaiting(t *testing.T) {
	var probes atomic.Int32
	var slow atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		if slow.Load() {
			time.Sleep(2 * time.Second)
		}
	}))
	defer srv.Close()
	h := NewSourcesHandler(NewIndexerStats(), meilisearch.NewSearchIndexer(srv.URL, "", "torrents"), Features{Multi: true})

	// first read waits for the first probe
	if !h.isMeiliHealthy() {
		t.Fatal("first probe: want healthy")
	}
	// stale value: served at once while one shared probe refreshes it
	slow.Store(true)
	h.meiliMu.Lock()
	h.meiliCheckedAt = time.Now().Add(-2 * meiliHealthTTL)
	h.meiliMu.Unlock()
	start := time.Now()
	for range 3 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sources?v=2", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("/sources waited for the probe: %v", d)
	}
	time.Sleep(50 * time.Millisecond)
	if n := probes.Load(); n != 2 {
		t.Fatalf("probes = %d, want 2 (one shared refresh)", n)
	}
}
