package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/magnet"
	"github.com/module195/torrent-indexer/schema"
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
)

// MultiHandler serves GET /indexers/multi: one request fans out to several
// indexers x queries in-process (through the same handlers, cache and stats as
// /indexers/<name>), merges the results by info_hash and answers by
// deadline_ms with whatever finished ("partial": true when something didn't).
type MultiHandler struct {
	routes map[string]http.HandlerFunc
}

// NewMultiHandler takes the /indexers/<name> handlers keyed by indexer name.
func NewMultiHandler(routes map[string]http.HandlerFunc) *MultiHandler {
	return &MultiHandler{routes: routes}
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

	// everything else (limit, sortBy, audio, year, imdb, ...) goes to each sub-call
	forward := url.Values{}
	for k, v := range params {
		switch k {
		case "q", "indexers", "deadline_ms":
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

	// A fixed pool of workers takes the jobs in order (every indexer for q[0],
	// then q[1], ...), so when there are more jobs than workers the ones left
	// waiting at the deadline are the last queries, not random ones. One
	// goroutine per job racing for a semaphore starts them in whatever order
	// the scheduler picks.
	var next atomic.Int64
	running := make([]atomic.Bool, len(jobs))
	done := make(chan multiResult, len(jobs))
	for range min(multiConcurrency, len(jobs)) {
		go func() {
			for deadlineCtx.Err() == nil {
				i := int(next.Add(1) - 1)
				if i >= len(jobs) {
					return
				}
				j := jobs[i]
				running[i].Store(true)
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
			}
		}()
	}

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
	if len(e.Error) > 200 {
		return e.Error[:200]
	}
	return e.Error
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
				m.IndexedTorrent = mergeTorrent(m.IndexedTorrent, t)
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

// multiQueries trims, dedups (case-insensitively) and caps the q values.
func multiQueries(raw []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, q := range raw {
		q = strings.TrimSpace(q)
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
