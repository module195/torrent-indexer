package handler

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/module195/torrent-indexer/schema"
)

func Test_torrentComparatorSeedTieBreak(t *testing.T) {
	few := schema.IndexedTorrent{Title: "few", Similarity: 0.5, SeedCount: 2}
	many := schema.IndexedTorrent{Title: "many", Similarity: 0.5, SeedCount: 9}
	best := schema.IndexedTorrent{Title: "best", Similarity: 0.9}

	for _, dir := range []string{"desc", "asc"} {
		cmp := torrentComparator(url.Values{"sortBy": {"similarity"}, "sortDirection": {dir}})
		if cmp(many, few) >= 0 {
			t.Errorf("%s: more seeds should win a similarity tie", dir)
		}
	}

	list := []schema.IndexedTorrent{few, best, many}
	slices.SortStableFunc(list, torrentComparator(url.Values{"q": {"x"}}))
	if got := titlesOf(list); !slices.Equal(got, []string{"best", "many", "few"}) {
		t.Errorf("default search order %v", got)
	}

	// sorting by seeds itself must keep the requested direction
	if torrentComparator(url.Values{"sortBy": {"seed_count"}, "sortDirection": {"asc"}})(few, many) >= 0 {
		t.Error("seed_count asc should put fewer seeds first")
	}
}

func Test_DedupByInfoHash(t *testing.T) {
	const hash = "dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c"
	in := []schema.IndexedTorrent{
		{Title: "first", InfoHash: strings.ToUpper(hash), SeedCount: 3},
		{Title: "second", InfoHash: hash, SeedCount: 10, Size: "1.4 GB"},
		{Title: "magnet-only", MagnetLink: "magnet:?xt=urn:btih:" + strings.ToUpper(hash) + "&dn=x", SeedCount: 1},
		{Title: "keyless"},
		{Title: "keyless"},
	}

	got := DedupByInfoHash(nil, nil, in)

	if titles := titlesOf(got); !slices.Equal(titles, []string{"first", "keyless", "keyless"}) {
		t.Fatalf("got %v", titles)
	}
	if got[0].SeedCount != 10 || got[0].Size != "1.4 GB" {
		t.Errorf("merged copy %+v, want max seeds and the filled size", got[0])
	}
}

func Test_multiHandlerSeedTieBreak(t *testing.T) {
	h := NewMultiHandler(map[string]http.HandlerFunc{
		"rede_torrent": fakeIndexer(schema.IndexedTorrent{Title: "few", InfoHash: "aaaa", SeedCount: 2, Similarity: 0.5}),
		"bludv_ru":     fakeIndexer(schema.IndexedTorrent{Title: "many", InfoHash: "bbbb", SeedCount: 9, Similarity: 0.5}),
	})

	rec, p := runMulti(t, h, "/indexers/multi?q=matrix&indexers=rede_torrent,bludv_ru")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var hashes []string
	for _, r := range p.Results {
		hashes = append(hashes, r.InfoHash)
	}
	if !slices.Equal(hashes, []string{"bbbb", "aaaa"}) {
		t.Errorf("got %v, want the 9-seed torrent first", hashes)
	}
}
