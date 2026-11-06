package handler

// Testes de caracterização (auditoria de desempenho, 06/11/2026): congelam a
// saída atual dos parsers e da cadeia de post-processors sobre o HTML gravado
// em testdata/, para que otimizações não mudem o resultado. Para regravar
// depois de uma mudança de comportamento aprovada:
//
//	go test ./api -run Test_golden -update

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"

	"github.com/module195/torrent-indexer/schema"
)

var updateGolden = flag.Bool("update", false, "regrava os arquivos golden de testdata/golden")

func loadFixtureDoc(t testing.TB, site, name string) *goquery.Document {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", site, name))
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

func assertGolden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "golden", name+".json")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s ausente (rode com -update): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("saída de %s mudou; veja com: go test ./api -run '%s' -update && git diff %s", name, t.Name(), path)
	}
}

// Links das páginas de listagem de cada site.
func Test_goldenListingLinks(t *testing.T) {
	cases := []struct {
		site, file string
		extract    func(*goquery.Document) []string
	}{
		{"rede_torrent", "busca-homem.html", extractRedeTorrentLinks},
		{"rede_torrent", "home.html", extractRedeTorrentLinks},
		{"rede_torrent", "pagina-2.html", extractRedeTorrentLinks},
		{"starck_filmes", "home.html", extractStarckFilmesLinks},
		{"torrentfilmeshds", "busca-lupin.html", extractTorrentFilmesHDsLinks},
	}
	for _, c := range cases {
		t.Run(c.site+"/"+c.file, func(t *testing.T) {
			assertGolden(t, "links_"+c.site+"_"+strings.TrimSuffix(c.file, ".html"), c.extract(loadFixtureDoc(t, c.site, c.file)))
		})
	}
}

var (
	bludvRuFixtures      = []string{"homem-aranha-um-novo-dia-2026.html", "mortal-kombat-2-2026.html", "the-boys-5a-temporada.html"}
	redeTorrentFixtures  = []string{"lupin-4a-temporada.html", "moana-2026.html", "onde-esta-a-wanda-2a-temporada.html", "os-bad-boys-colecao.html"}
	starckFilmesFixtures = []string{"anaconda-colecao-1997-2009.html", "homem-aranha-um-novo-dia-2026.html", "lupin-4-temporada-2026.html", "outlander-blood-of-my-blood-2-temporada-2026.html"}
	tfhdsFixtures        = []string{"lupin-4a-temporada-2026.html", "street-fighter-2026.html"}
	baixarApidoFixtures  = []string{"opp.html", "opp4k.html", "longe-de-casa-4k.html", "outlander-bomb-s1.html", "outlander-bomb-s2.html"}
)

// Páginas de título: dados da página + um release por magnet, na ordem do parser.
func Test_goldenTitlePages(t *testing.T) {
	type out struct {
		Page     any `json:"page"`
		Releases any `json:"releases"`
	}
	for _, f := range bludvRuFixtures {
		t.Run("bludv_ru/"+f, func(t *testing.T) {
			p, r := parseBludvRuPage(loadFixtureDoc(t, "bludv_ru", f))
			assertGolden(t, "page_bludv_ru_"+strings.TrimSuffix(f, ".html"), out{p, r})
		})
	}
	for _, f := range redeTorrentFixtures {
		t.Run("rede_torrent/"+f, func(t *testing.T) {
			p, r := parseRedeTorrentPage(loadFixtureDoc(t, "rede_torrent", f))
			assertGolden(t, "page_rede_torrent_"+strings.TrimSuffix(f, ".html"), out{p, r})
		})
	}
	for _, f := range starckFilmesFixtures {
		t.Run("starck_filmes/"+f, func(t *testing.T) {
			p, r := parseStarckFilmesPage(loadFixtureDoc(t, "starck_filmes", f))
			assertGolden(t, "page_starck_filmes_"+strings.TrimSuffix(f, ".html"), out{p, r})
		})
	}
	for _, f := range tfhdsFixtures {
		t.Run("torrentfilmeshds/"+f, func(t *testing.T) {
			assertGolden(t, "page_torrentfilmeshds_"+strings.TrimSuffix(f, ".html"), tfhdsFixtureReleases(t, f))
		})
	}
	for _, f := range baixarApidoFixtures {
		t.Run("baixarapido/"+f, func(t *testing.T) {
			torrents, err := parseBaixarApidoPage(loadFixtureDoc(t, "baixarapido", f), baixarApidoFixtureLink(f))
			if err != nil {
				t.Fatal(err)
			}
			assertGolden(t, "page_baixarapido_"+strings.TrimSuffix(f, ".html"), torrents)
		})
	}
}

