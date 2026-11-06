package requester

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// A remote server must not size an allocation with the Content-Length it
// declares: before the cap, 2^50 made buf.Grow panic and end the process.
func TestGetDocumentRejectsHugeDeclaredLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", strconv.FormatInt(1<<50, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okPage))
	}))
	defer srv.Close()

	rc, err := newTestRequester(t).GetDocument(context.Background(), srv.URL)
	if err == nil {
		rc.Close()
		t.Fatal("want an error for a declared length past the cap")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A body without Content-Length (chunked, or compressed and expanding) is
// cut at the cap instead of growing the buffer until memory runs out.
func TestGetDocumentRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		chunk := []byte(strings.Repeat("<p>x</p>", 8192))
		for written := 0; written <= maxDocumentBody; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	rc, err := newTestRequester(t).GetDocument(context.Background(), srv.URL)
	if err == nil {
		rc.Close()
		t.Fatal("want an error for a body past the cap")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("unexpected error: %v", err)
	}
}
