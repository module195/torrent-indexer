package handler

import (
	"net/http"
	"net/http/httptest"
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

// paramsReq builds the request AddSimilarityCheck and FilterBy read their params from.
func paramsReq(params url.Values) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/indexers/x?"+params.Encode(), nil)
}

func sortedTitles(torrents []schema.IndexedTorrent) []string {
	titles := titlesOf(torrents)
	slices.Sort(titles)
	return titles
}

func Test_AddSimilarityCheckThreshold(t *testing.T) {
	cases := []struct {
		q, title, original string
		keep               bool
	}{
		// what the sites' full-text search returned for "Breaking Bad" in prod
		{"Breaking Bad", "Better.Call.Saul.S04E10.1080p.WEB.X264-METCON", "Better Call Saul 4ª Temporada Torrent (2018) Dublado", false},
		{"Breaking Bad", "Os Bad Boys (1995) BDRip 1080p Dublado", "Os Bad Boys", false},
		{"Breaking Bad", "Midwinter Break 2026 1080p WEBRip Dublado", "Midwinter Break", false},
		{"Breaking Bad", "Breaking Bad - 3ª Temporada (2010) 720p Dual Áudio", "Breaking Bad Todas 1ª a 5ª Temporada Completa (2015)", true},
		{"Breaking Bad", "El Camino A Breaking Bad Movie 2019 (1080p WEB-DL)", "El Camino - A Breaking Bad Movie", true},
		// close titles that are not a substring of the query must stay
		{"duna parte 2", "Duna: Parte Dois (2024) 1080p", "", true},
		{"duna parte dois", "Dune.Part.Two.2024.1080p.WEB-DL", "Duna: Parte 2", true},
		{"braking bad", "Breaking Bad 1ª Temporada", "", true},
		{"senhor dos aneis", "O Senhor dos Anéis: A Sociedade do Anel (2001)", "", true},
	}
	for _, c := range cases {
		in := []schema.IndexedTorrent{{Title: c.title, OriginalTitle: c.original}}
		got := AddSimilarityCheck(nil, paramsReq(url.Values{"q": {c.q}}), in)
		if kept := len(got) == 1; kept != c.keep {
			t.Errorf("q=%q title=%q: kept=%v, want %v", c.q, c.title, kept, c.keep)
		}
	}
}

func Test_AddSimilarityCheckEmptyTitleIsNotAMatch(t *testing.T) {
	// "" is a substring of every query, so these used to get the 0.5 title-match boost
	in := []schema.IndexedTorrent{{Title: "", OriginalTitle: ""}, {Title: "1080p WEB-DL"}}
	if got := AddSimilarityCheck(nil, paramsReq(url.Values{"q": {"Breaking Bad"}}), in); len(got) != 0 {
		t.Errorf("got %+v, want nothing", got)
	}
}

func Test_AddSimilarityCheckFilterResultsFalse(t *testing.T) {
	in := []schema.IndexedTorrent{{Title: "Zzz Qqq"}, {Title: "Breaking Bad 1ª Temporada"}}
	got := AddSimilarityCheck(nil, paramsReq(url.Values{"q": {"Breaking Bad"}, "filter_results": {"false"}}), in)
	if titles := titlesOf(got); !slices.Equal(titles, []string{"Breaking Bad 1ª Temporada", "Zzz Qqq"}) {
		t.Errorf("got %v, want everything, best first", titles)
	}
}

func Test_AddSimilarityCheckMinSimilarity(t *testing.T) {
	const bcs, bb = "Better.Call.Saul.S04E10.1080p.WEB.X264-METCON", "Breaking Bad 1ª Temporada"
	cases := map[string][]string{
		"0.05": {bcs, bb},
		"0.9":  {},
		"abc":  {bb}, // invalid: default threshold
		"2":    {bb}, // out of range: default threshold
	}
	for minSim, want := range cases {
		in := []schema.IndexedTorrent{
			{Title: bcs, OriginalTitle: "Better Call Saul 4ª Temporada Torrent (2018) Dublado"},
			{Title: bb},
		}
		got := AddSimilarityCheck(nil, paramsReq(url.Values{"q": {"Breaking Bad"}, "min_similarity": {minSim}}), in)
		if titles := sortedTitles(got); !slices.Equal(titles, want) {
			t.Errorf("min_similarity=%s: got %v, want %v", minSim, titles, want)
		}
	}
}

func Test_FilterByIMDbExactID(t *testing.T) {
	in := []schema.IndexedTorrent{
		{Title: "exact", IMDB: "https://www.imdb.com/title/tt0903747/"},
		{Title: "pt-upper", IMDB: "https://www.imdb.com/pt/title/TT0903747/"},
		{Title: "longer", IMDB: "https://www.imdb.com/title/tt09037471/"},
		{Title: "other", IMDB: "https://www.imdb.com/title/tt3032476/"},
		{Title: "empty"},
	}
	for _, param := range []string{"tt0903747", " TT0903747 ", "https://www.imdb.com/title/tt0903747/", "0903747"} {
		got := FilterBy(nil, paramsReq(url.Values{"imdb": {param}}), slices.Clone(in))
		if titles := titlesOf(got); !slices.Equal(titles, []string{"exact", "pt-upper"}) {
			t.Errorf("imdb=%q: got %v", param, titles)
		}
	}
}

