package handler

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/module195/torrent-indexer/cache"
	"github.com/module195/torrent-indexer/schema"
	goscrape "github.com/module195/torrent-indexer/scrape"
)

// publicTrackersPerTorrent is how many trackers of the ranked public list go
// into each magnet, on top of the site's own: enough for the player or the
// debrid to find peers quickly without bloating the response.
const publicTrackersPerTorrent = 10

// AddPublicTrackers adds the best public trackers to each torrent and puts
// every tracker it knows into the magnet. The sites' magnets often carry few
// or dead trackers, which makes a P2P start (and a debrid download of
// something not cached) wait on DHT alone. Runs after SendToSearchIndexer so
// the extra trackers stay out of the Meilisearch index.
func AddPublicTrackers(i *Indexer, _ *http.Request, torrents []schema.IndexedTorrent) []schema.IndexedTorrent {
	var r *cache.Redis
	if i != nil {
		r = i.redis
	}
	addPublicTrackers(torrents, goscrape.BestTrackers(r, publicTrackersPerTorrent))
	return torrents
}

func addPublicTrackers(torrents []schema.IndexedTorrent, extra []string) {
	for idx := range torrents {
		t := &torrents[idx]
		if t.MagnetLink == "" && t.InfoHash == "" {
			continue
		}
		t.Trackers = unionTrackers(t.Trackers, extra)
		syncMagnetTrackers(t)
	}
}

// unionTrackers joins two tracker lists, ignoring case and a trailing slash
// when comparing.
func unionTrackers(a, b []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, tr := range list {
			tr = strings.TrimSpace(tr)
			key := trackerKey(tr)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, tr)
		}
	}
	return out
}

func trackerKey(tr string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(tr)), "/")
}

// syncMagnetTrackers appends to the magnet the trackers it lacks, keeping
// every other parameter as the site wrote it. Merging copies from several
// sites unions Trackers but keeps the first copy's magnet.
func syncMagnetTrackers(t *schema.IndexedTorrent) {
	if len(t.Trackers) == 0 || !strings.HasPrefix(strings.ToLower(t.MagnetLink), "magnet:?") {
		return
	}
	have := map[string]bool{}
	for _, part := range strings.Split(t.MagnetLink[len("magnet:?"):], "&") {
		name, value, ok := strings.Cut(part, "=")
		if !ok || !strings.EqualFold(name, "tr") {
			continue
		}
		if decoded, err := url.QueryUnescape(value); err == nil {
			value = decoded
		}
		have[trackerKey(value)] = true
	}
	var b strings.Builder
	b.WriteString(t.MagnetLink)
	for _, tr := range t.Trackers {
		if key := trackerKey(tr); key != "" && !have[key] {
			have[key] = true
			b.WriteString("&tr=")
			b.WriteString(url.QueryEscape(tr))
		}
	}
	t.MagnetLink = b.String()
}
