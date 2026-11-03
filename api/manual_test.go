package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/module195/torrent-indexer/cache"
	"github.com/module195/torrent-indexer/monitoring"
)

func newManualTestIndexer() *Indexer {
	return NewIndexers(IndexersConfig{}, cache.NewRedis(), monitoring.NewMetrics(), nil, nil, nil)
}

func postManual(i *Indexer, body string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/indexers/manual", strings.NewReader(body))
	if len(header) == 2 {
		r.Header.Set(header[0], header[1])
	}
	w := httptest.NewRecorder()
	i.HandlerManualIndexer(w, r)
	return w
}

// Rejected before Redis is touched, so these run anywhere.
func TestManualRejectsBadInput(t *testing.T) {
	i := newManualTestIndexer()
	for name, body := range map[string]string{
		"not a magnet": `{"magnetLink":"isto nao e um magnet"}`,
		"no hash":      `{"magnetLink":"magnet:?dn=Filme"}`,
		"bad json":     `{"magnetLink":`,
		"too big":      `{"magnetLink":"magnet:?xt=urn:btih:` + strings.Repeat("a", 40) + `&dn=` + strings.Repeat("x", 9000) + `"}`,
	} {
		if w := postManual(i, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", name, w.Code, w.Body.String())
		}
	}
}

func TestManualToken(t *testing.T) {
	t.Setenv("MANUAL_API_TOKEN", "segredo")
	i := newManualTestIndexer()
	body := `{"magnetLink":"isto nao e um magnet"}`
	if w := postManual(i, body); w.Code != http.StatusUnauthorized {
		t.Fatalf("without token: %d, want 401", w.Code)
	}
	if w := postManual(i, body, "Authorization", "Bearer errado"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d, want 401", w.Code)
	}
	// right token gets past the check (and fails on the magnet)
	if w := postManual(i, body, "Authorization", "Bearer segredo"); w.Code != http.StatusBadRequest {
		t.Fatalf("right token: %d, want 400", w.Code)
	}
}

// With Redis: concurrent POSTs all land, the same magnet is stored once and
// q filters the list. Skipped where no Redis runs (CI).
func TestManualConcurrentPosts(t *testing.T) {
	ctx := context.Background()
	r := cache.NewRedis()
	if err := r.SetWithExpiration(ctx, "test:ping", []byte("1"), time.Second); err != nil {
		t.Skip("no Redis:", err)
	}
	_ = r.Del(ctx, manualTorrentsRedisKey)
	t.Cleanup(func() { _ = r.Del(ctx, manualTorrentsRedisKey) })

	hashes := make([]string, 6)
	for k := range hashes {
		hashes[k] = fmt.Sprintf("%040x", k+1)
		// known peers, so the POST doesn't scrape real trackers
		_ = r.SetWithExpiration(ctx, cache.Key(cache.KindPeers, hashes[k], hashes[k]), []byte(`{"seed":5,"leech":1}`), time.Minute)
	}
	i := newManualTestIndexer()
	var wg sync.WaitGroup
	for k := 0; k < 18; k++ { // each magnet three times
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			name := "Outro.Filme.2025"
			if k%6 == 0 {
				name = "Duna.Parte.Dois.2024.1080p"
			}
			postManual(i, `{"magnetLink":"magnet:?xt=urn:btih:`+hashes[k%6]+`&dn=`+name+`"}`)
		}(k)
	}
	wg.Wait()

	get := func(query string) Response {
		w := httptest.NewRecorder()
		i.HandlerManualIndexer(w, httptest.NewRequest(http.MethodGet, "/indexers/manual"+query, nil))
		var resp Response
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: %v (%s)", query, err, w.Body.String())
		}
		return resp
	}
	if all := get(""); all.Count != 6 {
		t.Fatalf("stored %d torrents, want 6 (one per magnet)", all.Count)
	}
	if duna := get("?q=duna+parte+dois"); duna.Count != 1 || duna.IndexedCount != 6 {
		t.Fatalf("q=duna: count %d indexed %d, want 1 of 6", duna.Count, duna.IndexedCount)
	}
}
