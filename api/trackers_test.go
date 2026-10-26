package handler

import (
	"strings"
	"testing"

	"github.com/module195/torrent-indexer/schema"
	goscrape "github.com/module195/torrent-indexer/scrape"
)

const testHash = "0123456789abcdef0123456789abcdef01234567"

func Test_addPublicTrackersPutsThemInTheMagnet(t *testing.T) {
	torrents := []schema.IndexedTorrent{{
		InfoHash:   testHash,
		MagnetLink: "magnet:?xt=urn:btih:" + testHash + "&dn=Filme&tr=udp%3A%2F%2Ftracker.site.org%3A80%2Fannounce",
		Trackers:   []string{"udp://tracker.site.org:80/announce"},
	}}
	addPublicTrackers(torrents, []string{"udp://tracker.opentrackr.org:1337/announce", "UDP://tracker.site.org:80/announce/"})

	got := torrents[0]
	if len(got.Trackers) != 2 {
		t.Fatalf("trackers = %v, want the site's plus opentrackr, without the repeated one", got.Trackers)
	}
	want := "magnet:?xt=urn:btih:" + testHash + "&dn=Filme&tr=udp%3A%2F%2Ftracker.site.org%3A80%2Fannounce" +
		"&tr=udp%3A%2F%2Ftracker.opentrackr.org%3A1337%2Fannounce"
	if got.MagnetLink != want {
		t.Fatalf("magnet = %s\nwant      %s", got.MagnetLink, want)
	}
}

func Test_addPublicTrackersSkipsTorrentsWithoutMagnetOrHash(t *testing.T) {
	torrents := []schema.IndexedTorrent{{Title: "sem magnet"}}
	addPublicTrackers(torrents, []string{"udp://tracker.opentrackr.org:1337/announce"})
	if len(torrents[0].Trackers) != 0 {
		t.Fatalf("trackers = %v, want none", torrents[0].Trackers)
	}
}

func Test_syncMagnetTrackersLeavesBrokenMagnetAlone(t *testing.T) {
	it := schema.IndexedTorrent{MagnetLink: "http://site/download/1", Trackers: []string{"udp://a:1/announce"}}
	syncMagnetTrackers(&it)
	if it.MagnetLink != "http://site/download/1" {
		t.Fatalf("magnet = %s", it.MagnetLink)
	}
}

func Test_mergeMultiResultsPutsEveryCopysTrackersInTheMagnet(t *testing.T) {
	calls := []multiCall{{Indexer: "a"}, {Indexer: "b"}}
	results := map[int][]schema.IndexedTorrent{
		0: {{InfoHash: testHash, Title: "Filme", MagnetLink: "magnet:?xt=urn:btih:" + testHash + "&tr=udp%3A%2F%2Fa%3A1%2Fannounce", Trackers: []string{"udp://a:1/announce"}}},
		1: {{InfoHash: testHash, Title: "Filme", MagnetLink: "magnet:?xt=urn:btih:" + testHash + "&tr=udp%3A%2F%2Fb%3A2%2Fannounce", Trackers: []string{"udp://b:2/announce"}}},
	}
	out := mergeMultiResults(calls, results)
	if len(out) != 1 {
		t.Fatalf("got %d torrents, want 1", len(out))
	}
	if !strings.Contains(out[0].MagnetLink, "tr=udp%3A%2F%2Fb%3A2%2Fannounce") {
		t.Fatalf("magnet lacks the second copy's tracker: %s", out[0].MagnetLink)
	}
}

func Test_BestTrackersFallsBackToStaticWithoutBlocking(t *testing.T) {
	got := goscrape.BestTrackers(nil, 3)
	if len(got) != 3 {
		t.Fatalf("got %v, want 3 trackers", got)
	}
	for _, tr := range got {
		if !strings.HasPrefix(tr, "udp://") && !strings.HasPrefix(tr, "http") {
			t.Fatalf("unexpected tracker %q", tr)
		}
	}
}