func baixarApidoFixtureLink(f string) string {
	return "https://baixarapido.com/baixar/" + strings.TrimSuffix(f, ".html") + "/"
}

type tfhdsGolden struct {
	Title, Magnet, InfoHash, Size string
	Trackers                      []string
	Audio                         []schema.Audio
}

// tfhdsFixtureReleases repete o caminho dos magnets diretos de
// getTorrentsTorrentFilmesHDs (o parse fica dentro do handler).
func tfhdsFixtureReleases(t testing.TB, file string) []tfhdsGolden {
	doc := loadFixtureDoc(t, "torrentfilmeshds", file)
	title := strings.TrimSpace(doc.Find(".titulo").First().Text())
	var out []tfhdsGolden
	doc.Find("a[href^='magnet:']").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		rel, ok := buildTorrentFilmesHDsRelease(title, href, torrentFilmesHDsButtonLabel(s), nil, "")
		if !ok {
			return
		}
		out = append(out, tfhdsGolden{rel.title, rel.magnet, rel.infoHash, rel.size, rel.trackers, rel.audio})
	})
	return out
}

// fixtureTorrents monta os IndexedTorrent como os handlers montam (sem
// seeders: o scrape UDP não roda em teste), a partir de todas as páginas
// gravadas. A ordem é a do arquivo, não a de chegada das goroutines.
func fixtureTorrents(t testing.TB) []schema.IndexedTorrent {
	var all []schema.IndexedTorrent
	for _, f := range bludvRuFixtures {
		link := "https://bludv.ru/" + strings.TrimSuffix(f, ".html") + "/"
		p, rels := parseBludvRuPage(loadFixtureDoc(t, "bludv_ru", f))
		for _, r := range rels {
			all = append(all, schema.IndexedTorrent{Title: r.Title, OriginalTitle: p.Title, Details: link, Year: p.Year, IMDB: p.IMDB,
				Audio: r.Audio, MagnetLink: r.MagnetLink, Date: p.Date, InfoHash: r.InfoHash, Trackers: r.Trackers, Size: r.Size})
		}
	}
	for _, f := range redeTorrentFixtures {
		link := "https://redestorrents.com/" + strings.TrimSuffix(f, ".html") + "/"
		p, rels := parseRedeTorrentPage(loadFixtureDoc(t, "rede_torrent", f))
		for _, r := range rels {
			all = append(all, schema.IndexedTorrent{Title: r.Title, OriginalTitle: processTitle(p.Title, r.Audio), Details: link, Year: p.Year, IMDB: p.IMDB,
				Audio: r.Audio, MagnetLink: r.MagnetLink, Date: p.Date, InfoHash: r.InfoHash, Trackers: r.Trackers, Size: r.Size})
		}
	}
	for _, f := range starckFilmesFixtures {
		link := "https://starckfilmes-v28.com/" + strings.TrimSuffix(f, ".html") + "/"
		p, rels := parseStarckFilmesPage(loadFixtureDoc(t, "starck_filmes", f))
		for _, r := range rels { // IMDb vem do OMDb/TVmaze no handler; fica vazio aqui
			all = append(all, schema.IndexedTorrent{Title: r.Title, OriginalTitle: processTitle(p.Title, r.Audio), Details: link, Year: p.Year,
				Audio: r.Audio, MagnetLink: r.MagnetLink, Date: p.Date, InfoHash: r.InfoHash, Trackers: r.Trackers, Size: r.Size})
		}
	}
	for _, f := range tfhdsFixtures {
		link := "https://torrentfilmehds.org/" + strings.TrimSuffix(f, ".html") + "/"
		for _, r := range tfhdsFixtureReleases(t, f) {
			all = append(all, schema.IndexedTorrent{Title: r.Title, Details: link, Audio: r.Audio, MagnetLink: r.Magnet,
				InfoHash: r.InfoHash, Trackers: r.Trackers, Size: r.Size})
		}
	}
	for _, f := range baixarApidoFixtures {
		ts, err := parseBaixarApidoPage(loadFixtureDoc(t, "baixarapido", f), baixarApidoFixtureLink(f))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, ts...)
	}
	return all
}

