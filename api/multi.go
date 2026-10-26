package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/magnet"
	"github.com/module195/torrent-indexer/schema"
	"github.com/module195/torrent-indexer/utils"
)

const (
	multiMaxQueries      = 10
	multiConcurrency     = 16 // sub-calls running at once per /indexers/multi request
	multiDefaultDeadline = 8 * time.Second
	multiMinDeadline     = 500 * time.Millisecond
	multiMaxDeadline     = 60 * time.Second
	// multiCallTimeout bounds a sub-call that outlives the deadline. It keeps
	// running detached so its result still reaches the response cache.
	multiCallTimeout = 2 * time.Minute
	// multiBackgroundWait is how long a call still queued at the deadline
	// keeps waiting for a free slot to run in the background (only when the
	// response cache is on, so its result is reused); then it is dropped.
	multiBackgroundWait = 30 * time.Second
)

// Limits shared by every /indexers/multi request (and by the calls that
// outlive their deadline): at most multiPerIndexer calls on one site and
// multiGlobal in total, so back-to-back searches don't pile up on the same
// site and on FlareSolverr.
var (
	multiPerIndexer = positiveEnvInt("MULTI_INDEXER_CONCURRENCY", 3)
	multiGlobal     = positiveEnvInt("MULTI_GLOBAL_CONCURRENCY", 32)
)

func positiveEnvInt(name string, def int) int {
	if n, err := strconv.Atoi(utils.GetEnvOrDefault(name, "")); err == nil && n > 0 {
		return n
	}
	return def
}

// multiLimiter counts running sub-calls per indexer and in total.
type multiLimiter struct {
	mu         sync.Mutex
	perIndexer int
	global     int
	running    int
	byIndexer  map[string]int
	wake       chan struct{} // closed and replaced whenever a slot frees
}

func newMultiLimiter(perIndexer, global int) *multiLimiter {
	return &multiLimiter{perIndexer: perIndexer, global: global, byIndexer: map[string]int{}, wake: make(chan struct{})}
}

// tryAcquire takes a slot for name if both limits allow it.
func (l *multiLimiter) tryAcquire(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running >= l.global || l.byIndexer[name] >= l.perIndexer {
		return false
	}
	l.running++
	l.byIndexer[name]++
	return true
}

func (l *multiLimiter) release(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.running--
	l.byIndexer[name]--
	close(l.wake)
	l.wake = make(chan struct{})
}

// changed fires on the next release. Take it before trying to acquire, so a
// release in between is not missed.
func (l *multiLimiter) changed() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.wake
}

// MultiHandler serves GET /indexers/multi: one request fans out to several
// indexers x queries in-process (through the same handlers, cache and stats as
// /indexers/<name>), merges the results by info_hash and answers by
// deadline_ms with whatever finished ("partial": true when something didn't).
type MultiHandler struct {
	routes  map[string]http.HandlerFunc
	limiter *multiLimiter
	// WarmCache runs the calls still queued at the deadline in the
	// background, so their result lands in the response cache. Only worth it
	// when the cache is on.
	WarmCache bool
}

// NewMultiHandler takes the /indexers/<name> handlers keyed by indexer name.
func NewMultiHandler(routes map[string]http.HandlerFunc) *MultiHandler {
	return &MultiHandler{routes: routes, limiter: newMultiLimiter(multiPerIndexer, multiGlobal)}
}

// multiCall reports what happened to one (indexer, q) pair.
type multiCall struct {
	Indexer    string `json:"indexer"`
	Query      string `json:"q"`
	State      string `json:"state"` // ok | error | skipped | timeout
	Status     int    `json:"status,omitempty"`
	Count      int    `json:"count"`
	DurationMs int64  `json:"duration_ms"`
	Cache      string `json:"cache,omitempty"` // X-Cache of the sub-call
	// skipped: unknown_indexer | imdb_not_supported | text_not_supported;
	// timeout: not_started when the call was still queued at the deadline
	Reason string `json:"reason,omitempty"`
}

