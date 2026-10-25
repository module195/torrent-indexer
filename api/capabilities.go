package handler

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	meilisearch "github.com/module195/torrent-indexer/search"
)

// IndexerCapability describes which kind of query an indexer can answer.
// It mirrors the routing rules: only betor and bludv_ru accept IMDb IDs (for
// the others skipIMDBQueryMiddleware answers an empty list) and betor itself
// answers an empty list for free-text queries. bludv_ru finds an ID because
// its posts link the IMDb page and the WordPress search reads the post body.
type IndexerCapability struct {
	Name         string
	SupportsText bool
	SupportsIMDb bool
}

// ActiveIndexers lists the indexers advertised in "indexer_names", in the same
// order. Keep both lists in sync (see Test_activeIndexersMatchIndexerNames).
var ActiveIndexers = []IndexerCapability{
	{Name: "torrent-dos-filmes", SupportsText: true},
	{Name: "comando_torrents", SupportsText: true},
	{Name: "rede_torrent", SupportsText: true},
	{Name: "torrentfilmeshds", SupportsText: true},
	{Name: "bludv_ru", SupportsText: true, SupportsIMDb: true},
	{Name: "baixarapido", SupportsText: true},
	{Name: "betor", SupportsIMDb: true},
}

func capabilityFor(name string) (IndexerCapability, bool) {
	for _, c := range ActiveIndexers {
		if c.Name == name {
			return c, true
		}
	}
	return IndexerCapability{}, false
}

// Supports reports whether the indexer can answer q; when it can't, reason
// says why ("imdb_not_supported" or "text_not_supported").
func (c IndexerCapability) Supports(q string) (ok bool, reason string) {
	if reIMDbID.MatchString(strings.ToLower(strings.TrimSpace(q))) {
		if !c.SupportsIMDb {
			return false, "imdb_not_supported"
		}
		return true, ""
	}
	if !c.SupportsText {
		return false, "text_not_supported"
	}
	return true, ""
}

const (
	// statsWindow is how many recent successful runs feed p50_ms.
	statsWindow = 50
	// unhealthyAfter consecutive failed runs mark an indexer as unhealthy.
	unhealthyAfter = 3
	// healthRetryAfter is how long an unhealthy indexer stays out before it is
	// reported healthy again, so clients that skip unhealthy indexers retry
	// it. Each failed retry doubles the interval (5m, 10m, 20m, 40m) up to
	// healthRetryMax; a success resets the failure count.
	healthRetryAfter = 5 * time.Minute
	healthRetryMax   = time.Hour
	// startupGrace keeps every indexer reported healthy right after start,
	// while FlareSolverr sessions and caches are still warming up, so a few
	// early 5xx don't take indexers out for the first retry interval.
	startupGrace = 2 * time.Minute
)

// retryAfter is how long an indexer with n consecutive failures stays out.
func retryAfter(n int) time.Duration {
	d := healthRetryAfter
	for i := unhealthyAfter; i < n && d < healthRetryMax; i++ {
		d *= 2
	}
	if d > healthRetryMax {
		return healthRetryMax
	}
	return d
}

type indexerStat struct {
	durations           []time.Duration // ring buffer of successful runs
	next                int
	consecutiveFailures int
	lastFailure         time.Time
}

// IndexerStats keeps a small in-memory view of how each indexer is doing, so
// /sources?v=2 can tell clients (guindex) which indexers are healthy and how
// long they usually take. Cached replays (X-Cache: HIT) are not counted.
type IndexerStats struct {
	mu      sync.Mutex
	byName  map[string]*indexerStat
	now     func() time.Time
	started time.Time
}

func NewIndexerStats() *IndexerStats {
	return &IndexerStats{byName: map[string]*indexerStat{}, now: time.Now, started: time.Now()}
}

// statusWriter passes everything through and remembers the status code.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(p)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Track records the outcome and duration of every GET served by next. The
// response itself is passed through untouched.
func (s *IndexerStats) Track(name string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			next(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next(sw, r)
		if r.Context().Err() != nil || sw.Header().Get("X-Cache") == "HIT" {
			// client gave up, or a cached replay: says nothing about the site
			return
		}
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		s.record(name, status, time.Since(start))
	}
}

func isFailureStatus(status int) bool {
	return status >= 500 || status == http.StatusForbidden || status == http.StatusTooManyRequests
}

func (s *IndexerStats) record(name string, status int, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.byName[name]
	if st == nil {
		st = &indexerStat{}
		s.byName[name] = st
	}
	if isFailureStatus(status) {
		st.consecutiveFailures++
		st.lastFailure = s.now()
		return
	}
	st.consecutiveFailures = 0
	if status >= 400 {
		return
	}
	if len(st.durations) < statsWindow {
		st.durations = append(st.durations, d)
	} else {
		st.durations[st.next] = d
	}
	st.next = (st.next + 1) % statsWindow
}

