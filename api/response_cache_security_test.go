package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// S-03 (auditoria de segurança 2026-11-09): parâmetros que nenhum handler lê
// (?x=<aleatório>) ou repetidos não podem gerar chaves novas de cache, senão
// cada pedido vira um scrape novo nos sites de origem.
func TestResponseCacheKeyIgnoresUnreadParams(t *testing.T) {
	base := url.Values{"q": {"matrix"}, "season": {"1"}}
	same := []url.Values{
		{"q": {"matrix"}, "season": {"1"}, "x": {"8f3a"}},
		{"q": {"matrix"}, "season": {"1"}, "_": {"1"}, "utm_source": {"a"}},
		{"q": {"matrix", "other"}, "season": {"1", "2"}},
	}
	want := responseCacheKey("rede_torrent", base)
	for _, q := range same {
		if got := responseCacheKey("rede_torrent", q); got != want {
			t.Errorf("%s: key %q, want %q", q.Encode(), got, want)
		}
	}
	// what the handlers do read still splits the key
	for _, k := range []string{"q", "page", "filter_results", "min_similarity", "limit", "sortBy", "sortDirection", "audio", "year", "imdb", "quality", "season", "episode"} {
		q := url.Values{"q": {"matrix"}, "season": {"1"}}
		q.Set(k, "zz")
		if responseCacheKey("rede_torrent", q) == want {
			t.Errorf("param %s does not change the key", k)
		}
	}
}

// The handler must see the same query the key was computed from, so a cached
// response never answers a request it was not built for.
func TestResponseCacheWrapPassesCanonicalQuery(t *testing.T) {
	rc, _ := newTestResponseCache(t)
	var seen string
	h := rc.Wrap("caracterizacao-s03", func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cachedBody))
	})
	req := httptest.NewRequest(http.MethodGet, "/indexers/x?q=s03-canonical&q=b&junk=1&season=2&nocache=1", nil)
	h(httptest.NewRecorder(), req)
	if seen != "q=s03-canonical&season=2" {
		t.Fatalf("handler saw %q, want %q", seen, "q=s03-canonical&season=2")
	}
}

// With RESPONSE_CACHE_BYPASS_TOKEN set, nocache / Cache-Control: no-cache only
// skip the cache for callers that present the token.
func TestWantsFreshRequiresBypassTokenWhenSet(t *testing.T) {
	t.Setenv("RESPONSE_CACHE_BYPASS_TOKEN", "s3cr3t-test")
	cases := []struct {
		name, target, cacheControl, auth string
		want                             bool
	}{
		{"nocache sem token", "/x?q=a&nocache=1", "", "", false},
		{"no-cache sem token", "/x?q=a", "no-cache", "", false},
		{"nocache token errado", "/x?q=a&nocache=1", "", "Bearer errado", false},
		{"nocache com token", "/x?q=a&nocache=1", "", "Bearer s3cr3t-test", true},
		{"no-cache com token", "/x?q=a", "no-cache", "Bearer s3cr3t-test", true},
		{"sem pedido", "/x?q=a", "", "Bearer s3cr3t-test", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, c.target, nil)
		if c.cacheControl != "" {
			r.Header.Set("Cache-Control", c.cacheControl)
		}
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		if got := wantsFresh(r); got != c.want {
			t.Errorf("%s: wantsFresh = %v, want %v", c.name, got, c.want)
		}
		if r.URL.RawQuery != "q=a" {
			t.Errorf("%s: nocache not stripped: %q", c.name, r.URL.RawQuery)
		}
	}
}
