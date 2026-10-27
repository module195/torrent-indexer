package handler

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func loadRedeTorrentDoc(t *testing.T, name string) *goquery.Document {
	t.Helper()
	f, err := os.Open("testdata/rede_torrent/" + name)
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

func Test_extractRedeTorrentLinks_onlyCards(t *testing.T) {
	// the domain has "torrent" in it: the menu (home, /filmes/, /qualidade/4k/)
	// must not be taken for title pages
	for _, name := range []string{"home.html", "pagina-2.html", "busca-homem.html"} {
		links := extractRedeTorrentLinks(loadRedeTorrentDoc(t, name))
		if len(links) != 20 {
			t.Errorf("%s: got %d links, want 20", name, len(links))
		}
		for _, l := range links {
			if !strings.HasSuffix(l, "-download/") {
				t.Errorf("%s: not a title page: %q", name, l)
			}
		}
	}
	if links := extractRedeTorrentLinks(loadRedeTorrentDoc(t, "home.html")); links[0] != "https://redestorrents.com/moana-torrent-dublado-dual-audio-legendado-download/" {
		t.Errorf("first link %q", links[0])
	}
	// a title page lists other titles, but they are not results
	if links := extractRedeTorrentLinks(loadRedeTorrentDoc(t, "moana-2026.html")); len(links) != 0 {
		t.Errorf("got %d links from a title page: %v", len(links), links)
	}
}

func Test_redeTorrentListingURL(t *testing.T) {
	m := IndexerMeta{URL: "https://redestorrents.com/", SearchURL: "index.php?busca=", PagePattern: "pagina/%s/"}
	cases := []struct{ q, page, want string }{
		{"", "", "https://redestorrents.com/"},
		{"", "3", "https://redestorrents.com/pagina/3/"},
		{"homem aranha", "", "https://redestorrents.com/index.php?busca=homem+aranha"},
		{"homem aranha", "1", "https://redestorrents.com/index.php?busca=homem+aranha"},
		{"Homem Aranha", "2", "https://redestorrents.com/pesquisa/homem+aranha/pagina/2/"},
	}
	for _, c := range cases {
		if got := redeTorrentListingURL(m, c.q, c.page); got != c.want {
			t.Errorf("q=%q page=%q: got %q, want %q", c.q, c.page, got, c.want)
		}
	}
}

func redeAudioNames(r redeTorrentRelease) []string {
	var names []string
	for _, a := range r.Audio {
		names = append(names, a.String())
	}
	return names
}

func Test_parseRedeTorrentPage_movieWith4K(t *testing.T) {
	page, rels := parseRedeTorrentPage(loadRedeTorrentDoc(t, "moana-2026.html"))
	if page.Title != "Moana" || page.Year != "2026" || page.IMDB != "tt27419466" || page.Series {
		t.Errorf("page: %+v", page)
	}
	if page.Date.IsZero() {
		t.Error("no date")
	}
	want := []struct{ hash, title, size string }{
		{"e3a794aa2254df00ee3451791db66bda2d57234d", "Moana 2026 1080p WEB-DL x264 DUAL 5 1", "2.67 GB"},
		{"b25146f014c9dc256ba5baca663897447db7e914", "Moana 2026 1080p FULL HD WEB-DL DUAL 5 1", "6.21 GB"},
		{"943c3c4c9bdd2479ae9947525950d0453dbddfb1", "Moana 2026 WEB-DL 2160p x265 DV HDR10 DUAL 5 1", "12.20 GB"},
	}
	if len(rels) != len(want) {
		t.Fatalf("got %d releases, want %d", len(rels), len(want))
	}
	for k, w := range want {
		r := rels[k]
		if r.InfoHash != w.hash || r.Title != w.title || r.Size != w.size {
			t.Errorf("#%d: got %q %q %q, want %q %q %q", k, r.InfoHash, r.Title, r.Size, w.hash, w.title, w.size)
		}
		if !slices.Equal(redeAudioNames(r), []string{"brazilian", "eng"}) {
			t.Errorf("#%d: audio %v", k, redeAudioNames(r))
		}
	}
}

func Test_parseRedeTorrentPage_seasonPack(t *testing.T) {
	page, rels := parseRedeTorrentPage(loadRedeTorrentDoc(t, "lupin-4a-temporada.html"))
	if page.Title != "Lupin - 4ª Temporada" || !page.Series || page.IMDB != "" {
		t.Errorf("page: %+v", page)
	}
	if len(rels) != 1 {
		t.Fatalf("got %d releases", len(rels))
	}
	// the "TEMPORADA-04" tag of the BaixaRápido dn no longer leads the name
	if r := rels[0]; r.Title != "Lupin S04 1080p x265 WEB-DL DUAL 5 1" || r.Size != "6.37 GB" {
		t.Errorf("release: %q %q", r.Title, r.Size)
	}
	if got := redeAudioNames(rels[0]); !slices.Equal(got, []string{"brazilian", "fra"}) {
		t.Errorf("audio %v", got)
	}
	// series: original title without the season, and no year (it is the season's)
	if title, year := redeTorrentIMDBSearch(page); title != "Lupin" || year != "" {
		t.Errorf("imdb search: %q %q", title, year)
	}
}

func Test_parseRedeTorrentPage_gluedEpisode(t *testing.T) {
	page, rels := parseRedeTorrentPage(loadRedeTorrentDoc(t, "onde-esta-a-wanda-2a-temporada.html"))
	if len(rels) != 1 {
		t.Fatalf("got %d releases", len(rels))
	}
	// dn "OndeEstaWandaS02E01WEB-DL1080px264DUAL5.1": page title + episode + label tags
	if r := rels[0]; r.Title != "Onde Está a Wanda? S02E01 WEB-DL 1080P" || r.Size == "" {
		t.Errorf("release: %q %q", r.Title, r.Size)
	}
	if title, _ := redeTorrentIMDBSearch(page); title != "Wheres Wanda?" {
		t.Errorf("imdb search title %q", title)
	}
}

func Test_parseRedeTorrentPage_collection(t *testing.T) {
	page, rels := parseRedeTorrentPage(loadRedeTorrentDoc(t, "os-bad-boys-colecao.html"))
	if page.Year != "1995" || page.IMDB != "tt0112442" {
		t.Errorf("page: %+v", page)
	}
	if len(rels) != 1 || rels[0].InfoHash != "7bbe26520d8274adf28e1631fa5d3b9264459577" || len(rels[0].Trackers) < 10 {
		t.Fatalf("releases: %+v", rels)
	}
	if title, year := redeTorrentIMDBSearch(page); title != "Bad Boys Collection" || year != "1995" {
		t.Errorf("imdb search: %q %q", title, year)
	}
}

func Test_stripBaixarApidoTagBlock(t *testing.T) {
	in := "BAIXARAPIDO.COM-.TEMPORADA-04.WEB-DL.1080P.MKV.5.1.-DUBLADO-DUAL-AUDIO-.Lupin.S04.1080p.x265.WEB-DL.DUAL.5.1"
	for name, fn := range map[string]func(string) string{"rede": cleanRedeTorrentTitle, "baixarapido": cleanBaixarApidoTitle} {
		if got := fn(in); got != "Lupin S04 1080p x265 WEB-DL DUAL 5 1" {
			t.Errorf("%s: got %q", name, got)
		}
	}
	// without the block the name is left alone
	if got := stripBaixarApidoTagBlock("Bad.Boys.Coleção.1995-2024.1080p.BluRay.x264.DUAL-STARCKFILMES"); got != "Bad.Boys.Coleção.1995-2024.1080p.BluRay.x264.DUAL-STARCKFILMES" {
		t.Errorf("got %q", got)
	}
}
