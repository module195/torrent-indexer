package requester

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/module195/torrent-indexer/cache"
)

// newTestRequester uses a Redis address nobody listens on: every cache read
// misses and every write fails, which GetDocument tolerates.
func newTestRequester(t *testing.T) *Requster {
	t.Helper()
	t.Setenv("REDIS_HOST", "127.0.0.1")
	r := NewRequester(nil, cache.NewRedis(), 5*time.Second)
	// the test sites are httptest servers on 127.0.0.1
	r.AllowPrivateHosts()
	return r
}

const okPage = "<html><body><div class=\"post\">ok</div></body></html>"

func TestGetDocumentStatus(t *testing.T) {
	cases := []struct {
		status  int
		wantErr bool
	}{
		{http.StatusOK, false},
		{http.StatusNotFound, false}, // empty search on some sites
		{http.StatusForbidden, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusServiceUnavailable, true},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, okPage)
			}))
			defer srv.Close()

			rc, err := newTestRequester(t).GetDocument(context.Background(), srv.URL)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("status %d: unexpected error %v", tc.status, err)
				}
				rc.Close()
				return
			}
			var se *UpstreamStatusError
			if !errors.As(err, &se) {
				t.Fatalf("status %d: want UpstreamStatusError, got %v", tc.status, err)
			}
			if se.StatusCode != tc.status {
				t.Fatalf("status in error = %d, want %d", se.StatusCode, tc.status)
			}
		})
	}
}