type multiTorrent struct {
	schema.IndexedTorrent
	FoundIn []string `json:"found_in"`
}

type multiResponse struct {
	Results []multiTorrent `json:"results"`
	Count   int            `json:"count"`
	Partial bool           `json:"partial"`
	Calls   []multiCall    `json:"calls"`
}

type multiJob struct {
	idx   int // position in calls
	name  string
	q     string
	route http.HandlerFunc
}

type multiResult struct {
	idx      int
	call     multiCall
	torrents []schema.IndexedTorrent
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (h *MultiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	params := r.URL.Query()
	queries := multiQueries(params["q"])
	if len(queries) == 0 {
		writeJSONError(w, http.StatusBadRequest, "at least one non-empty q is required")
		return
	}
	names := multiIndexerNames(params.Get("indexers"))
	deadline := multiDeadline(params.Get("deadline_ms"))

	// everything else (sortBy, audio, year, imdb, ...) goes to each sub-call.
	// limit is applied once, after the merge: cutting each sub-call first
	// would drop torrents that rank high only once seeds are merged.
	forward := url.Values{}
	for k, v := range params {
		switch k {
		case "q", "indexers", "deadline_ms", "limit":
			continue
		}
		forward[k] = v
	}

	calls := make([]multiCall, 0, len(queries)*len(names))
	var jobs []multiJob
	for _, q := range queries {
		for _, name := range names {
			c := multiCall{Indexer: name, Query: q}
			route, hasRoute := h.routes[name]
			capab, known := capabilityFor(name)
			if !hasRoute || !known {
				c.State, c.Reason = "skipped", "unknown_indexer"
			} else if ok, reason := capab.Supports(q); !ok {
				c.State, c.Reason = "skipped", reason
			} else {
				jobs = append(jobs, multiJob{idx: len(calls), name: name, q: q, route: route})
			}
			calls = append(calls, c)
		}
	}

	started := time.Now()
	deadlineCtx, cancel := context.WithTimeout(r.Context(), deadline)
	defer cancel()
	// Sub-calls must not touch r once this handler returns, so they all
	// derive from a detached copy made up front.
	base := r.Clone(context.WithoutCancel(r.Context()))

	// The jobs start in order (every indexer for q[0], then q[1], ...), so the
	// ones left waiting at the deadline are the last queries. A job whose
	// indexer is at its limit is passed over for the next one, which may be a
	// different site, and tried again when a slot frees. At most
	// multiConcurrency run at once for this request.
	running := make([]atomic.Bool, len(jobs))
	done := make(chan multiResult, len(jobs))
	go h.dispatch(deadlineCtx, base, jobs, forward, running, done, started, deadline)

	results := make(map[int][]schema.IndexedTorrent, len(jobs))
	finished := make(map[int]bool, len(jobs))
collect:
	for len(finished) < len(jobs) {
		select {
		case res := <-done:
			calls[res.idx] = res.call
			results[res.idx] = res.torrents
			finished[res.idx] = true
		case <-deadlineCtx.Done():
			break collect
		}
	}
	if r.Context().Err() != nil {
		return // client went away
	}

	partial := false
	for i, j := range jobs {
		if finished[j.idx] {
			continue
		}
		partial = true
		calls[j.idx].State = "timeout"
		if running[i].Load() {
			calls[j.idx].DurationMs = deadline.Milliseconds()
		} else {
			// never ran, so it says nothing about how fast the indexer is
			calls[j.idx].Reason = "not_started"
		}
	}

	logMultiCalls(calls, partial, deadline, time.Since(started))

	merged := mergeMultiResults(calls, results)
	sortParams := url.Values{}
	for k, v := range forward {
		sortParams[k] = v
	}
	sortParams.Set("q", queries[0])
	cmp := torrentComparator(sortParams)
	slices.SortStableFunc(merged, func(a, b multiTorrent) int {
		return cmp(a.IndexedTorrent, b.IndexedTorrent)
	})
	if limit, err := strconv.Atoi(params.Get("limit")); err == nil && limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(multiResponse{Results: merged, Count: len(merged), Partial: partial, Calls: calls})
}

// dispatch starts jobs as slots free up. Until the deadline it serves the
// response; after it, with WarmCache, what is still queued keeps going for up
// to multiBackgroundWait so its result reaches the response cache.
func (h *MultiHandler) dispatch(deadlineCtx context.Context, base *http.Request, jobs []multiJob, forward url.Values,
	running []atomic.Bool, done chan<- multiResult, started time.Time, deadline time.Duration) {
	var inRequest atomic.Int32
	pending := make([]int, len(jobs))
	for i := range pending {
		pending[i] = i
	}
	deadlineDone := deadlineCtx.Done() // nil once the deadline passed
	var background <-chan time.Time
	toBackground := func() bool {
		deadlineDone = nil
		if !h.WarmCache {
			return false
		}
		background = time.After(multiBackgroundWait)
		return true
	}
	for len(pending) > 0 {
		if deadlineDone != nil && deadlineCtx.Err() != nil && !toBackground() {
			return
		}
		wake := h.limiter.changed()
		kept := pending[:0]
		for _, i := range pending {
			j := jobs[i]
			if inRequest.Load() >= multiConcurrency || !h.limiter.tryAcquire(j.name) {
				kept = append(kept, i)
				continue
			}
			inRequest.Add(1)
			running[i].Store(true)
			go func() {
				defer func() {
					inRequest.Add(-1)
					h.limiter.release(j.name)
				}()
				res := runMultiCall(base, j, forward)
				if time.Since(started) > deadline {
					// The response already went out marking this call as timeout;
					// this is the only place its real duration shows up.
					logging.Warn().
						Str("indexer", j.name).
						Str("q", j.q).
						Str("state", res.call.State).
						Int("count", res.call.Count).
						Int64("duration_ms", res.call.DurationMs).
						Int64("deadline_ms", deadline.Milliseconds()).
						Msg("multi sub-call finished after deadline")
				}
				done <- res
			}()
		}
		pending = kept
		if len(pending) == 0 {
			return
		}
		select {
		case <-wake:
		case <-deadlineDone:
			if !toBackground() {
				return
			}
		case <-background:
			logging.Info().Int("dropped", len(pending)).Msg("multi: queued sub-calls dropped, no free slot")
			return
		}
	}
}

// runMultiCall runs one indexer handler for q and decodes its JSON.
func runMultiCall(base *http.Request, j multiJob, forward url.Values) multiResult {
	ctx, cancel := context.WithTimeout(base.Context(), multiCallTimeout)
	defer cancel()

	vals := url.Values{}
	for k, v := range forward {
		vals[k] = v
	}
	vals.Set("q", j.q)
	sub := base.Clone(ctx)
	sub.URL.Path = "/indexers/" + j.name
	sub.URL.RawPath = ""
	sub.URL.RawQuery = vals.Encode()
	sub.RequestURI = sub.URL.RequestURI()
	sub.Body = http.NoBody
	sub.ContentLength = 0

	start := time.Now()
	bw := &bufferedWriter{header: http.Header{}}
	j.route(bw, sub)
	status := bw.status
	if status == 0 {
		status = http.StatusOK
	}
	c := multiCall{
		Indexer:    j.name,
		Query:      j.q,
		Status:     status,
		DurationMs: time.Since(start).Milliseconds(),
		Cache:      bw.header.Get("X-Cache"),
	}

	var resp Response
	if status != http.StatusOK {
		c.State, c.Reason = "error", errorReason(bw.buf.Bytes())
		return multiResult{idx: j.idx, call: c}
	}
	if err := json.Unmarshal(bw.buf.Bytes(), &resp); err != nil {
		c.State, c.Reason = "error", "invalid_json"
		return multiResult{idx: j.idx, call: c}
	}
	c.State, c.Count = "ok", len(resp.Results)
	return multiResult{idx: j.idx, call: c, torrents: resp.Results}
}

// logMultiCalls writes one line per /indexers/multi request with how each
// (indexer, q) pair went: "indexer=state/duration/count", skipped ones left out.
func logMultiCalls(calls []multiCall, partial bool, deadline, elapsed time.Duration) {
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		if c.State == "skipped" {
			continue
		}
		parts = append(parts, c.Indexer+"="+c.State+"/"+strconv.FormatInt(c.DurationMs, 10)+"ms/"+strconv.Itoa(c.Count))
	}
	logging.Info().
		Bool("partial", partial).
		Int64("deadline_ms", deadline.Milliseconds()).
		Int64("elapsed_ms", elapsed.Milliseconds()).
		Str("calls", strings.Join(parts, " ")).
		Msg("multi calls")
}

