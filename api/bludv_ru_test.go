package handler

import (
	"os"
	"slices"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func loadBludvRuPage(t *testing.T, name string) (bludvRuPage, []bludvRuRelease) {
	t.Helper()
	f, err := os.Open("testdata/bludv_ru/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	doc, err := goquery.NewDocumentFromReader(f)
	if err != nil {
		t.Fatal(err)
	}
	return parseBludvRuPage(doc)
}

func audioNames(rel bludvRuRelease) []string {
	var out []string
	for _, a := range rel.Audio {
		out = append(out, a.String())
	}
	return out
}

func Test_parseBludvRuPage_dubbedAndSubtitledSections(t *testing.T) {
	page, rels := loadBludvRuPage(t, "mortal-kombat-2-2026.html")
	if page.IMDB != "https://www.imdb.com/title/tt17490712/" || page.Year != "2026" {
		t.Errorf("page: %+v", page)
	}
	want := []struct {
		title, size string
		audio       []string
	}{
		{"Mortal Kombat 2 2026 WEB-DL 1080p x264 DUAL 5.1", "2.75 GB", []string{"brazilian", "eng"}},
		{"Mortal.Kombat.2.2026.1080p.FULL.HD.WEB-DL.DUAL.5.1", "7.22 GB", []string{"brazilian", "eng"}},
		{"Mortal Kombat II (2026) [720p] [YTS.BZ] WEB-DL", "793 MB", []string{"eng"}},
		{"Mortal Kombat II (2026) [1080p] [YTS.BZ] WEB-DL", "1.76 GB", []string{"eng"}},
		{"Mortal Kombat II (2026) [1080p] [YTS.BZ] WEB-DL x265", "1.58 GB", []string{"eng"}},
		{"Mortal Kombat II (2026) DCPRiP Better Audio.mkv WEB-DL 1080p", "9.11 GB", []string{"eng"}},
	}
	if len(rels) != len(want) {
		t.Fatalf("got %d releases", len(rels))
	}
	for k, w := range want {
		r := rels[k]
		if r.Title != w.title || r.Size != w.size || !slices.Equal(audioNames(r), w.audio) {
			t.Errorf("#%d: got %q %q %v, want %q %q %v", k, r.Title, r.Size, audioNames(r), w.title, w.size, w.audio)
		}
	}
}

func Test_parseBludvRuPage_movieWith4K(t *testing.T) {
	page, rels := loadBludvRuPage(t, "homem-aranha-um-novo-dia-2026.html")
	if page.IMDB != "https://www.imdb.com/title/tt22084616/" {
		t.Errorf("imdb %q", page.IMDB)
	}
	sizes := []string{"3.78 GB", "10.03 GB", "25.95 GB"} // the 4K one has xl=
	if len(rels) != 3 {
		t.Fatalf("got %d releases", len(rels))
	}
	for k, r := range rels {
		if r.Size != sizes[k] || !slices.Equal(audioNames(r), []string{"brazilian", "eng"}) {
			t.Errorf("#%d: %q %q %v", k, r.Title, r.Size, audioNames(r))
		}
	}
	if rels[2].Title != "Homem-Aranha - Um Novo Dia 2026 WEB-DL 2160p x265 DV HDR10+ DUAL 5.1" {
		t.Errorf("4K title %q", rels[2].Title)
	}
}

func Test_parseBludvRuPage_episodes(t *testing.T) {
	_, rels := loadBludvRuPage(t, "the-boys-5a-temporada.html")
	if len(rels) != 7 {
		t.Fatalf("got %d releases", len(rels))
	}
	for _, r := range rels {
		// one "Tamanho: 4 GB" for seven magnets says nothing about each one
		if r.Size != "" || !slices.Equal(audioNames(r), []string{"brazilian", "eng"}) || len(r.Trackers) != 2 {
			t.Errorf("%q: size %q audio %v trackers %d", r.Title, r.Size, audioNames(r), len(r.Trackers))
		}
	}
}

func Test_withLabelTags(t *testing.T) {
	cases := []struct{ title, label, want string }{
		{"Movie [1080p] [YTS.BZ]", "SERVIDOR PARA DOWNLOAD WEB-DL 1080p x265 (1.58 GB)", "Movie [1080p] [YTS.BZ] WEB-DL x265"},
		{"Movie 2026 2160p x265 DV HDR10+", "WEB-DL 2160p 4K DV HDR10+ (27.8 GB)", "Movie 2026 2160p x265 DV HDR10+ WEB-DL"},
		{"The.Boys.S05E03.1080p.WEB-DL", "Episódio 03: 1080p", "The.Boys.S05E03.1080p.WEB-DL"},
	}
	for _, c := range cases {
		if got := withLabelTags(c.title, c.label); got != c.want {
			t.Errorf("withLabelTags(%q, %q) = %q, want %q", c.title, c.label, got, c.want)
		}
	}
}