func Test_FilterBySeason(t *testing.T) {
	in := []schema.IndexedTorrent{
		{Title: "Better.Call.Saul.S04E10.1080p.WEB"},
		{Title: "Breaking.Bad.S04.1080p.BluRay"},
		{Title: "Breaking Bad 2011 4ª Temporada Completa"},
		{Title: "Breaking Bad - 3ª Temporada (2010) 720p", OriginalTitle: "Breaking Bad Todas 1ª a 5ª Temporada Completa (2015)"},
		{Title: "Breaking Bad Todas 1ª a 5ª Temporada Completa"},
		{Title: "Breaking Bad S01-S05 Completa"},
		{Title: "Breaking Bad Todas as Temporadas"},
		{Title: "Breaking Bad 5 Temporada PARTE 1 ( 2012)"},
		{Title: "Breaking.Bad.720p.BluRay", OriginalTitle: "Breaking Bad 4a Temporada"},
		{Title: "Breaking_Bad_Season_4_720p"},
		{Title: "El Camino A Breaking Bad Movie 2019"},
		{Title: "Grey's Anatomy 14ª Temporada"},
	}
	want := []string{
		"Better.Call.Saul.S04E10.1080p.WEB",
		"Breaking.Bad.S04.1080p.BluRay",
		"Breaking Bad 2011 4ª Temporada Completa",
		"Breaking Bad Todas 1ª a 5ª Temporada Completa",
		"Breaking Bad S01-S05 Completa",
		"Breaking Bad Todas as Temporadas",
		"Breaking.Bad.720p.BluRay",
		"Breaking_Bad_Season_4_720p",
	}
	got := FilterBy(nil, paramsReq(url.Values{"season": {"4"}}), slices.Clone(in))
	if titles := titlesOf(got); !slices.Equal(titles, want) {
		t.Errorf("season=4:\n got %q\nwant %q", titles, want)
	}

	for _, bad := range []string{"abc", "0", "-1"} {
		if got := FilterBy(nil, paramsReq(url.Values{"season": {bad}}), slices.Clone(in)); len(got) != len(in) {
			t.Errorf("season=%s should be ignored, kept %d of %d", bad, len(got), len(in))
		}
	}
}

func Test_FilterByEpisode(t *testing.T) {
	in := []schema.IndexedTorrent{
		{Title: "Better.Call.Saul.S04E10.1080p"},
		{Title: "Breaking.Bad.S04E09.1080p"},
		{Title: "Breaking.Bad.S04E08-E10.1080p"},
		{Title: "Breaking.Bad.S03E10.1080p"},
		{Title: "Breaking Bad 4ª Temporada Completa"},
		{Title: "Breaking Bad Todas 1ª a 5ª Temporada"},
		{Title: "El Camino A Breaking Bad Movie 2019"},
	}

	// season packs that hold the episode are kept
	got := FilterBy(nil, paramsReq(url.Values{"season": {"4"}, "episode": {"10"}}), slices.Clone(in))
	want := []string{
		"Better.Call.Saul.S04E10.1080p",
		"Breaking.Bad.S04E08-E10.1080p",
		"Breaking Bad 4ª Temporada Completa",
		"Breaking Bad Todas 1ª a 5ª Temporada",
	}
	if titles := titlesOf(got); !slices.Equal(titles, want) {
		t.Errorf("season=4&episode=10:\n got %q\nwant %q", titles, want)
	}

	// without a season only explicit episodes can match
	got = FilterBy(nil, paramsReq(url.Values{"episode": {"10"}}), slices.Clone(in))
	want = []string{"Better.Call.Saul.S04E10.1080p", "Breaking.Bad.S04E08-E10.1080p", "Breaking.Bad.S03E10.1080p"}
	if titles := titlesOf(got); !slices.Equal(titles, want) {
		t.Errorf("episode=10:\n got %q\nwant %q", titles, want)
	}
}

func Test_isMultiEpisodeRelease(t *testing.T) {
	for title, want := range map[string]bool{
		"The.Boys.S04.1080p.WEB-DL":          true,
		"The Boys 4ª Temporada Completa":     true,
		"Breaking Bad S01-S05 Completa":      true,
		"Breaking.Bad.S04E08-E10.1080p":      true,
		"Breaking Bad Todas as Temporadas":   true,
		"The.Boys.S04E05.1080p.WEB-DL":       false,
		"Oppenheimer.2023.1080p.BluRay.x264": false,
	} {
		if got := isMultiEpisodeRelease(title); got != want {
			t.Errorf("isMultiEpisodeRelease(%q) = %v, want %v", title, got, want)
		}
	}
}
