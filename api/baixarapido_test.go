package handler

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"

	"github.com/module195/torrent-indexer/schema"
)

func loadBaixarApidoFixture(t *testing.T, name string) *goquery.Document {
	t.Helper()
	f, err := os.Open("testdata/baixarapido/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	doc, err := goquery.NewDocumentFromReader(f)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func Test_parseBaixarApidoQuery(t *testing.T) {
	cases := []struct {
		q    string
		want baixarApidoQuery
	}{
		{"Oppenheimer 2023", baixarApidoQuery{title: "Oppenheimer", year: "2023"}},
		{"The Last of Us S02E03", baixarApidoQuery{title: "The Last of Us", season: 2, episode: 3}},
		{"The Last of Us S02", baixarApidoQuery{title: "The Last of Us", season: 2}},
		{"The Last of Us temporada 2 episodio 3", baixarApidoQuery{title: "The Last of Us", season: 2, episode: 3}},
		{"Lupin 4ª Temporada", baixarApidoQuery{title: "Lupin", season: 4}},
		{"1917", baixarApidoQuery{title: "1917"}},
		{"Blade Runner 2049", baixarApidoQuery{title: "Blade Runner 2049"}},
		{"Homem Aranha", baixarApidoQuery{title: "Homem Aranha"}},
	}
	for _, c := range cases {
		if got := parseBaixarApidoQuery(c.q); got != c.want {
			t.Errorf("parseBaixarApidoQuery(%q) = %+v, want %+v", c.q, got, c.want)
		}
	}
}

func Test_baixarApidoSearchURL(t *testing.T) {
	base := "https://baixarapido.com"
	cases := []struct{ term, page, want string }{
		{"Homem Aranha", "1", base + "/pesquisa/%22Homem+Aranha%22/"},
		{"Homem Aranha", "2", base + "/pesquisa/%22Homem+Aranha%22/pagina/2/"},
		{"", "1", base + "/"},
		{"", "3", base + "/pagina/3/"},
	}
	for _, c := range cases {
		if got := baixarApidoSearchURL(base, c.term, c.page); got != c.want {
			t.Errorf("baixarApidoSearchURL(%q, %q) = %q, want %q", c.term, c.page, got, c.want)
		}
	}
}

func Test_buildBaixarApidoFallbackQueries(t *testing.T) {
	got := buildBaixarApidoFallbackQueries("O Senhor dos Anéis")
	if len(got) == 0 || got[0] != "Senhor dos Anéis" {
		t.Errorf("fallback = %q", got)
	}
	got = buildBaixarApidoFallbackQueries("Homem-Aranha: Longe de Casa")
	if len(got) != 1 || got[0] != "Homem Aranha Longe de Casa" {
		t.Errorf("fallback = %q", got)
	}
}

func Test_isSubtitleOnlyMagnet(t *testing.T) {
	keep := []string{
		"VAMOSTORRENT.COM..-LEGENDADO-..Oppenheimer.2023.2160p.UHD.Bluray.REMUX.HDR10-GHD",
		"-LEGENDADO-..Oppenheimer (2023) [720p] [YTS.MX]",
		"Oppenheimer 2023 BluRay 1080p x264 DUAL 5.1",
		"Serie.S01E01.1080p.Legendas.Embutidas.mkv",
	}
	for _, dn := range keep {
		if isSubtitleOnlyMagnet(dn) {
			t.Errorf("isSubtitleOnlyMagnet(%q) = true", dn)
		}
	}
	drop := []string{"Oppenheimer.PT-BR.srt", "Legendas Oppenheimer", "Oppenheimer subtitles pack"}
	for _, dn := range drop {
		if !isSubtitleOnlyMagnet(dn) {
			t.Errorf("isSubtitleOnlyMagnet(%q) = false", dn)
		}
	}
}

func Test_parseBaixarApidoPage_oppenheimer(t *testing.T) {
	doc := loadBaixarApidoFixture(t, "opp.html")
	torrents, err := parseBaixarApidoPage(doc, "https://baixarapido.com/baixar/oppenheimer/")
	if err != nil {
		t.Fatal(err)
	}
	if len(torrents) != 10 {
		t.Fatalf("got %d releases, want 10 (LEGENDADO releases are videos)", len(torrents))
	}
	var fourK, dual int
	for _, it := range torrents {
		if it.IMDB != "https://www.imdb.com/title/tt15398776/" {
			t.Errorf("%s: imdb = %q", it.Title, it.IMDB)
		}
		if it.Year != "2023" {
			t.Errorf("%s: year = %q", it.Title, it.Year)
		}
		if is4KRelease(it.Title) {
			fourK++
		}
		hasPT := false
		for _, a := range it.Audio {
			if a.String() == schema.Audio(schema.AudioPortuguese).String() {
				hasPT = true
			}
		}
		if hasPT {
			dual++
		}
		if strings.Contains(it.Title, "999MB") && it.Size != "999 MB" {
			t.Errorf("%s: size = %q, want the 999 MB in its name", it.Title, it.Size)
		}
	}
	if fourK != 3 {
		t.Errorf("got %d 4K releases, want 3", fourK)
	}
	if dual != 3 {
		t.Errorf("got %d releases with Portuguese audio, want 3 (the DUAL ÁUDIO buttons)", dual)
	}
}

func Test_dedupBaixarApidoTorrents_acrossPages(t *testing.T) {
	var all []schema.IndexedTorrent
	for _, p := range []struct{ file, link string }{
		{"opp4k.html", "https://baixarapido.com/baixar/oppenheimer-4k/"},
		{"opp.html", "https://baixarapido.com/baixar/oppenheimer/"},
	} {
		torrents, err := parseBaixarApidoPage(loadBaixarApidoFixture(t, p.file), p.link)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, torrents...)
	}
	got := dedupBaixarApidoTorrents(all)
	if len(got) != 10 {
		t.Fatalf("got %d after dedup, want 10", len(got))
	}
	for _, it := range got {
		if it.Details != "https://baixarapido.com/baixar/oppenheimer/" {
			t.Errorf("%s: details = %q, want the main page", it.Title, it.Details)
		}
	}
}

