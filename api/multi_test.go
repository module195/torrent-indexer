package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/module195/torrent-indexer/schema"
)

func Test_activeIndexersMatchIndexerNames(t *testing.T) {
	rec := httptest.NewRecorder()
	HandlerIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var root struct {
		IndexerNames []string `json:"indexer_names"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range ActiveIndexers {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, root.IndexerNames) {
		t.Fatalf("ActiveIndexers %v != indexer_names %v", names, root.IndexerNames)
	}
}

func Test_capabilitySupports(t *testing.T) {
	betor, _ := capabilityFor("betor")
	rede, _ := capabilityFor("rede_torrent")
	if ok, _ := betor.Supports(" TT0133093 "); !ok {
		t.Error("betor should support IMDb ids")
	}
	if ok, reason := betor.Supports("matrix"); ok || reason != "text_not_supported" {
		t.Errorf("betor text: ok=%v reason=%q", ok, reason)
	}
	if ok, reason := rede.Supports("tt0133093"); ok || reason != "imdb_not_supported" {
		t.Errorf("rede_torrent imdb: ok=%v reason=%q", ok, reason)
	}
	if ok, _ := rede.Supports("matrix"); !ok {
		t.Error("rede_torrent should support text")
	}
}

func Test_indexerStatsTrack(t *testing.T) {
	s := NewIndexerStats()
	s.started = time.Time{} // past the startup grace
	fail := s.Track("rede_torrent", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ok := s.Track("rede_torrent", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[],"count":0}`))
	})
	hit := s.Track("rede_torrent", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Cache", "HIT")
		w.WriteHeader(http.StatusInternalServerError)
	})
	get := func(h http.HandlerFunc) {
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/indexers/rede_torrent?q=x", nil))
	}
	status := func() IndexerStatus {
		for _, st := range s.Snapshot() {
			if st.Name == "rede_torrent" {
				return st
			}
		}
		t.Fatal("rede_torrent missing from snapshot")
		return IndexerStatus{}
	}

	if st := status(); !st.Healthy || st.P50Ms != nil {
		t.Fatalf("fresh indexer: %+v", st)
	}
	for range unhealthyAfter {
		get(fail)
	}
	get(hit) // cached replays don't count
	if st := status(); st.Healthy || st.ConsecutiveFailures != unhealthyAfter {
		t.Fatalf("after failures: %+v", st)
	}
	get(ok)
	if st := status(); !st.Healthy || st.Samples != 1 || st.P50Ms == nil {
		t.Fatalf("after success: %+v", st)
	}
}

