package requester

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const challengePage = "<html><body>Just a moment...</body></html>"

// cfSite answers like a Cloudflare-protected site: a challenge unless the
// request carries cf_clearance=ok with the solver's user agent.
func cfSite(t *testing.T, accept func() bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var cleared atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("cf_clearance")
		if err == nil && c.Value == "ok" && r.UserAgent() == "SolverUA" && accept() {
			cleared.Add(1)
			_, _ = io.WriteString(w, "<html><body><div class=\"post\">"+r.URL.Path+"</div></body></html>")
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, challengePage)
	}))
	t.Cleanup(srv.Close)
	return srv, &cleared
}

// solverFor solves any page of site, returning the page and a clearance.
func solverFor(t *testing.T, solves *atomic.Int32) *httptest.Server {
	return solverAnswering(t, emptyList, func(target string) string {
		solves.Add(1)
		host := clearanceHost(target)
		page := "<html><body><div class=\\\"post\\\">solved</div></body></html>"
		exp := time.Now().Add(time.Hour).Unix()
		return fmt.Sprintf(`{"status":"ok","solution":{"status":200,"response":"%s","userAgent":"SolverUA","cookies":[{"domain":%q,"name":"cf_clearance","value":"ok","path":"/","expiry":%d},{"domain":%q,"name":"__cf_bm","value":"x","path":"/"}]}}`, page, host, exp, host)
	})
}

func get(t *testing.T, r *Requster, url string) string {
	t.Helper()
	rc, err := r.GetDocument(context.Background(), url)
	if err != nil {
		t.Fatalf("%s: %v", url, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

// After one solve, the other pages of the same host go through the plain
// client with the solver's clearance: one solve per host, not one per page.
func TestClearanceReusedAcrossPagesOfHost(t *testing.T) {
	site, cleared := cfSite(t, func() bool { return true })
	var solves atomic.Int32
	r := newTestRequester(t)
	r.fs = NewFlareSolverr(solverFor(t, &solves).URL, 5000, 1)

	if body := get(t, r, site.URL+"/busca?s=x"); !strings.Contains(body, "solved") {
		t.Fatalf("first page should come from the solver: %s", body)
	}
	for _, p := range []string{"/filme-1", "/filme-2", "/filme-3"} {
		if body := get(t, r, site.URL+p); !strings.Contains(body, p) {
			t.Fatalf("%s: want the page through the plain client, got %s", p, body)
		}
	}
	if n := solves.Load(); n != 1 {
		t.Fatalf("solves = %d, want 1", n)
	}
	if n := cleared.Load(); n != 3 {
		t.Fatalf("plain requests past the challenge = %d, want 3", n)
	}
}

// A clearance the site no longer takes is dropped, the page still comes
// from the solver, and no new clearance is stored for that host for a while.
func TestClearanceRejectedFallsBackAndBacksOff(t *testing.T) {
	site, _ := cfSite(t, func() bool { return false })
	var solves atomic.Int32
	r := newTestRequester(t)
	r.fs = NewFlareSolverr(solverFor(t, &solves).URL, 5000, 1)

	get(t, r, site.URL+"/a") // solve, clearance stored
	if _, ok := r.fs.clearanceFor(site.URL + "/b"); !ok {
		t.Fatal("clearance should be stored after a solve")
	}
	if body := get(t, r, site.URL+"/b"); !strings.Contains(body, "solved") {
		t.Fatalf("rejected clearance must fall back to the solver: %s", body)
	}
	if _, ok := r.fs.clearanceFor(site.URL + "/c"); ok {
		t.Fatal("clearance kept after the site rejected it")
	}
	if n := solves.Load(); n != 2 {
		t.Fatalf("solves = %d, want 2", n)
	}
}

func TestClearanceStoreRules(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	s := newClearanceStore()
	s.now = func() time.Time { return now }
	sol := func(name string, expiry int) solution {
		var x solution
		x.UserAgent = "UA"
		x.Cookies = append(x.Cookies, solverCookie{Name: name, Value: "v", Expiry: expiry})
		return x
	}

	s.remember("https://a.com/x", sol("__cf_bm", 0))
	if _, ok := s.lookup("https://a.com/y"); ok {
		t.Fatal("stored without cf_clearance")
	}

	s.remember("https://A.com/x", sol("cf_clearance", 0))
	c, ok := s.lookup("https://a.com/y")
	if !ok || c.expires != now.Add(clearanceMaxTTL) {
		t.Fatalf("want clearance for the host with the max TTL, got %v %v", ok, c.expires)
	}
	if _, ok := s.lookup("https://b.com/y"); ok {
		t.Fatal("clearance leaked to another host")
	}

	soon := now.Add(5 * time.Minute)
	s.remember("https://c.com/", sol("cf_clearance", int(soon.Unix())))
	if c, _ := s.lookup("https://c.com/"); !c.expires.Equal(soon) {
		t.Fatalf("cookie expiry not honored: %v", c.expires)
	}

	now = now.Add(clearanceMaxTTL)
	if _, ok := s.lookup("https://a.com/"); ok {
		t.Fatal("expired clearance reused")
	}

	s.rejected("https://d.com/")
	s.remember("https://d.com/", sol("cf_clearance", 0))
	if _, ok := s.lookup("https://d.com/"); ok {
		t.Fatal("stored during the backoff")
	}
	now = now.Add(clearanceBackoff)
	s.remember("https://d.com/", sol("cf_clearance", 0))
	if _, ok := s.lookup("https://d.com/"); !ok {
		t.Fatal("not stored after the backoff")
	}
}
