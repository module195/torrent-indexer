package handler

import (
	"encoding/json"
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

func Test_needsFileList(t *testing.T) {
	cases := []struct {
		name   string
		it     schema.IndexedTorrent
		series bool
		want   bool
	}{
		{"named pack", schema.IndexedTorrent{Title: "The.Boys.S04.1080p.WEB-DL"}, false, true},
		{"bare release name on a season page", schema.IndexedTorrent{Title: "The.Boys.1080p.WEB-DL", OriginalTitle: "The Boys 4ª Temporada Completa"}, false, true},
		{"single episode on a season page", schema.IndexedTorrent{Title: "The.Boys.S04E05.1080p", OriginalTitle: "The Boys 4ª Temporada"}, false, false},
		{"movie", schema.IndexedTorrent{Title: "Oppenheimer.2023.1080p", OriginalTitle: "Oppenheimer (2023)"}, false, false},
		{"any release on a series request", schema.IndexedTorrent{Title: "The.Boys.S04E05.1080p"}, true, true},
		{"already has files", schema.IndexedTorrent{Title: "The.Boys.S04.1080p", Files: []schema.File{{Path: "a.mkv"}}}, true, false},
	}
	for _, c := range cases {
		if got := needsFileList(c.it, c.series); got != c.want {
			t.Errorf("%s: needsFileList = %v, want %v", c.name, got, c.want)
		}
	}
}

func Test_FilterByEpisodeGluedAndNamelessReleases(t *testing.T) {
	page := "https://site/the-boys-4-temporada/"
	in := []schema.IndexedTorrent{
		{Title: "TheBoysS04E05WEB-DL1080pDUAL", Details: page},
		{Title: "TheBoysS04E08WEB-DL1080pDUAL", Details: page},
		// magnet without dn: the title fell back to the page title
		{Title: "The Boys 4ª Temporada", OriginalTitle: "The Boys 4ª Temporada (brazilian)", Details: page},
		// a named season pack on the same page is still a pack
		{Title: "The.Boys.S04.1080p.WEB-DL", OriginalTitle: "The Boys 4ª Temporada (brazilian)", Details: page},
		// a real pack from another page keeps matching through its page title
		{Title: "The.Boys.Complete.1080p", OriginalTitle: "The Boys 4ª Temporada Completa", Details: "https://site/pack/"},
	}
	got := FilterBy(nil, paramsReq(url.Values{"season": {"4"}, "episode": {"5"}}), slices.Clone(in))
	want := []string{"TheBoysS04E05WEB-DL1080pDUAL", "The.Boys.S04.1080p.WEB-DL", "The.Boys.Complete.1080p"}
	if titles := titlesOf(got); !slices.Equal(titles, want) {
		t.Errorf("season=4&episode=5:\n got %q\nwant %q", titles, want)
	}
}

func Test_AddSimilarityCheckKeepsSequelAndPollutedOriginalTitle(t *testing.T) {
	in := []schema.IndexedTorrent{
		// bludv_ru: original_title is the whole post header
		{Title: "Duna.Parte.2.2024.1080p.WEB-DL.DUAL.5.1", OriginalTitle: "Duna: Parte Dois\n24 de outubro de 2026\nWEB-DL 1080p Dual Áudio (brazilian, eng)"},
		// rede_torrent: release named in english, audio suffix appended
		{Title: "Dune Part Two 2024 1080p WEBRip x264 (brazilian, eng)", OriginalTitle: "Duna - Parte Dois (brazilian, eng)"},
		{Title: "Duna Parte 2 2024 2160p (brazilian)", OriginalTitle: "Duna Parte 2"},
	}
	got := AddSimilarityCheck(nil, paramsReq(url.Values{"q": {"duna parte dois"}}), slices.Clone(in))
	if len(got) != len(in) {
		t.Fatalf("kept %d of %d: %v", len(got), len(in), titlesOf(got))
	}
}

func Test_normalizeSequelNumbers(t *testing.T) {
	for in, want := range map[string]string{
		"Duna: Parte Dois":      "duna parte 2",
		"Dune Part Two":         "dune part 2",
		"Duna - Parte  Dois":    "duna parte 2",
		"um sonho de liberdade": "um sonho de liberdade",
	} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func Test_FilterByAudioAliases(t *testing.T) {
	in := []schema.IndexedTorrent{
		{Title: "dublado", Audio: []schema.Audio{schema.AudioPortuguese, schema.AudioEnglish}},
		{Title: "legendado", Audio: []schema.Audio{schema.AudioEnglish}},
	}
	for _, v := range []string{"por", "pt-br", "PT", "brazilian", "Português"} {
		got := FilterBy(nil, paramsReq(url.Values{"audio": {v}}), slices.Clone(in))
		if titles := titlesOf(got); !slices.Equal(titles, []string{"dublado"}) {
			t.Errorf("audio=%s: got %v", v, titles)
		}
	}
	got := FilterBy(nil, paramsReq(url.Values{"audio": {"en"}}), slices.Clone(in))
	if titles := titlesOf(got); !slices.Equal(titles, []string{"dublado", "legendado"}) {
		t.Errorf("audio=en: got %v", titles)
	}
}

func Test_PropagateIMDB(t *testing.T) {
	const imdb = "https://www.imdb.com/title/tt0458339/"
	torrents := []schema.IndexedTorrent{
		{Title: "Capitão América - O Primeiro Vingador 4K", OriginalTitle: "Capitão América - O Primeiro Vingador 4K", Year: "2011", IMDB: imdb},
		{Title: "x", OriginalTitle: "Capitão América: O Primeiro Vingador – BluRay 3D HSBS (2011) Dublado Torrent", Year: "2011"},
		{Title: "Capitão América (2011) BluRay 1080p Dublado 5.1 (brazilian, eng)", Year: "2011"},
		{Title: "Capitão América 2 - O Soldado Invernal", Year: "2014"},
		{Title: "Capitão América - O Primeiro Vingador", Year: ""},
	}
	got := PropagateIMDB(nil, nil, torrents)
	if got[1].IMDB != imdb {
		t.Errorf("same work from another site: got %q", got[1].IMDB)
	}
	for _, i := range []int{2, 3, 4} {
		if got[i].IMDB != "" {
			t.Errorf("result %d (%q) got IMDb %q, want none", i, got[i].Title, got[i].IMDB)
		}
	}
}

func Test_PropagateIMDBAmbiguous(t *testing.T) {
	torrents := []schema.IndexedTorrent{
		{Title: "Duna", Year: "2021", IMDB: "tt1160419"},
		{Title: "Duna", Year: "2021", IMDB: "tt9999999"},
		{Title: "Duna 1080p", Year: "2021"},
	}
	if got := PropagateIMDB(nil, nil, torrents); got[2].IMDB != "" {
		t.Errorf("ambiguous key filled with %q", got[2].IMDB)
	}
}

func Test_CleanupTitleWebsitesUnescapesHTML(t *testing.T) {
	torrents := []schema.IndexedTorrent{{Title: "Capit&atilde;o Am&eacute;rica (2011)", OriginalTitle: "Capit&atilde;o Am&eacute;rica"}}
	got := CleanupTitleWebsites(nil, nil, torrents)
	if got[0].Title != "Capitão América (2011)" || got[0].OriginalTitle != "Capitão América" {
		t.Errorf("got %q / %q", got[0].Title, got[0].OriginalTitle)
	}
}

func Test_zeroDateOmitted(t *testing.T) {
	b, err := json.Marshal(schema.IndexedTorrent{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"date"`) {
		t.Errorf("zero date serialized: %s", b)
	}
}

func Test_splitGluedRelease(t *testing.T) {
	cases := map[string]string{
		"-TedLassoS01E01-05720p":                 "Ted.Lasso.S01E01-E05.720p",
		"-TedLassoS01E06-071080pDUAL":            "Ted.Lasso.S01E06-E07.1080pDUAL",
		"-TedLassoS01E091080pDUAL":               "Ted.Lasso.S01E09.1080pDUAL",
		"TedLassoS03E111080pWEB-DLDUAL5.1":       "Ted.Lasso.S03E11.1080pWEB-DLDUAL5.1",
		"TedLassoS04E07WEB-DL1080px264DUAL5.1":   "Ted.Lasso.S04E07.WEB-DL1080px264DUAL5.1",
		"Ted.Lasso.S04E04.1080p.WEB-DL.DUAL.5.1": "Ted.Lasso.S04E04.1080p.WEB-DL.DUAL.5.1",
		"The.Boys.S04E08-E10.1080p":              "The.Boys.S04E08-E10.1080p",
		"Ted Lasso 4ª Temporada (2026)":          "Ted Lasso 4ª Temporada (2026)",
		"Duna Parte Dois 2024 1080p":             "Duna Parte Dois 2024 1080p",
	}
	for in, want := range cases {
		if got := splitGluedRelease(in); got != want {
			t.Errorf("splitGluedRelease(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string][2]int{
		"Ted.Lasso.S01E01-E05.720p": {1, 5},
		"Ted.Lasso.S03E11.1080pWEB": {11, 11},
	} {
		e := parseReleaseInfo(in).episodes
		if len(e) != 1 || e[0].from != want[0] || e[0].to != want[1] {
			t.Errorf("parseReleaseInfo(%q) = %+v", in, e)
		}
	}
}
