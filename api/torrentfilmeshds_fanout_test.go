package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A detail page with hundreds of link.php anchors (half of them repeated)
// is resolved with a bounded number of fetches, a few at a time. Each anchor
// used to start its own fetch, all at once (security audit 18/11).
func TestTorrentFilmesHDsLinkPHPFanoutIsBounded(t *testing.T) {
	const anchors, distinct = 200, 100
	var inFlight, peak, hits atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/link.php" {
			hits.Add(1)
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			inFlight.Add(-1)
			id, _ := strconv.Atoi(r.URL.Query().Get("id"))
			fmt.Fprintf(w, `<html><body><a href="magnet:?xt=urn:btih:%040d&dn=ep">m</a></body></html>`, id)
			return
		}
		var b strings.Builder
		b.WriteString(`<html><head><title>Serie</title></head><body><div class="titulo">Serie 1ª Temporada (2024)</div>`)
		for k := 0; k < anchors; k++ {
			fmt.Fprintf(&b, `<a href="/link.php?id=%d">S01 1080p Ep.%02d</a>`, k%distinct+1, k%distinct+1)
		}
		b.WriteString(`</body></html>`)
		_, _ = w.Write([]byte(b.String()))
	}))
	defer site.Close()
	i := newTfhdsTestIndexer(t, site)

	// collect and resolve only: the rest of getTorrentsTorrentFilmesHDs
	// scrapes trackers, which fetches the public tracker list from the
	// network and leaves it in process-wide state other tests read
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	doc, err := getDocument(ctx, i, site.URL+"/serie-1a-temporada", torrentFilmesHDs.URL)
	if err != nil {
		t.Fatal(err)
	}
	urls, labels := collectLinkPHP(doc, site.URL)
	if len(urls) != tfhdsMaxLinkPHP || len(labels) != len(urls) {
		t.Fatalf("collected %d urls / %d labels, want %d", len(urls), len(labels), tfhdsMaxLinkPHP)
	}
	resolved := resolveLinkPHPs(ctx, i, urls)
	if got := hits.Load(); got != tfhdsMaxLinkPHP {
		t.Errorf("link.php fetched %d times, want %d (one per distinct anchor followed)", got, tfhdsMaxLinkPHP)
	}
	if got := peak.Load(); got > tfhdsLinkPHPConcurrency {
		t.Errorf("peak concurrent link.php fetches = %d, want at most %d", got, tfhdsLinkPHPConcurrency)
	}
	for k, mag := range resolved {
		if want := fmt.Sprintf("%040d", k+1); !strings.Contains(mag, want) {
			t.Errorf("resolved[%d] = %q, want the magnet of id %d (page order)", k, mag, k+1)
		}
	}
}