// cloneTorrents copia o lote com fatias independentes (os post-processors
// alteram Trackers e Audio no lugar).
func cloneTorrents(base []schema.IndexedTorrent) []schema.IndexedTorrent {
	out := make([]schema.IndexedTorrent, len(base))
	copy(out, base)
	for k := range out {
		out[k].Trackers = append([]string(nil), base[k].Trackers...)
		out[k].Audio = append([]schema.Audio(nil), base[k].Audio...)
		out[k].Files = append([]schema.File(nil), base[k].Files...)
	}
	return out
}

func runChain(i *Indexer, query string, torrents []schema.IndexedTorrent) []schema.IndexedTorrent {
	r := httptest.NewRequest("GET", "/indexers/x?"+query, nil)
	for _, pp := range GlobalPostProcessors {
		torrents = pp(i, r, torrents)
	}
	return torrents
}

// goldenChainQueries são as formas de query que o guindex manda (texto, com
// ano, com temporada/episódio, IMDb) e os parâmetros que mudam a cadeia.
var goldenChainQueries = []string{
	"q=homem+aranha",
	"q=Homem-Aranha+Um+Novo+Dia+2026",
	"q=lupin",
	"q=lupin&season=4",
	"q=Lupin+S04E02",
	"q=outlander+temporada+2",
	"q=oppenheimer",
	"q=the+boys&season=5&episode=1",
	"q=moana&audio=brazilian",
	"q=anaconda&sortBy=date&sortDirection=asc",
	"q=street+fighter&limit=2",
	"q=filme+que+nao+existe",
	"q=lupin&filter_results=false",
}

// Cadeia completa de post-processors (similaridade, dedup, IMDb, tamanho,
// limpeza de título, áudio, ordenação, filtros, limite, trackers públicos)
// sobre o lote inteiro.
func Test_goldenPostProcessorChain(t *testing.T) {
	base := fixtureTorrents(t)
	if len(base) < 20 {
		t.Fatalf("só %d torrents nas fixtures", len(base))
	}
	for _, q := range goldenChainQueries {
		t.Run(q, func(t *testing.T) {
			got := runChain(&Indexer{}, q, cloneTorrents(base))
			vals, _ := url.ParseQuery(q)
			assertGolden(t, "chain_"+sanitizeGoldenName(vals.Encode()), got)
		})
	}
}

// A cadeia precisa dar o mesmo resultado em toda execução (sem depender da
// ordem de iteração de mapas), senão os golden não servem de referência.
func Test_goldenChainIsDeterministic(t *testing.T) {
	base := fixtureTorrents(t)
	for _, q := range goldenChainQueries {
		first, _ := json.Marshal(runChain(&Indexer{}, q, cloneTorrents(base)))
		for k := 0; k < 2; k++ {
			got, _ := json.Marshal(runChain(&Indexer{}, q, cloneTorrents(base)))
			if !bytes.Equal(got, first) {
				t.Fatalf("%s: cadeia não determinística", q)
			}
		}
	}
}

func sanitizeGoldenName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
