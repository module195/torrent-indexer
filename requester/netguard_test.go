package requester

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/module195/torrent-indexer/cache"
)

func TestIsPublicAddr(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8":          true,
		"104.21.3.4":       true,
		"2606:4700::1111":  true,
		"127.0.0.1":        false,
		"10.1.2.3":         false,
		"172.16.0.1":       false,
		"192.168.1.1":      false,
		"169.254.169.254":  false, // cloud metadata
		"100.64.0.1":       false,
		"0.0.0.0":          false,
		"255.255.255.255":  false,
		"224.0.0.1":        false,
		"::1":              false,
		"fe80::1":          false,
		"fd00::1":          false,
		"::ffff:127.0.0.1": false, // IPv4-mapped loopback
		"::ffff:10.0.0.1":  false,
	}
	for in, want := range cases {
		if got := isPublicAddr(netip.MustParseAddr(in)); got != want {
			t.Errorf("isPublicAddr(%s) = %v, want %v", in, got, want)
		}
	}
}

// guardedRequester keeps the destination checks on, as in production.
func guardedRequester(t *testing.T) *Requster {
	t.Helper()
	t.Setenv("REDIS_HOST", "127.0.0.1")
	t.Setenv("REQUESTER_ALLOW_PRIVATE_HOSTS", "")
	return NewRequester(nil, cache.NewRedis(), 5*time.Second)
}

// A scraped link to an internal host is refused before any request is
// sent, so neither the plain client nor FlareSolverr reaches it.
func TestGetDocumentRefusesInternalHost(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(okPage))
	}))
	defer srv.Close()

	r := guardedRequester(t)
	for _, u := range []string{
		srv.URL + "/secret",
		"http://localhost:6379/",
		"http://LOCALHOST./",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:7700/keys",
		"file:///etc/passwd",
		"gopher://127.0.0.1:6379/_FLUSHALL",
		"http:///no-host",
	} {
		_, err := r.GetDocument(context.Background(), u)
		if !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("GetDocument(%q) error = %v, want ErrBlockedDestination", u, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("internal server received %d requests, want 0", n)
	}
}

// The dial-time check is what stops a redirect, or a DNS answer that changed
// after checkDestination, from reaching an internal address.
func TestDialControlRefusesInternalAddress(t *testing.T) {
	r := guardedRequester(t)
	if err := r.dialControl("tcp", "127.0.0.1:6379", nil); !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("dialControl(127.0.0.1) = %v, want ErrBlockedDestination", err)
	}
	if err := r.dialControl("tcp", "[fd00::5]:443", nil); !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("dialControl(fd00::5) = %v, want ErrBlockedDestination", err)
	}
	if err := r.dialControl("tcp", "8.8.8.8:443", nil); err != nil {
		t.Fatalf("dialControl(8.8.8.8) = %v, want nil", err)
	}
}

// A redirect to a non-http scheme ends the fetch with ErrBlockedDestination
// and no FlareSolverr fallback (the requester here has none: a fallback
// would fail with another error).
func TestGetDocumentRefusesRedirectToOtherScheme(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	}))
	defer srv.Close()

	r := newTestRequester(t) // the test server itself is on 127.0.0.1
	_, err := r.GetDocument(context.Background(), srv.URL)
	if !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("GetDocument error = %v, want ErrBlockedDestination", err)
	}
}

func TestCheckRedirectLimit(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	via := make([]*http.Request, maxRedirects)
	if err := checkRedirect(req, via); err == nil {
		t.Fatal("checkRedirect after the limit = nil, want an error")
	}
	if err := checkRedirect(req, via[:1]); err != nil {
		t.Fatalf("checkRedirect = %v, want nil", err)
	}
}

func TestAllowPrivateHostsFromEnv(t *testing.T) {
	t.Setenv("REDIS_HOST", "127.0.0.1")
	t.Setenv("REQUESTER_ALLOW_PRIVATE_HOSTS", "true")
	r := NewRequester(nil, cache.NewRedis(), time.Second)
	if err := r.checkDestination(context.Background(), "http://127.0.0.1:8080/"); err != nil {
		t.Fatalf("checkDestination with the opt-out = %v, want nil", err)
	}
	if err := r.checkDestination(context.Background(), "file:///etc/passwd"); !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("checkDestination(file://) with the opt-out = %v, want ErrBlockedDestination", err)
	}
}
