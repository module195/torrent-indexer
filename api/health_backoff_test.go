package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func statusOf(t *testing.T, s *IndexerStats, name string) IndexerStatus {
	t.Helper()
	for _, st := range s.Snapshot() {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("%s missing from snapshot", name)
	return IndexerStatus{}
}

func retryAfterS(st IndexerStatus) int64 {
	if st.RetryAfterS == nil {
		return 0
	}
	return *st.RetryAfterS
}

// Calls that keep coming while the indexer is out (guindex reads /sources?v=2
// every 2 minutes) must not push it to the 1-hour cap; only a failed retry
// doubles the interval.
func TestIndexerStatsBackoffDoublesPerRetry(t *testing.T) {
	s := NewIndexerStats()
	clock := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return clock }
	s.started = time.Time{}
	fail := s.Track("starck-filmes", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	ok := s.Track("starck-filmes", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"results":[]}`)) })
	get := func(h http.HandlerFunc) {
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/indexers/starck-filmes?q=x", nil))
	}

	for range unhealthyAfter {
		get(fail)
	}
	if got := retryAfterS(statusOf(t, s, "starck-filmes")); got != 300 {
		t.Fatalf("retry_after_s after %d failures = %d, want 300", unhealthyAfter, got)
	}
	// two minutes of searches on a stale view: 6 searches x 3 queries
	for range 6 {
		clock = clock.Add(20 * time.Second)
		for range 3 {
			get(fail)
		}
	}
	st := statusOf(t, s, "starck-filmes")
	if st.Healthy || st.ConsecutiveFailures != unhealthyAfter || retryAfterS(st) != 180 {
		t.Fatalf("stale calls changed the backoff: %+v retry=%d", st, retryAfterS(st))
	}

	// the interval runs out and the retry fails: now it doubles
	clock = clock.Add(3 * time.Minute)
	if !statusOf(t, s, "starck-filmes").Healthy {
		t.Fatal("should be offered again once the interval ran out")
	}
	get(fail)
	if got := retryAfterS(statusOf(t, s, "starck-filmes")); got != 600 {
		t.Fatalf("retry_after_s after a failed retry = %d, want 600", got)
	}

	// a success brings it back at once
	get(ok)
	if st := statusOf(t, s, "starck-filmes"); !st.Healthy || st.ConsecutiveFailures != 0 {
		t.Fatalf("after success: %+v", st)
	}
}

// A run cut by its own time limit (an unsolvable Cloudflare challenge takes
// 4 FlareSolverr tries of 30 s, past the 2-minute cap) is a failure; a client
// that went away is not.
func TestIndexerStatsTrackDeadline(t *testing.T) {
	s := NewIndexerStats()
	s.started = time.Time{}
	hang := s.Track("bludv_ru", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		w.WriteHeader(http.StatusInternalServerError)
	})
	run := func(ctx context.Context) {
		hang(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/indexers/bludv_ru?q=x", nil).WithContext(ctx))
	}

	for range unhealthyAfter {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		run(ctx)
	}
	if st := statusOf(t, s, "bludv_ru"); !st.Healthy || st.ConsecutiveFailures != 0 {
		t.Fatalf("a client giving up counted as failure: %+v", st)
	}

	for range unhealthyAfter {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		run(ctx)
		cancel()
	}
	if st := statusOf(t, s, "bludv_ru"); st.Healthy || st.ConsecutiveFailures != unhealthyAfter {
		t.Fatalf("runs past their time limit should make it unhealthy: %+v", st)
	}
}

// Inside the response cache the run is detached from the client, so only its
// own time limit can end it early; a cached replay still isn't counted.
func TestIndexerStatsTrackUnderResponseCacheHit(t *testing.T) {
	s := NewIndexerStats()
	s.started = time.Time{}
	h := s.Track("betor", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Cache", "HIT")
		w.WriteHeader(http.StatusInternalServerError)
	})
	for range unhealthyAfter {
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/indexers/betor?q=tt1", nil))
	}
	if st := statusOf(t, s, "betor"); !st.Healthy || st.ConsecutiveFailures != 0 {
		t.Fatalf("HIT counted: %+v", st)
	}
}