func Test_extractBaixarApidoEntries_jsonLD(t *testing.T) {
	entries := extractBaixarApidoEntries(loadBaixarApidoFixture(t, "busca-homem.html"))
	if len(entries) != 30 {
		t.Fatalf("got %d entries, want 30", len(entries))
	}
	e := entries[0]
	if e.url != "https://baixarapido.com/baixar/o-homem-eletronico/" || e.year != "1990" || !strings.Contains(e.formats, "1080P") {
		t.Errorf("first entry = %+v", e)
	}
	if home := extractBaixarApidoEntries(loadBaixarApidoFixture(t, "home.html")); len(home) != 30 {
		t.Errorf("home: got %d entries, want 30", len(home))
	}
}

func Test_filterBaixarApidoEntries(t *testing.T) {
	entries := []baixarApidoEntry{
		{url: "https://baixarapido.com/baixar/duna/", year: "2021", formats: "1080p / 4K"},
		{url: "https://baixarapido.com/baixar/duna-1984/", year: "1984", formats: "720p"},
		{url: "https://baixarapido.com/baixar/lupin-3a-temporada/", name: "Lupin - 3ª Temporada"},
		{url: "https://baixarapido.com/baixar/lupin-4a-temporada/", name: "Lupin - 4ª Temporada"},
	}
	got := filterBaixarApidoEntries(entries[:2], baixarApidoQuery{title: "Duna", year: "2021"}, false)
	if len(got) != 1 || got[0].year != "2021" {
		t.Errorf("year filter = %+v", got)
	}
	got = filterBaixarApidoEntries(entries[:2], baixarApidoQuery{title: "Duna"}, true)
	if len(got) != 1 || got[0].year != "2021" {
		t.Errorf("4K filter = %+v", got)
	}
	got = filterBaixarApidoEntries(entries[2:], baixarApidoQuery{title: "Lupin", season: 4}, false)
	if len(got) != 1 || !strings.Contains(got[0].url, "4a-temporada") {
		t.Errorf("season filter = %+v", got)
	}
}

func Test_withSeasonEpisodeParams(t *testing.T) {
	r := httptest.NewRequest("GET", "/indexers/baixarapido?q=The+Last+of+Us+S02E03", nil)
	got := withSeasonEpisodeParams(r, 2, 3).URL.Query()
	if got.Get("season") != "2" || got.Get("episode") != "3" {
		t.Errorf("params = %v", got)
	}
	r = httptest.NewRequest("GET", "/indexers/baixarapido?q=x&season=1", nil)
	if withSeasonEpisodeParams(r, 2, 3) != r {
		t.Errorf("caller's season must win")
	}
}

func Test_releaseHasQuality(t *testing.T) {
	if !releaseHasQuality("Oppenheimer 2023 BluRay 2160p x265", "4k") || !releaseHasQuality("Oppenheimer IMAX 4K HDR", "2160p") {
		t.Error("4K release not matched")
	}
	if releaseHasQuality("Oppenheimer 2023 BluRay 1080p x264", "2160p") {
		t.Error("1080p release matched as 4K")
	}
	if !releaseHasQuality("Oppenheimer.2023.1080p.WEB-DL", "1080p") {
		t.Error("1080p not matched")
	}
}

func Test_parseBaixarApidoPage_originalTitle(t *testing.T) {
	doc := loadBaixarApidoFixture(t, "longe-de-casa-4k.html")
	torrents, err := parseBaixarApidoPage(doc, "https://baixarapido.com/baixar/homem-aranha-longe-de-casa-4k/")
	if err != nil {
		t.Fatal(err)
	}
	if len(torrents) != 7 {
		t.Fatalf("got %d releases, want 7", len(torrents))
	}
	for _, it := range torrents {
		if !strings.HasPrefix(it.OriginalTitle, "Spider-Man: Far from Home / Homem-Aranha") {
			t.Errorf("original title = %q", it.OriginalTitle)
		}
	}
	// a página do Oppenheimer tem o mesmo nome nas duas línguas: nada muda
	opp, _ := parseBaixarApidoPage(loadBaixarApidoFixture(t, "opp.html"), "x")
	if strings.Contains(opp[0].OriginalTitle, " / ") {
		t.Errorf("original title = %q", opp[0].OriginalTitle)
	}
}

func Test_baixarApidoPageTotal(t *testing.T) {
	if got := baixarApidoPageTotal(loadBaixarApidoFixture(t, "busca-homem.html")); got != 16 {
		t.Errorf("page total = %d, want 16", got)
	}
}

func Test_rankBaixarApidoEntries(t *testing.T) {
	entries := []baixarApidoEntry{
		{url: "https://baixarapido.com/baixar/venom/", name: "Venom"},
		{url: "https://baixarapido.com/baixar/homem-aranha-3/", name: "Homem-Aranha 3"},
		{url: "https://baixarapido.com/baixar/lego-marvel/", name: "LEGO Marvel Super-Heróis"},
		{url: "https://baixarapido.com/baixar/homem-aranha-longe-de-casa/"},
	}
	got := rankBaixarApidoEntries(entries, "Homem Aranha")
	want := []string{"homem-aranha-3", "homem-aranha-longe-de-casa", "venom", "lego-marvel"}
	for i, w := range want {
		if !strings.Contains(got[i].url, "/"+w+"/") {
			t.Fatalf("order = %+v", got)
		}
	}
}