func Test_sourcesHandler(t *testing.T) {
	h := NewSourcesHandler(NewIndexerStats(), nil, Features{Multi: true, XCache: true})

	plain := httptest.NewRecorder()
	h.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/sources", nil))
	if strings.Contains(plain.Body.String(), "api_version") {
		t.Fatal("/sources without v=2 must stay the legacy payload")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sources?v=2", nil))
	var v2 struct {
		APIVersion   int             `json:"api_version"`
		IndexerNames []string        `json:"indexer_names"`
		Indexers     []IndexerStatus `json:"indexers"`
		Search       map[string]bool `json:"search"`
		Features     Features        `json:"features"`
		Endpoints    json.RawMessage `json:"endpoints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v2); err != nil {
		t.Fatal(err)
	}
	if v2.APIVersion != 2 || len(v2.IndexerNames) != len(ActiveIndexers) || len(v2.Indexers) != len(ActiveIndexers) ||
		v2.Search["meilisearch"] || !v2.Features.Multi || len(v2.Endpoints) == 0 {
		t.Fatalf("unexpected v2 payload: %s", rec.Body.String())
	}
}

func fakeIndexer(results ...schema.IndexedTorrent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		out := make([]schema.IndexedTorrent, 0, len(results))
		for _, it := range results {
			it.Title = it.Title + " " + q
			out = append(out, it)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "MISS")
		_ = json.NewEncoder(w).Encode(Response{Results: out, Count: len(out)})
	}
}

type multiPayload struct {
	Results []struct {
		Title      string   `json:"title"`
		InfoHash   string   `json:"info_hash"`
		SeedCount  int      `json:"seed_count"`
		Similarity float32  `json:"similarity"`
		FoundIn    []string `json:"found_in"`
	} `json:"results"`
	Count   int         `json:"count"`
	Partial bool        `json:"partial"`
	Calls   []multiCall `json:"calls"`
}

func runMulti(t *testing.T, h *MultiHandler, target string) (*httptest.ResponseRecorder, multiPayload) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	var p multiPayload
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("invalid JSON %q: %v", rec.Body.String(), err)
		}
	}
	return rec, p
}

func Test_multiHandlerMergesAndSkips(t *testing.T) {
	var betorQ atomic.Value
	h := NewMultiHandler(map[string]http.HandlerFunc{
		"rede_torrent": fakeIndexer(
			schema.IndexedTorrent{Title: "A", InfoHash: "AAAA", SeedCount: 5, Similarity: 0.4},
			schema.IndexedTorrent{Title: "B", InfoHash: "bbbb", SeedCount: 1, Similarity: 0.9},
		),
		"bludv_ru": fakeIndexer(
			schema.IndexedTorrent{Title: "A2", InfoHash: "aaaa", SeedCount: 9, Similarity: 0.7},
		),
		"betor": func(w http.ResponseWriter, r *http.Request) {
			betorQ.Store(r.URL.Query().Get("q"))
			fakeIndexer(schema.IndexedTorrent{Title: "C", InfoHash: "cccc", Similarity: 0.1})(w, r)
		},
	})

	rec, p := runMulti(t, h, "/indexers/multi?q=matrix&q=tt0133093&q=Matrix&indexers=rede_torrent,bludv_ru,betor,nope")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if p.Partial {
		t.Error("nothing should time out")
	}
	// 2 distinct queries ("Matrix" dedups) x 4 indexers
	if len(p.Calls) != 8 {
		t.Fatalf("calls: %+v", p.Calls)
	}
	states := map[string]string{}
	for _, c := range p.Calls {
		states[c.Indexer+"|"+c.Query] = c.State + ":" + c.Reason
	}
	want := map[string]string{
		"rede_torrent|matrix":    "ok:",
		"bludv_ru|matrix":        "ok:",
		"betor|matrix":           "skipped:text_not_supported",
		"nope|matrix":            "skipped:unknown_indexer",
		"rede_torrent|tt0133093": "skipped:imdb_not_supported",
		"bludv_ru|tt0133093":     "ok:",
		"betor|tt0133093":        "ok:",
		"nope|tt0133093":         "skipped:unknown_indexer",
	}
	for k, v := range want {
		if states[k] != v {
			t.Errorf("%s: got %q want %q", k, states[k], v)
		}
	}
	if got, _ := betorQ.Load().(string); got != "tt0133093" {
		t.Errorf("betor got q=%q", got)
	}

	// AAAA/aaaa merge: max seeds, max similarity, both indexers
	if p.Count != 3 || len(p.Results) != 3 {
		t.Fatalf("results: %+v", p.Results)
	}
	// default sort for a text query: similarity desc
	if p.Results[0].InfoHash != "bbbb" || p.Results[1].InfoHash != "AAAA" || p.Results[2].InfoHash != "cccc" {
		t.Fatalf("order: %+v", p.Results)
	}
	a := p.Results[1]
	if a.SeedCount != 9 || a.Similarity < 0.69 || !slices.Equal(a.FoundIn, []string{"rede_torrent", "bludv_ru"}) {
		t.Errorf("merged A: %+v", a)
	}

	rec, p = runMulti(t, h, "/indexers/multi?q=matrix&indexers=rede_torrent&limit=1&sortBy=seed_count&sortDirection=asc")
	if rec.Code != http.StatusOK || p.Count != 1 || p.Results[0].InfoHash != "bbbb" {
		t.Fatalf("limit/sort: %d %+v", rec.Code, p)
	}
}

func Test_multiHandlerDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h := NewMultiHandler(map[string]http.HandlerFunc{
		"rede_torrent": fakeIndexer(schema.IndexedTorrent{Title: "A", InfoHash: "aaaa"}),
		"bludv_ru": func(w http.ResponseWriter, r *http.Request) {
			<-release
		},
	})
	start := time.Now()
	rec, p := runMulti(t, h, "/indexers/multi?q=matrix&indexers=rede_torrent,bludv_ru&deadline_ms=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("deadline not honoured: %v", d)
	}
	if !p.Partial || p.Count != 1 || p.Results[0].InfoHash != "aaaa" {
		t.Fatalf("partial: %+v", p)
	}
	for _, c := range p.Calls {
		if c.Indexer == "bludv_ru" && c.State != "timeout" {
			t.Errorf("bludv_ru state %q", c.State)
		}
	}
}

func Test_multiHandlerBadRequests(t *testing.T) {
	h := NewMultiHandler(nil)
	if rec, _ := runMulti(t, h, "/indexers/multi?q=%20&q="); rec.Code != http.StatusBadRequest {
		t.Errorf("empty q: %d", rec.Code)
	}
	post := httptest.NewRecorder()
	h.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/indexers/multi?q=x", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", post.Code)
	}
	rec, p := runMulti(t, h, "/indexers/multi?q=x&indexers=rede_torrent")
	if rec.Code != http.StatusOK || p.Count != 0 || !strings.Contains(rec.Body.String(), `"results":[]`) {
		t.Fatalf("no routes: %d %s", rec.Code, rec.Body.String())
	}
}

func Test_xCacheBypass(t *testing.T) {
	var rc *ResponseCache
	rec := httptest.NewRecorder()
	rc.Wrap("x", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[],"count":0}`))
	})(rec, httptest.NewRequest(http.MethodGet, "/indexers/x?q=a", nil))
	if rec.Header().Get("X-Cache") != "BYPASS" || rec.Body.String() != `{"results":[],"count":0}` {
		t.Fatalf("got %q %q", rec.Header().Get("X-Cache"), rec.Body.String())
	}
}

