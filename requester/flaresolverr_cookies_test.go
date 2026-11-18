package requester

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/module195/torrent-indexer/cache"
)

// solverAnswering is a FlareSolverr that answers request.get with reply and
// the session commands with an empty list and a created session.
func solverAnswering(t *testing.T, list string, reply func(target string) string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"msg":"FlareSolverr is ready!"}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(body, `"sessions.list"`):
			_, _ = w.Write([]byte(list))
		case strings.Contains(body, `"sessions.create"`):
			_, _ = w.Write([]byte(`{"status":"ok","session":"` + sessionPrefix + `new"}`))
		default:
			i := strings.Index(body, `"url":"`) + len(`"url":"`)
			target := body[i : i+strings.Index(body[i:], `"`)]
			_, _ = w.Write([]byte(reply(target)))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const emptyList = `{"status":"ok","sessions":[]}`

// When the solver returns only cookies, the page is fetched again with them.
// The cookie used to be stored under a URL without scheme, which the jar
// ignores, so the second request went without cf_clearance.
func TestFlareSolverrRetrySendsCookies(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("cf_clearance")
		if err != nil || c.Value != "ok" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<html><head><title>Just a moment...</title></head><body>Checking your browser</body></html>"))
			return
		}
		_, _ = w.Write([]byte("<html><body><div class=\"post\">pagina</div></body></html>"))
	}))
	defer site.Close()
	host, _, _ := strings.Cut(strings.TrimPrefix(site.URL, "http://"), ":")

	solver := solverAnswering(t, emptyList, func(string) string {
		return fmt.Sprintf(`{"status":"ok","solution":{"status":200,"response":"","userAgent":"UA","cookies":[{"domain":%q,"name":"cf_clearance","value":"ok","path":"/"}]}}`, host)
	})
	f := NewFlareSolverr(solver.URL, 5000, 1)
	t.Setenv("REDIS_HOST", "127.0.0.1")
	t.Setenv("REQUESTER_ALLOW_PRIVATE_HOSTS", "true") // the fixture site is on loopback
	NewRequester(f, cache.NewRedis(), 5*time.Second)
	body, err := f.Get(t.Context(), site.URL+"/busca", 0)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(body)
	if !strings.Contains(string(page), "pagina") {
		t.Fatalf("retry did not carry the cookie: %s", page)
	}
}

// The re-fetch with the solver's cookies runs in this process, so it goes
// through the requester's destination checks: a site that resolves (or
// redirects) into the internal network is refused, as with the plain client.
func TestFlareSolverrRefetchIsGuarded(t *testing.T) {
	var hits atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("<html><body>interno</body></html>"))
	}))
	defer site.Close()
	solver := solverAnswering(t, emptyList, func(string) string {
		return `{"status":"ok","solution":{"status":200,"response":"<?xml version=\"1.0\"?><x/>","userAgent":"UA","cookies":[{"domain":"127.0.0.1","name":"c","value":"v","path":"/"}]}}`
	})

	f := NewFlareSolverr(solver.URL, 5000, 1)
	if _, err := f.Get(t.Context(), site.URL+"/", 0); err == nil {
		t.Fatal("re-fetch without the requester's transport must fail")
	}

	t.Setenv("REDIS_HOST", "127.0.0.1")
	t.Setenv("REQUESTER_ALLOW_PRIVATE_HOSTS", "")
	NewRequester(f, cache.NewRedis(), 5*time.Second)
	_, err := f.Get(t.Context(), site.URL+"/", 0)
	if !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("want ErrBlockedDestination, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("internal site was reached %d times", hits.Load())
	}
}

// A page the site answered with 503, even when solved, is a failure.
func TestFlareSolverrSiteStatusIsChecked(t *testing.T) {
	solver := solverAnswering(t, emptyList, func(string) string {
		return `{"status":"ok","solution":{"status":503,"response":"<html><body>manutencao</body></html>"}}`
	})
	f := NewFlareSolverr(solver.URL, 5000, 1)
	_, err := f.Get(t.Context(), "https://example.com/", 0)
	var se *UpstreamStatusError
	if !errors.As(err, &se) || se.StatusCode != 503 {
		t.Fatalf("want UpstreamStatusError 503, got %v", err)
	}
}

// Only this process's sessions are adopted; another one's are left alone.
func TestFlareSolverrAdoptsOnlyOwnSessions(t *testing.T) {
	list := `{"status":"ok","sessions":["someone-elses-uuid","` + sessionPrefix + `old"]}`
	var used []string
	solver := solverAnswering(t, list, func(string) string {
		return `{"status":"ok","solution":{"status":200,"response":"<html><body>ok</body></html>"}}`
	})
	f := NewFlareSolverr(solver.URL, 5000, 2)
	for len(f.sessionPool) > 0 {
		used = append(used, <-f.sessionPool)
	}
	got := strings.Join(used, ",")
	if strings.Contains(got, "someone-elses-uuid") || !strings.Contains(got, sessionPrefix+"old") || len(used) != 2 {
		t.Fatalf("pool = %v", used)
	}
}

// A sessions.list in an unexpected shape is "not supported", not a panic.
func TestFlareSolverrListSessionsOddShape(t *testing.T) {
	for _, list := range []string{
		`{"status":"ok","sessions":{"a":1}}`,
		`{"status":"ok","sessions":[{"id":"a"}]}`,
	} {
		solver := solverAnswering(t, list, func(string) string { return `{}` })
		f := &FlareSolverr{url: solver.URL, httpClient: http.DefaultClient}
		ids, err := f.ListSessions()
		if err != nil && !errors.Is(err, ErrListSessions) {
			t.Fatalf("%s: %v", list, err)
		}
		if len(ids) != 0 {
			t.Fatalf("%s: ids = %v", list, ids)
		}
	}
}

func TestCookieURL(t *testing.T) {
	target, _ := url.Parse("https://www.site.com/busca")
	for _, dom := range []string{".site.com", "site.com", ""} {
		if u := cookieURL(target, dom); u.Scheme != "https" || strings.HasPrefix(u.Host, ".") || u.Host == "" {
			t.Errorf("cookieURL(%q) = %v", dom, u)
		}
	}
}