// errorReason extracts the {"error": "..."} message indexers write on failure.
func errorReason(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error == "" {
		return ""
	}
	return truncateUTF8(e.Error, 200)
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// mergeMultiResults dedups by info_hash (falling back to the magnet's), in
// call order, keeping the best similarity and every indexer that had it.
func mergeMultiResults(calls []multiCall, results map[int][]schema.IndexedTorrent) []multiTorrent {
	byHash := map[string]int{}
	out := make([]multiTorrent, 0)
	for idx, c := range calls {
		for _, t := range results[idx] {
			key := torrentKey(t)
			if key == "" {
				out = append(out, multiTorrent{IndexedTorrent: t, FoundIn: []string{c.Indexer}})
				continue
			}
			if pos, ok := byHash[key]; ok {
				m := &out[pos]
				sim := max(m.Similarity, t.Similarity)
				if isUnsafeTitle(m.Title) && !isUnsafeTitle(t.Title) && t.Title != "" {
					// a real title beats the [UNSAFE] fallback, so keep this copy as the base
					m.IndexedTorrent = mergeTorrent(t, m.IndexedTorrent)
				} else {
					m.IndexedTorrent = mergeTorrent(m.IndexedTorrent, t)
				}
				m.Similarity = sim
				if !slices.Contains(m.FoundIn, c.Indexer) {
					m.FoundIn = append(m.FoundIn, c.Indexer)
				}
				continue
			}
			byHash[key] = len(out)
			out = append(out, multiTorrent{IndexedTorrent: t, FoundIn: []string{c.Indexer}})
		}
	}
	ptrs := make([]*schema.IndexedTorrent, len(out))
	for i := range out {
		ptrs[i] = &out[i].IndexedTorrent
	}
	propagateIMDB(ptrs)
	return out
}

func isUnsafeTitle(title string) bool {
	return reBAUnsafe.MatchString(title)
}

func torrentKey(t schema.IndexedTorrent) string {
	if h := strings.ToLower(strings.TrimSpace(t.InfoHash)); h != "" {
		return h
	}
	if t.MagnetLink == "" {
		return ""
	}
	m, err := magnet.ParseMagnetUri(t.MagnetLink)
	if err != nil {
		return ""
	}
	return strings.ToLower(m.InfoHash.String())
}

// multiQueries trims, collapses inner whitespace, dedups (case-insensitively)
// and caps the q values.
func multiQueries(raw []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, q := range raw {
		q = strings.Join(strings.Fields(q), " ")
		k := strings.ToLower(q)
		if q == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, q)
		if len(out) == multiMaxQueries {
			break
		}
	}
	return out
}

// multiIndexerNames parses the "indexers" CSV; empty means every active one.
func multiIndexerNames(csv string) []string {
	var out []string
	for _, n := range strings.Split(csv, ",") {
		n = strings.ToLower(strings.TrimSpace(n))
		if n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		for _, c := range ActiveIndexers {
			out = append(out, c.Name)
		}
	}
	return out
}

func multiDeadline(v string) time.Duration {
	ms, err := strconv.Atoi(v)
	if err != nil || ms <= 0 {
		return multiDefaultDeadline
	}
	d := time.Duration(ms) * time.Millisecond
	if d < multiMinDeadline {
		return multiMinDeadline
	}
	if d > multiMaxDeadline {
		return multiMaxDeadline
	}
	return d
}
