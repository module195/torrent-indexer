package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
			fmt.Fprintf(w, `<html><body><a href="magnet:?xt=urn:btih:%040s&dn=ep">m</a></body></html>`, r.URL.Query().Get("id"))
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	torrents, err := getTorrentsTorrentFilmesHDs(ctx, i, site.URL+"/serie-1a-temporada")
	if err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got > tfhdsMaxLinkPHP {
		t.Errorf("link.php fetched %d times, want at most %d", got, tfhdsMaxLinkPHP)
	}
	if got := peak.Load(); got > tfhdsLinkPHPConcurrency {
		t.Errorf("peak concurrent link.php fetches = %d, want at most %d", got, tfhdsLinkPHPConcurrency)
	}
	if len(torrents) != tfhdsMaxLinkPHP {
		t.Errorf("got %d torrents, want %d (one per distinct anchor followed)", len(torrents), tfhdsMaxLinkPHP)
	}
}
