package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func Test_releaseFromButtonLabel(t *testing.T) {
	page := "Ted Lasso 1ª Temporada (2020)"
	cases := []struct{ label, want string }{
		{"🧲S01.1080p | MKV | Ep.01/05 | Dual Áudio→", "Ted.Lasso.S01E01-E05.1080p.MKV.DUAL"},
		{"🧲S01.1080p | MKV | Ep.08 | Dual Áudio→", "Ted.Lasso.S01E08.1080p.MKV.DUAL"},
		{"🧲S01.1080p | MKV | Ep.10 Final | Dual Áudio→", "Ted.Lasso.S01E10.1080p.MKV.DUAL"},
		{"S3 720p", "Ted.Lasso.S03.720p"},
		{"Baixar", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := releaseFromButtonLabel(page, c.label); got != c.want {
			t.Errorf("releaseFromButtonLabel(%q) = %q, want %q", c.label, got, c.want)
		}
	}
}

func Test_isGluedRelease(t *testing.T) {
	for _, s := range []string{"COMANDO.TO-TedLassoS01E01-05720p", "TedLassoS01E091080pDUAL", "TedLassoS04E07WEB-DL1080px264DUAL5.1"} {
		if !isGluedRelease(s) {
			t.Errorf("isGluedRelease(%q) = false", s)
		}
	}
	for _, s := range []string{"Ted.Lasso.S01E01.1080p.WEB-DL", "Ted Lasso S01E01-E05 720p", "Ted.Lasso.1080p"} {
		if isGluedRelease(s) {
			t.Errorf("isGluedRelease(%q) = true", s)
		}
	}
}

func loadTorrentFilmesHDsFixture(t *testing.T, name string) *goquery.Document {
	t.Helper()
	f, err := os.Open("testdata/torrentfilmeshds/" + name)
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

func Test_extractTorrentFilmesHDsLinks_search(t *testing.T) {
	links := extractTorrentFilmesHDsLinks(loadTorrentFilmesHDsFixture(t, "busca-lupin.html"))
	want := []string{
		"https://www.torrentfilmehds.org/lupin-4a-temporada-2026/",
		"https://www.torrentfilmehds.org/lupin-3a-temporada-2023/",
		"https://www.torrentfilmehds.org/lupin-2a-temporada-2022/",
		"https://www.torrentfilmehds.org/lupin-1a-temporada-2021/",
	}
	if strings.Join(links, " ") != strings.Join(want, " ") {
		t.Errorf("links = %v, want %v", links, want)
	}
}

func Test_normalizeMagnet_mangledTrackers(t *testing.T) {
	in := "magnet:?xt=urn:btih:abc&#038;dn=X&#038;tr=udptracker.openbittorrent.com80announce&#038;tr=udptracker.opentrackr.org1337announce"
	want := "magnet:?xt=urn:btih:abc&dn=X&tr=udp://tracker.openbittorrent.com:80/announce&tr=udp://tracker.opentrackr.org:1337/announce"
	if got := normalizeMagnet(in); got != want {
		t.Errorf("normalizeMagnet() = %q, want %q", got, want)
	}
	ok := "magnet:?xt=urn:btih:abc&tr=udp://tracker.opentrackr.org:1337/announce&tr=udp%3A%2F%2Fopen.stealth.si%3A80%2Fannounce"
	if got := normalizeMagnet(ok); got != ok {
		t.Errorf("normalizeMagnet() changed a valid magnet: %q", got)
	}
}

func Test_audioFromButtonLabel(t *testing.T) {
	cases := []struct {
		label string
		want  string
	}{
		{"S04.1080p | MKV | 7.49 GB | Dual Áudio", "Português,Inglês"},
		{"1080p.TELESYNC | 4.43 GB | Inglês", "Inglês"},
		{"CAMRip | MKV | 2.21 GB | Dublado", "Português"},
		{"1080p | Legendado", ""},
		{"Baixar", ""},
	}
	for _, c := range cases {
		var got []string
		for _, a := range audioFromButtonLabel(c.label) {
			got = append(got, string(a))
		}
		if strings.Join(got, ",") != c.want {
			t.Errorf("audioFromButtonLabel(%q) = %v, want %q", c.label, got, c.want)
		}
	}
}

// Páginas reais (25/10/2026): cada magnet tem um botão com áudio e tamanho próprios.
func Test_buildTorrentFilmesHDsRelease_pages(t *testing.T) {
	type want struct{ hash, audio, size, tracker string }
	cases := []struct {
		page string
		want []want
	}{
		{"lupin-4a-temporada-2026.html", []want{
			{"829160119aa195c8c5372f331538b205675eb07c", "Português,Inglês", "7.49 GB", "udp://tracker.opentrackr.org:1337/announce"},
		}},
		{"street-fighter-2026.html", []want{
			{"21b8cc8dd8ff5b845d02f7a360b25b4257fcb3e3", "Inglês", "4.43 GB", "udp://tracker.bittor.pw:1337/announce"},
			{"0534a846a244a36502e53a65820e5e97b67388bd", "Inglês", "5.11 GB", "udp://tracker.bittor.pw:1337/announce"},
			// xl= (3603989927 bytes) vence o rótulo, que diz 2.21 GB
			{"740c14a957280a9d5b4cd6d790ef91067d39f062", "Português", "3.36 GB", "udp://tracker.openbittorrent.com:80/announce"},
		}},
	}
	for _, c := range cases {
		doc := loadTorrentFilmesHDsFixture(t, c.page)
		title := strings.TrimSpace(doc.Find(".titulo").First().Text())
		var got []want
		doc.Find("a[href^='magnet:']").Each(func(_ int, s *goquery.Selection) {
			href, _ := s.Attr("href")
			rel, ok := buildTorrentFilmesHDsRelease(title, href, torrentFilmesHDsButtonLabel(s), nil, "")
			if !ok {
				t.Errorf("%s: release rejected: %s", c.page, href)
				return
			}
			var audio []string
			for _, a := range rel.audio {
				audio = append(audio, string(a))
			}
			got = append(got, want{rel.infoHash, strings.Join(audio, ","), rel.size, rel.trackers[0]})
		})
		if len(got) != len(c.want) {
			t.Fatalf("%s: %d releases, want %d: %+v", c.page, len(got), len(c.want), got)
		}
		for k := range got {
			if got[k] != c.want[k] {
				t.Errorf("%s #%d = %+v, want %+v", c.page, k, got[k], c.want[k])
			}
		}
	}
}

func Test_yearFits(t *testing.T) {
	cases := []struct {
		omdb   string
		y      int
		series bool
		want   bool
	}{
		{"2016", 2026, false, false}, // Moana (2016) não é Moana (2026)
		{"2026", 2026, false, true},
		{"2025", 2026, false, true},
		{"2021–", 2026, true, true},
		{"2021–2023", 2026, true, false},
		{"2027–", 2026, true, false},
		{"N/A", 2026, false, false},
	}
	for _, c := range cases {
		if got := yearFits(c.omdb, c.y, c.series); got != c.want {
			t.Errorf("yearFits(%q, %d, %v) = %v, want %v", c.omdb, c.y, c.series, got, c.want)
		}
	}
}

func Test_lookupIMDBByTitleAndYear(t *testing.T) {
	omdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("t") {
		case "Moana": // sem y= o OMDb devolve a animação de 2016
			if r.URL.Query().Get("y") == "2026" {
				_, _ = w.Write([]byte(`{"Title":"Moana","Year":"2026","imdbID":"tt22022452"}`))
				return
			}
			_, _ = w.Write([]byte(`{"Title":"Moana","Year":"2016","imdbID":"tt3521164"}`))
		case "Lupin":
			_, _ = w.Write([]byte(`{"Title":"Lupin","Year":"2021–","imdbID":"tt2531336"}`))
		case "Interestelar": // título em português: o OMDb acha outra coisa
			_, _ = w.Write([]byte(`{"Title":"Interestelar: O Documentário","Year":"2014","imdbID":"tt0000001"}`))
		default:
			_, _ = w.Write([]byte(`{"Response":"False"}`))
		}
	}))
	defer omdb.Close()
	tvmaze := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"Outra Série","premiered":"2020-01-01","externals":{"imdb":"tt9999999"}}`))
	}))
	defer tvmaze.Close()
	oldOMDb, oldTV := tfhdsOMDbURL, tfhdsTVmazeURL
	tfhdsOMDbURL, tfhdsTVmazeURL = omdb.URL+"/", tvmaze.URL
	defer func() { tfhdsOMDbURL, tfhdsTVmazeURL = oldOMDb, oldTV }()

	cases := []struct {
		name, year string
		series     bool
		want       string
	}{
		{"Moana", "2026", false, "https://www.imdb.com/title/tt22022452/"},
		{"Lupin", "2026", true, "https://www.imdb.com/title/tt2531336/"},
		{"Interestelar", "2014", false, ""},
		{"Série Desconhecida", "2026", true, ""}, // o TVmaze responde com outro nome
	}
	for _, c := range cases {
		if got := lookupIMDBByTitleAndYear(context.Background(), c.name, c.year, c.series); got != c.want {
			t.Errorf("lookupIMDBByTitleAndYear(%q, %q, %v) = %q, want %q", c.name, c.year, c.series, got, c.want)
		}
	}
}
