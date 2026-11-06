package handler

// Caracterização do cache de respostas (RESPONSE_CACHE_TTL), auditoria de
// desempenho de 06/11/2026: quantas vezes o handler do site roda para pedidos
// iguais, o X-Cache de cada um e o corpo devolvido.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/module195/torrent-indexer/cache"
	"github.com/module195/torrent-indexer/monitoring"
)

var testMetricsOnce = sync.OnceValue(monitoring.NewMetrics)

// newTestResponseCache usa o Redis em REDIS_HOST (localhost por padrão). Sem
// Redis, toda leitura falha e o cache só coalesce, que é o que os testes de
// coalescência precisam; os testes de HIT pulam.
func newTestResponseCache(t *testing.T) (*ResponseCache, bool) {
	t.Helper()
	r := cache.NewRedis()
	ok := redisReachable()
	i := &Indexer{redis: r, metrics: testMetricsOnce()}
	return NewResponseCache(i, time.Minute), ok
}

func redisReachable() bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:6379", 200*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

const cachedBody = `{"results":[{"title":"x"}],"count":1,"indexed_count":1}`

func countingHandler(runs *atomic.Int32, release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runs.Add(1)
		if release != nil {
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cachedBody))
	}
}

// Pedidos idênticos simultâneos rodam o handler uma vez. O singleflight marca
// como compartilhado também quem disparou a execução, então todos saem SHARED.
func TestResponseCacheCoalescesIdenticalRequests(t *testing.T) {
	rc, _ := newTestResponseCache(t)
	var runs atomic.Int32
	release := make(chan struct{})
	h := rc.Wrap("caracterizacao", countingHandler(&runs, release))
	q := "/indexers/caracterizacao?q=coalesce-" + time.Now().Format("150405.000000")

	const n = 6
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for k := 0; k < n; k++ {
		recs[k] = httptest.NewRecorder()
		wg.Add(1)
		go func() {
			defer wg.Done()
			h(recs[k], httptest.NewRequest(http.MethodGet, q, nil))
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for runs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := runs.Load(); got != 1 {
		t.Fatalf("handler rodou %d vezes, want 1", got)
	}
	count := map[string]int{}
	for _, rec := range recs {
		count[rec.Header().Get("X-Cache")]++
		if rec.Body.String() != cachedBody || rec.Code != http.StatusOK {
			t.Fatalf("resposta %d %q", rec.Code, rec.Body.String())
		}
	}
	if count["SHARED"] != n {
		t.Fatalf("X-Cache = %v, want %d SHARED", count, n)
	}
}

// A ordem dos parâmetros não muda a chave; parâmetros diferentes mudam.
func TestResponseCacheKeyCanonicalization(t *testing.T) {
	rc, ok := newTestResponseCache(t)
	if !ok {
		t.Skip("sem Redis em 127.0.0.1:6379")
	}
	var runs atomic.Int32
	h := rc.Wrap("caracterizacao", countingHandler(&runs, nil))
	tag := time.Now().Format("150405.000000")
	t.Cleanup(func() {
		for _, q := range []string{"q=a-" + tag + "&season=1", "q=a-" + tag + "&season=2"} {
			_ = rc.redis.Del(context.Background(), cache.Key(cache.KindResp, "caracterizacao:"+q, ""))
		}
	})
	do := func(path string) string {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Header().Get("X-Cache")
	}
	if got := do("/indexers/caracterizacao?q=a-" + tag + "&season=1"); got != "MISS" {
		t.Fatalf("primeiro = %s", got)
	}
	if got := do("/indexers/caracterizacao?season=1&q=a-" + tag); got != "HIT" {
		t.Fatalf("mesma query em outra ordem = %s, want HIT", got)
	}
	if got := do("/indexers/caracterizacao?q=a-" + tag + "&season=2"); got != "MISS" {
		t.Fatalf("query diferente = %s, want MISS", got)
	}
	if got := do("/indexers/caracterizacao?q=a-" + tag + "&season=1&nocache=1"); got != "MISS" {
		t.Fatalf("nocache = %s, want MISS", got)
	}
	if got := runs.Load(); got != 3 {
		t.Fatalf("handler rodou %d vezes, want 3", got)
	}
}
