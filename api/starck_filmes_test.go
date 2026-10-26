package handler

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
)

func loadStarckFilmesDoc(t *testing.T, name string) *goquery.Document {
	t.Helper()
	f, err := os.Open("testdata/starck_filmes/" + name)
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

func Test_extractStarckFilmesLinks_home(t *testing.T) {
	links := extractStarckFilmesLinks(loadStarckFilmesDoc(t, "home.html"))
	if len(links) != 32 {
		t.Fatalf("got %d links, want 32", len(links))
	}
	if links[0] != "https://starckfilmes-v27.com/catalog/tony-2026-26-10-2026/" {
		t.Errorf("first link %q", links[0])
	}
}

func Test_extractStarckFilmesLinks_skipsSuggestions(t *testing.T) {
	// a title page lists "Você pode gostar" cards that are not search results
	if links := extractStarckFilmesLinks(loadStarckFilmesDoc(t, "lupin-4-temporada-2026.html")); len(links) != 0 {
		t.Errorf("got %d links from a title page: %v", len(links), links)
	}
}

func Test_parseStarckFilmesPage_movieWith4K(t *testing.T) {
	page, rels := parseStarckFilmesPage(loadStarckFilmesDoc(t, "homem-aranha-um-novo-dia-2026.html"))
	if page.Title != "Homem-Aranha: Um Novo Dia" || page.OriginalTitle != "Spider-Man: Brand New Day" || page.Year != "2026" {
		t.Errorf("page: %+v", page)
	}
	want := []struct{ hash, title, size string }{
		{"2a6b5b14ef835da8e3d595c4d185ebe43d093e5d", "Homem-Aranha.Um.Novo.Dia.2026.WEB-DL.1080p.x264.DUAL.5.1-STARCKFILMES", "3.53 GB"},
		{"e7ca9615df5424db2e00a0f8d9f4c617c01a86b8", "Homem-Aranha.Um.Novo.Dia.2026.WEB-DL.1080p.x264.FULLHD.DUAL.5.1-STARCKFILMES", "9.64 GB"},
		{"5c0e393cc4585af21cbf2d1a1a63c1e4906161da", "Homem-Aranha - Um Novo Dia 2026 WEB-DL 2160p x265 DV HDR10+ DUAL 5.1", "25.95 GB"},
	}
	if len(rels) != len(want) {
		t.Fatalf("got %d releases, want %d", len(rels), len(want))
	}
	for k, w := range want {
		r := rels[k]
		if r.InfoHash != w.hash || r.Title != w.title {
			t.Errorf("#%d: got %q %q, want %q %q", k, r.InfoHash, r.Title, w.hash, w.title)
		}
		if r.Size == "" {
			t.Errorf("#%d: no size", k)
		}
		if !slices.Equal(starckAudioNames(r), []string{"brazilian", "eng"}) {
			t.Errorf("#%d: audio %v", k, starckAudioNames(r))
		}
		if len(r.Trackers) < 10 {
			t.Errorf("#%d: %d trackers", k, len(r.Trackers))
		}
		for _, tr := range r.Trackers {
			if strings.Contains(tr, "anunciar") {
				t.Errorf("#%d: translated tracker left: %q", k, tr)
			}
		}
		if strings.Contains(r.MagnetLink, "anunciar") {
			t.Errorf("#%d: translated tracker left in magnet", k)
		}
	}
}

func Test_parseStarckFilmesPage_seasonPack(t *testing.T) {
	page, rels := parseStarckFilmesPage(loadStarckFilmesDoc(t, "lupin-4-temporada-2026.html"))
	if page.Title != "Lupin 4ª Temporada" || page.OriginalTitle != "Lupin" {
		t.Errorf("page: %+v", page)
	}
	if len(rels) != 1 || rels[0].InfoHash != "e0bd6b939a13644237518abf0412f57f09bdcb88" ||
		rels[0].Title != "Lupin.2026.S04.WEB-DL.1080p.x264.DUAL.5.1-STARCKFILMES" || rels[0].Size == "" {
		t.Errorf("releases: %+v", rels)
	}
}

func Test_parseStarckFilmesPage_collectionYearRange(t *testing.T) {
	page, rels := parseStarckFilmesPage(loadStarckFilmesDoc(t, "anaconda-colecao-1997-2009.html"))
	if page.Year != "1997" || page.OriginalTitle != "Anaconda" {
		t.Errorf("page: %+v", page)
	}
	if len(rels) != 1 || rels[0].InfoHash != "dba9c38dee6c326cb1af45c4ce6b7bfc5edc9aee" {
		t.Errorf("releases: %+v", rels)
	}
}

func Test_starckFilmesDateFromLink(t *testing.T) {
	tests := map[string]time.Time{
		// day ≤ 12: "2026-05-10" must not be read as yyyy-mm-dd
		"https://starckfilmes-v27.com/catalog/homem-aranha-um-novo-dia-2026-05-10-2026/": time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
		"https://starckfilmes-v27.com/catalog/lupin-4-temporada-2026-23-10-2026/":        time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC),
		"https://starckfilmes-v27.com/catalog/anaconda-colecao-1997-2009-25-10-2026":     time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC),
	}
	for link, want := range tests {
		if got := starckFilmesDateFromLink(link); !got.Equal(want) {
			t.Errorf("%s: got %v, want %v", link, got, want)
		}
	}
}

func starckAudioNames(rel starckFilmesRelease) []string {
	var out []string
	for _, a := range rel.Audio {
		out = append(out, a.String())
	}
	return out
}