func Test_mergeMultiResultsPropagatesIMDB(t *testing.T) {
	calls := []multiCall{{Indexer: "bludv"}, {Indexer: "rede_torrent"}}
	results := map[int][]schema.IndexedTorrent{
		0: {{Title: "Capitão América - O Primeiro Vingador", Year: "2011", InfoHash: "aa", IMDB: "tt0458339"}},
		1: {{Title: "Capitão América: O Primeiro Vingador 1080p", Year: "2011", InfoHash: "bb"}},
	}
	out := mergeMultiResults(calls, results)
	if len(out) != 2 || out[1].IMDB != "tt0458339" {
		t.Fatalf("got %+v", out)
	}
}

func Test_multiHandlerRunsQueriesInOrder(t *testing.T) {
	// more jobs than workers: the first query must run on every indexer
	// before any later one starts, and what is still queued at the deadline
	// comes back as not_started with no duration
	release := make(chan struct{})
	defer close(release)
	routes := map[string]http.HandlerFunc{}
	var names []string
	for _, c := range ActiveIndexers {
		if !c.SupportsText {
			continue
		}
		names = append(names, c.Name)
		routes[c.Name] = func(w http.ResponseWriter, r *http.Request) {
			<-release
		}
	}
	queries := (multiConcurrency + len(names) - 1) / len(names)
	url := "/indexers/multi?deadline_ms=500&indexers=" + strings.Join(names, ",")
	for i := 0; i <= queries; i++ {
		url += "&q=titulo" + strconv.Itoa(i)
	}
	_, p := runMulti(t, NewMultiHandler(routes), url)

	notStarted := 0
	for i, c := range p.Calls {
		if c.State != "timeout" {
			t.Fatalf("call %d: %+v", i, c)
		}
		if i < multiConcurrency {
			if c.Reason != "" || c.DurationMs == 0 {
				t.Errorf("call %d (%s) should have started: %+v", i, c.Query, c)
			}
			continue
		}
		notStarted++
		if c.Reason != "not_started" || c.DurationMs != 0 {
			t.Errorf("call %d (%s) should be not_started: %+v", i, c.Query, c)
		}
	}
	if notStarted == 0 {
		t.Fatal("test needs more jobs than workers")
	}
}