// IndexerStatus is one entry of the "indexers" list in /sources?v=2.
type IndexerStatus struct {
	Name                string `json:"name"`
	SupportsText        bool   `json:"supports_text"`
	SupportsIMDb        bool   `json:"supports_imdb"`
	Healthy             bool   `json:"healthy"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastFailureAt       string `json:"last_failure_at,omitempty"` // RFC 3339, UTC
	P50Ms               *int64 `json:"p50_ms,omitempty"`          // absent until a run succeeded
	Samples             int    `json:"samples"`
	// RetryAfterS is set only while unhealthy: seconds until it is offered again.
	RetryAfterS *int64 `json:"retry_after_s,omitempty"`
}

// Snapshot returns the status of every active indexer, in ActiveIndexers order.
func (s *IndexerStats) Snapshot() []IndexerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make([]IndexerStatus, 0, len(ActiveIndexers))
	for _, c := range ActiveIndexers {
		is := IndexerStatus{Name: c.Name, SupportsText: c.SupportsText, SupportsIMDb: c.SupportsIMDb, Healthy: true}
		if st := s.byName[c.Name]; st != nil {
			is.ConsecutiveFailures = st.consecutiveFailures
			if st.consecutiveFailures >= unhealthyAfter && now.Sub(s.started) >= startupGrace {
				if left := retryAfter(st.consecutiveFailures) - now.Sub(st.lastFailure); left > 0 {
					is.Healthy = false
					secs := int64((left + time.Second - 1) / time.Second)
					is.RetryAfterS = &secs
				}
			}
			if !st.lastFailure.IsZero() {
				is.LastFailureAt = st.lastFailure.UTC().Format(time.RFC3339)
			}
			is.Samples = len(st.durations)
			if len(st.durations) > 0 {
				sorted := slices.Clone(st.durations)
				slices.Sort(sorted)
				p50 := sorted[len(sorted)/2].Milliseconds()
				is.P50Ms = &p50
			}
		}
		out = append(out, is)
	}
	return out
}

// Features advertises optional server capabilities in /sources?v=2.
type Features struct {
	Multi         bool `json:"multi"`          // GET /indexers/multi is available
	XCache        bool `json:"x_cache"`        // indexer routes send X-Cache
	ResponseCache bool `json:"response_cache"` // RESPONSE_CACHE_TTL is enabled
}

// meiliHealthTTL bounds how often /sources?v=2 probes Meilisearch.
const meiliHealthTTL = 60 * time.Second

// SourcesHandler serves /sources. Without "v=2" it is exactly HandlerIndex
// (what the catch-all "/" already answered for /sources). With "v=2" it adds
// "api_version", "indexers", "search" and "features" to the same payload.
type SourcesHandler struct {
	stats    *IndexerStats
	search   *meilisearch.SearchIndexer
	features Features

	meiliMu        sync.Mutex
	meiliHealthy   bool
	meiliCheckedAt time.Time
}

func NewSourcesHandler(stats *IndexerStats, si *meilisearch.SearchIndexer, f Features) *SourcesHandler {
	return &SourcesHandler{stats: stats, search: si, features: f}
}

func (h *SourcesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("v") != "2" {
		HandlerIndex(w, r)
		return
	}

	bw := &bufferedWriter{header: http.Header{}}
	HandlerIndex(bw, r)
	var base map[string]json.RawMessage
	if err := json.Unmarshal(bw.buf.Bytes(), &base); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	payload := make(map[string]any, len(base)+4)
	for k, v := range base {
		payload[k] = v
	}
	payload["api_version"] = 2
	payload["indexers"] = h.stats.Snapshot()
	payload["search"] = map[string]bool{"meilisearch": h.isMeiliHealthy()}
	payload["features"] = h.features

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// isMeiliHealthy reports whether Meilisearch is configured and answering,
// caching the probe for meiliHealthTTL.
func (h *SourcesHandler) isMeiliHealthy() bool {
	if h.search == nil || h.search.BaseURL == "" {
		return false
	}
	h.meiliMu.Lock()
	if time.Since(h.meiliCheckedAt) < meiliHealthTTL {
		ok := h.meiliHealthy
		h.meiliMu.Unlock()
		return ok
	}
	h.meiliMu.Unlock()

	ok := h.search.IsHealthy()

	h.meiliMu.Lock()
	h.meiliHealthy, h.meiliCheckedAt = ok, time.Now()
	h.meiliMu.Unlock()
	return ok
}
