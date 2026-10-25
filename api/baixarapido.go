package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/magnet"
	"github.com/module195/torrent-indexer/schema"
	goscrape "github.com/module195/torrent-indexer/scrape"
	"github.com/module195/torrent-indexer/utils"
)

var baixarapido = IndexerMeta{
	Label:       "baixarapido",
	URL:         utils.GetIndexerURLFromEnv("INDEXER_BAIXARAPIDO_URL", "https://baixarapido.com"),
	SearchURL:   "/pesquisa/",
	PagePattern: "/pesquisa/%s/pagina/%s/",
}

const (
	// páginas de título abertas ao mesmo tempo (a busca traz até 30)
	baixarApidoPageConcurrency = 8
	// scrapes de tracker ao mesmo tempo
	baixarApidoScrapeConcurrency = 16
)

// baixarApidoBudget é o tempo que o indexer gasta abrindo páginas antes de
// responder com o que já tem; o multi corta em 9 s por padrão.
var baixarApidoBudget = func() time.Duration {
	if ms, err := strconv.Atoi(utils.GetEnvOrDefault("INDEXER_BAIXARAPIDO_BUDGET_MS", "7000")); err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return 7 * time.Second
}()

// Regexes do baixarapido, compiladas uma vez.
var (
	reBASeasonEpisode = regexp.MustCompile(`(?i)\bS(\d{1,2})\s*E(\d{1,3})\b`)
	reBASeasonOnly    = regexp.MustCompile(`(?i)\bS(\d{1,2})\b`)
	reBATemporada     = regexp.MustCompile(`(?i)\b(?:temporada|season)\s*(\d{1,2})\b|\b(\d{1,2})\s*(?:ª|a|º)?\s*temporada\b`)
	reBAEpisodio      = regexp.MustCompile(`(?i)\b(?:epis[oó]dio|episode|ep\.?)\s*(\d{1,3})\b`)
	reBAYear          = regexp.MustCompile(`\b(19\d{2}|20\d{2})\b`)
	reBASeasonSlug    = regexp.MustCompile(`-(\d{1,2})a-temporada`)
	reBASeasonName    = regexp.MustCompile(`(?i)\b(\d{1,2})\s*ª\s*temporada\b`)
	reBATorrentSuffix = regexp.MustCompile(`(?i)\s*torrent.*$`)
	reBAYearInParens  = regexp.MustCompile(`\((\d{4})\)`)
	reBAIMDbID        = regexp.MustCompile(`tt\d{7,8}`)
	reBAResolution    = regexp.MustCompile(`(?i)\b(\d{3,4}p)\b`)
	reBAHasResolution = regexp.MustCompile(`(?i)\d{3,4}p`)
	reBAMagnetDN      = regexp.MustCompile(`[?&]dn=([^&]+)`)
	reBASubtitleFile  = regexp.MustCompile(`(?i)\.(srt|sub|ass|ssa|idx|vtt)$`)
	reBAVideoMarker   = regexp.MustCompile(`(?i)\d{3,4}p|\b(4k|uhd|mkv|mp4|avi|bluray|blu-ray|bdrip|brrip|web-?dl|web-?rip|hdtv|remux|x264|x265|hevc|h264|h265)\b`)
	reBASubtitleWord  = regexp.MustCompile(`(?i)\b(legendas|subtitles?|subs)\b`)
	reBALabelPrefix   = regexp.MustCompile(`(?i)^.*?download\s+torrent\s*`)
	reBAExtension     = regexp.MustCompile(`(?i)\.(mkv|mp4|avi|torrent)$`)
	reBAGenericPrefix = regexp.MustCompile(`(?i)^[A-Z]+TORRENTS?\s+COM\s*[-.]?\s*`)
	reBAUnsafe        = regexp.MustCompile(`(?i)^\[UNSAFE\]\s*`)
	reBAEpisodePrefix = regexp.MustCompile(`(?i)^[\s.-]*EPISODIO[\s.-]*\d+[\s.-]*`)
	reBAFileTypeStart = regexp.MustCompile(`(?i)^(MP4|MKV|AVI)\s+`)
	reBAAudioStart    = regexp.MustCompile(`(?i)^(LEGENDADO|DUBLADO|DUAL-AUDIO|DUAL)\s+`)
	reBAAbbrevFirst   = regexp.MustCompile(`(?i)^(S\d|E\d|\d+P|WEB|HD|MKV|MP4)`)
	reBANeedsPrefix   = regexp.MustCompile(`(?i)^(S\d{2}E\d{2}|EPISODIO\s+\d+|\d{3,4}P\s|(MKV|MP4|AVI)\s|(DUBLADO|LEGENDADO)\s)`)
)

// baixarApidoSitePrefixes são os prefixos de sites que aparecem no dn dos magnets.
var baixarApidoSitePrefixes = func() []*regexp.Regexp {
	prefixes := []string{
		"BAIXARAPIDO COM", "SITEDETORRENTS COM", "HIDRATORRENTS ORG",
		"VAMOSTORRENT COM", "TORRENTFILMESHDS", "BLUDV",
		"VEMTORRENT COM", "COMOEUBAIXO COM", "VACATORRENT COM",
		"HIDRATORRENT COM", "STARCKFILMES", "TDF",
		"COMANDO TORRENTS", "COMANDO1", "REDE TORRENT",
		"VACA TORRENT", "VIA FILMES", "BAIXANDO TORRENTS",
	}
	out := make([]*regexp.Regexp, 0, len(prefixes))
	for _, p := range prefixes {
		out = append(out, regexp.MustCompile(`(?i)^`+regexp.QuoteMeta(p)+`\s*[-.]?\s*`))
	}
	return out
}()

// baixarApidoHyphenTags são tags com hífen que não podem virar espaço.
var baixarApidoHyphenTags = func() []struct {
	tag string
	re  *regexp.Regexp
} {
	tags := []string{"WEB-DL", "WEB-RIP", "BD-RIP", "DUAL-AUDIO", "CAM-RIP"}
	out := make([]struct {
		tag string
		re  *regexp.Regexp
	}, 0, len(tags))
	for _, t := range tags {
		out = append(out, struct {
			tag string
			re  *regexp.Regexp
		}{t, regexp.MustCompile(`(?i)` + regexp.QuoteMeta(t))})
	}
	return out
}()

// baixarApidoQuery é a busca separada do que o site não entende: com aspas, a
// busca é por frase exata, e ano ou episódio a mais derrubam tudo
// ("Oppenheimer 2023" e "The Last of Us S02E03" dão 0 resultados).
type baixarApidoQuery struct {
	title   string
	year    string
	season  int
	episode int
}

func parseBaixarApidoQuery(q string) baixarApidoQuery {
	var bq baixarApidoQuery
	s := q
	if m := reBASeasonEpisode.FindStringSubmatch(s); m != nil {
		bq.season, _ = strconv.Atoi(m[1])
		bq.episode, _ = strconv.Atoi(m[2])
		s = reBASeasonEpisode.ReplaceAllString(s, " ")
	}
	if m := reBATemporada.FindStringSubmatch(s); m != nil {
		if bq.season == 0 {
			n := m[1]
			if n == "" {
				n = m[2]
			}
			bq.season, _ = strconv.Atoi(n)
		}
		s = reBATemporada.ReplaceAllString(s, " ")
	}
	if m := reBAEpisodio.FindStringSubmatch(s); m != nil {
		if bq.episode == 0 {
			bq.episode, _ = strconv.Atoi(m[1])
		}
		s = reBAEpisodio.ReplaceAllString(s, " ")
	}
	if m := reBASeasonOnly.FindStringSubmatch(s); m != nil {
		if bq.season == 0 {
			bq.season, _ = strconv.Atoi(m[1])
		}
		s = reBASeasonOnly.ReplaceAllString(s, " ")
	}
	// o ano só sai quando sobra título ("1917" e "2012" são nomes de filme) e
	// quando pode ser ano de lançamento ("Blade Runner 2049" fica inteiro)
	if m := reBAYear.FindAllStringIndex(s, -1); len(m) > 0 {
		last := m[len(m)-1]
		y, _ := strconv.Atoi(s[last[0]:last[1]])
		rest := strings.TrimSpace(s[:last[0]] + " " + s[last[1]:])
		if rest != "" && y <= time.Now().Year()+1 {
			bq.year = s[last[0]:last[1]]
			s = rest
		}
	}
	bq.title = strings.TrimSpace(reWhitespace.ReplaceAllString(s, " "))
	return bq
}

// baixarApidoSearchURL monta a URL da busca (com aspas) ou da home.
// A paginação do site é /pagina/N/ (/page/N/ dá 404).
func baixarApidoSearchURL(baseURL, term, page string) string {
	base := strings.TrimRight(baseURL, "/")
	if term == "" {
		if page == "" || page == "1" {
			return base + "/"
		}
		return base + "/pagina/" + page + "/"
	}
	// PathEscape já codifica as aspas. Não codifique de novo.
	encoded := strings.ReplaceAll(url.PathEscape(`"`+term+`"`), "%20", "+")
	if page == "" || page == "1" {
		return base + "/pesquisa/" + encoded + "/"
	}
	return base + fmt.Sprintf(baixarapido.PagePattern, encoded, page)
}

func (i *Indexer) HandlerBaixarApidoIndexer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	metadata := baixarapido
	defer func() {
		i.metrics.IndexerDuration.WithLabelValues(metadata.Label).Observe(time.Since(start).Seconds())
		i.metrics.IndexerRequests.WithLabelValues(metadata.Label).Inc()
	}()

	ctx := r.Context()
	q := r.URL.Query().Get("q")
	page := r.URL.Query().Get("page")
	if page == "" {
		page = "1"
	}

	decoded, err := url.QueryUnescape(q)
	if err != nil {
		decoded = q
	}
	decoded = strings.ReplaceAll(decoded, "+", " ")
	bq := parseBaixarApidoQuery(decoded)
	if q != "" && bq.title == "" {
		bq.title = strings.TrimSpace(decoded)
	}

	targetURL := baixarApidoSearchURL(baixarapido.URL, bq.title, page)

	logging.Info().
		Str("url", r.URL.String()).
		Str("target_url", targetURL).
		Str("method", r.Method).
		Msg("Processing indexer request")

	doc, err := getListingDocument(ctx, i, targetURL, baixarapido.URL)
	if err != nil {
		logging.Error().Err(err).Str("url", targetURL).Msg("Failed to get document")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
		return
	}

	entries := extractBaixarApidoEntries(doc)

	// Fallback: se não achou nada, tenta variações do título, sempre com aspas.
	// Sem aspas o site faz OU entre as palavras e traz lixo.
	if len(entries) == 0 && bq.title != "" && page == "1" {
		for _, altQ := range buildBaixarApidoFallbackQueries(bq.title) {
			altURL := baixarApidoSearchURL(baixarapido.URL, altQ, "1")
			logging.Info().
				Str("original_q", q).
				Str("fallback_q", altQ).
				Str("url", altURL).
				Msg("Trying fallback search")

			altDoc, err := getListingDocument(ctx, i, altURL, baixarapido.URL)
			if err != nil {
				continue
			}
			entries = extractBaixarApidoEntries(altDoc)
			if len(entries) > 0 {
				break
			}
		}
	}

	entries = filterBaixarApidoEntries(entries, bq)
	links := make([]string, 0, len(entries))
	for _, e := range entries {
		links = append(links, e.url)
	}
	logging.Info().
		Int("links_found", len(links)).
		Str("url", targetURL).
		Msg("Extracted links")

	allTorrents := fetchBaixarApidoPages(ctx, i, links, start.Add(baixarApidoBudget))
	allTorrents = dedupBaixarApidoTorrents(allTorrents)
	scrapeBaixarApidoSeeders(ctx, i, allTorrents)

	// Ordena: seeders desc, year desc, title asc
	sort.SliceStable(allTorrents, func(a, b int) bool {
		if allTorrents[a].SeedCount != allTorrents[b].SeedCount {
			return allTorrents[a].SeedCount > allTorrents[b].SeedCount
		}
		ya, _ := strconv.Atoi(allTorrents[a].Year)
		yb, _ := strconv.Atoi(allTorrents[b].Year)
		if ya != yb {
			return ya > yb
		}
		return allTorrents[a].Title < allTorrents[b].Title
	})

	// Temporada/episódio tirados da query viram filtro (o FilterBy já sabe ler).
	pr := withSeasonEpisodeParams(r, bq.season, bq.episode)

	// Aplica post-processors (similarity, etc)
	postProcessedTorrents := allTorrents
	for _, processor := range i.postProcessors {
		postProcessedTorrents = processor(i, pr, postProcessedTorrents)
	}

	resp := Response{
		Count:        len(postProcessedTorrents),
		IndexedCount: len(allTorrents),
		Results:      postProcessedTorrents,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// withSeasonEpisodeParams devolve r com season/episode da query quando quem
// chamou não mandou os seus.
func withSeasonEpisodeParams(r *http.Request, season, episode int) *http.Request {
	params := r.URL.Query()
	if season == 0 || params.Get("season") != "" || params.Get("episode") != "" {
		return r
	}
	params.Set("season", strconv.Itoa(season))
	if episode > 0 {
		params.Set("episode", strconv.Itoa(episode))
	}
	r2 := r.Clone(r.Context())
	r2.URL.RawQuery = params.Encode()
	return r2
}

// buildBaixarApidoFallbackQueries tenta o título sem artigo e sem pontuação.
// Com aspas, título de uma palavra é seguro ("Duna" acha só Duna).
func buildBaixarApidoFallbackQueries(title string) []string {
	var out []string
	seen := map[string]bool{title: true}
	add := func(s string) {
		s = strings.TrimSpace(reWhitespace.ReplaceAllString(s, " "))
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	noArticle := reLeadingArticle.ReplaceAllString(title, "")
	add(noArticle)
	// "Homem-Aranha: Longe de Casa" -> "Homem Aranha Longe de Casa"
	add(strings.NewReplacer(":", " ", "-", " ", "–", " ").Replace(noArticle))
	return out
}

// baixarApidoEntry é um resultado da busca ou da home.
type baixarApidoEntry struct {
	url     string
	name    string
	year    string
	formats string // "1080p / 4K / 720p / BluRay"
}

// extractBaixarApidoEntries lê o JSON-LD ItemList da página e, se ele faltar,
// os cards (<a><article class="movie-card">).
func extractBaixarApidoEntries(doc *goquery.Document) []baixarApidoEntry {
	var entries []baixarApidoEntry
	seen := map[string]bool{}
	doc.Find(`script[type="application/ld+json"]`).Each(func(_ int, s *goquery.Selection) {
		// a busca e a home trazem o ItemList dentro de um CollectionPage
		var page struct {
			Type       string          `json:"@type"`
			MainEntity json.RawMessage `json:"mainEntity"`
		}
		raw := []byte(s.Text())
		if err := json.Unmarshal(raw, &page); err != nil {
			return
		}
		if page.Type == "CollectionPage" && len(page.MainEntity) > 0 {
			raw = page.MainEntity
		}
		var list struct {
			Type     string `json:"@type"`
			Elements []struct {
				Name string `json:"name"`
				URL  string `json:"url"`
				Item struct {
					Name           string `json:"name"`
					URL            string `json:"url"`
					CopyrightYear  any    `json:"copyrightYear"`
					EncodingFormat string `json:"encodingFormat"`
				} `json:"item"`
			} `json:"itemListElement"`
		}
		if err := json.Unmarshal(raw, &list); err != nil || list.Type != "ItemList" {
			return
		}
		for _, el := range list.Elements {
			u := el.Item.URL
			if u == "" {
				u = el.URL
			}
			if u == "" || seen[u] {
				continue
			}
			name := el.Item.Name
			if name == "" {
				name = el.Name
			}
			seen[u] = true
			entries = append(entries, baixarApidoEntry{
				url:     u,
				name:    name,
				year:    strings.TrimSpace(fmt.Sprint(nilToEmpty(el.Item.CopyrightYear))),
				formats: el.Item.EncodingFormat,
			})
		}
	})
	if len(entries) > 0 {
		return entries
	}
	doc.Find("article.movie-card").Each(func(_ int, s *goquery.Selection) {
		// O link é o pai do article
		parent := s.Parent()
		if parent.Is("a") {
			href, exists := parent.Attr("href")
			if exists && href != "" && !seen[href] {
				seen[href] = true
				entries = append(entries, baixarApidoEntry{url: href})
			}
		}
	})
	return entries
}

func nilToEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// filterBaixarApidoEntries descarta, antes de abrir a página, o resultado que
// com certeza não serve: ano longe do pedido ou outra temporada. Sem o dado,
// o resultado fica.
func filterBaixarApidoEntries(entries []baixarApidoEntry, bq baixarApidoQuery) []baixarApidoEntry {
	wantYear, _ := strconv.Atoi(bq.year)
	return utils.Filter(entries, func(e baixarApidoEntry) bool {
		if wantYear > 0 && bq.season == 0 {
			if y, err := strconv.Atoi(e.year); err == nil && y > 0 && (y < wantYear-1 || y > wantYear+1) {
				return false
			}
		}
		if bq.season > 0 {
			if s := baixarApidoEntrySeason(e); s > 0 && s != bq.season {
				return false
			}
		}
		return true
	})
}

// baixarApidoEntrySeason lê a temporada do slug (-2a-temporada) ou do nome (2ª Temporada).
func baixarApidoEntrySeason(e baixarApidoEntry) int {
	if m := reBASeasonSlug.FindStringSubmatch(e.url); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	if m := reBASeasonName.FindStringSubmatch(e.name); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// fetchBaixarApidoPages abre as páginas com concorrência limitada e devolve o
// que terminou até deadline; o resto é abandonado (e fica no cache para a próxima).
func fetchBaixarApidoPages(ctx context.Context, i *Indexer, links []string, deadline time.Time) []schema.IndexedTorrent {
	type result struct {
		torrents []schema.IndexedTorrent
	}
	results := make(chan result, len(links))
	sem := make(chan struct{}, baixarApidoPageConcurrency)
	for _, link := range links {
		go func(link string) {
			sem <- struct{}{}
			defer func() { <-sem }()
			torrents, err := getTorrentsBaixarApido(ctx, i, link)
			if err != nil {
				logging.Error().Err(err).Str("link", link).Msg("Failed to get torrents")
			}
			results <- result{torrents}
		}(link)
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	var all []schema.IndexedTorrent
	for range links {
		select {
		case res := <-results:
			all = append(all, res.torrents...)
		case <-timer.C:
			logging.Warn().Int("pages", len(links)).Msg("baixarapido: budget reached, returning partial results")
			return all
		case <-ctx.Done():
			return all
		}
	}
	return all
}

// dedupBaixarApidoTorrents tira os magnets repetidos entre páginas
// (oppenheimer, oppenheimer-4k e oppenheimer-4k-hdr têm os mesmos), antes do
// scrape de tracker. Fica a página de URL mais curta, a principal.
func dedupBaixarApidoTorrents(torrents []schema.IndexedTorrent) []schema.IndexedTorrent {
	best := map[string]int{}
	var out []schema.IndexedTorrent
	for _, t := range torrents {
		key := strings.ToLower(t.InfoHash)
		if key == "" {
			out = append(out, t)
			continue
		}
		if idx, ok := best[key]; ok {
			if len(t.Details) < len(out[idx].Details) {
				out[idx] = t
			}
			continue
		}
		best[key] = len(out)
		out = append(out, t)
	}
	return out
}

// scrapeBaixarApidoSeeders preenche seeders/leechers com concorrência limitada.
func scrapeBaixarApidoSeeders(ctx context.Context, i *Indexer, torrents []schema.IndexedTorrent) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, baixarApidoScrapeConcurrency)
	for idx := range torrents {
		wg.Add(1)
		go func(t *schema.IndexedTorrent) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if peer, seed, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, t.InfoHash, t.Trackers); err == nil {
				t.SeedCount = seed
				t.LeechCount = peer
			}
		}(&torrents[idx])
	}
	wg.Wait()
}

// baixarApidoPageMeta são os dados estruturados da página de título.
type baixarApidoPageMeta struct {
	imdb string
	year string
}

// readBaixarApidoPageMeta lê IMDb e ano do JSON-LD Movie e da microdata.
func readBaixarApidoPageMeta(doc *goquery.Document) baixarApidoPageMeta {
	var meta baixarApidoPageMeta
	doc.Find(`script[type="application/ld+json"]`).EachWithBreak(func(_ int, s *goquery.Selection) bool {
		var movie struct {
			Type          string `json:"@type"`
			SameAs        any    `json:"sameAs"`
			CopyrightYear any    `json:"copyrightYear"`
		}
		if err := json.Unmarshal([]byte(s.Text()), &movie); err != nil {
			return true
		}
		if movie.Type != "Movie" && movie.Type != "TVSeries" {
			return true
		}
		meta.imdb = reBAIMDbID.FindString(fmt.Sprint(nilToEmpty(movie.SameAs)))
		if y := reBAYear.FindString(fmt.Sprint(nilToEmpty(movie.CopyrightYear))); y != "" {
			meta.year = y
		}
		return false
	})
	if meta.imdb == "" {
		doc.Find(`meta[itemprop="sameAs"]`).EachWithBreak(func(_ int, s *goquery.Selection) bool {
			meta.imdb = reBAIMDbID.FindString(s.AttrOr("content", ""))
			return meta.imdb == ""
		})
	}
	if meta.year == "" {
		if y := reBAYear.FindString(doc.Find(`[itemprop="datePublished"]`).First().AttrOr("content", "")); y != "" {
			meta.year = y
		}
	}
	if meta.year == "" {
		doc.Find("td.text-light.fw-bold").EachWithBreak(func(_ int, s *goquery.Selection) bool {
			if strings.EqualFold(strings.TrimSpace(s.Text()), "Título Original:") {
				if m := reBAYearInParens.FindStringSubmatch(s.Next().Text()); m != nil {
					meta.year = m[1]
				}
				return false
			}
			return true
		})
	}
	return meta
}

// baixarApidoMagnet é um botão de download: o magnet e o rótulo do bloco
// workExample que o contém ("DUAL ÁUDIO 5.1 MKV X265 BLURAY 2160P ULTRA HD 4K HDR").
type baixarApidoMagnet struct {
	link  string
	label string
	size  string
}

func readBaixarApidoMagnets(doc *goquery.Document) []baixarApidoMagnet {
	var out []baixarApidoMagnet
	doc.Find("a[href^='magnet:']").Each(func(_ int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" {
			return
		}
		label := ""
		if block := s.Closest(`[itemprop="workExample"]`); block.Length() > 0 {
			label = block.ChildrenFiltered(`meta[itemprop="name"]`).First().AttrOr("content", "")
			label = strings.TrimSpace(reBALabelPrefix.ReplaceAllString(label, ""))
		}
		out = append(out, baixarApidoMagnet{link: href, label: label, size: sizeNearMagnet(s)})
	})
	return out
}

// isSubtitleOnlyMagnet diz se o magnet é só de legenda. No baixarapido,
// "LEGENDADO" é o filme com áudio original e legenda: esse fica.
func isSubtitleOnlyMagnet(dn string) bool {
	dn = strings.TrimSpace(dn)
	if reBASubtitleFile.MatchString(dn) {
		return true
	}
	return reBASubtitleWord.MatchString(dn) && !reBAVideoMarker.MatchString(dn)
}

func getTorrentsBaixarApido(ctx context.Context, i *Indexer, link string) ([]schema.IndexedTorrent, error) {
	doc, err := getDocument(ctx, i, link, baixarapido.URL)
	if err != nil {
		return nil, err
	}
	return parseBaixarApidoPage(doc, link)
}

// parseBaixarApidoPage lê os releases de uma página /baixar/…, sem seeders
// (o scrape de tracker vem depois da dedup).
func parseBaixarApidoPage(doc *goquery.Document, link string) ([]schema.IndexedTorrent, error) {
	h1 := strings.TrimSpace(doc.Find("h1").First().Text())
	if h1 == "" {
		return nil, fmt.Errorf("no title found at %s", link)
	}
	h1 = strings.TrimSpace(reBATorrentSuffix.ReplaceAllString(h1, ""))

	bodyText := doc.Text()
	meta := readBaixarApidoPageMeta(doc)

	// Year: dados estruturados primeiro, texto da página como último recurso
	year := meta.year
	if year == "" {
		year = findYearFromText(bodyText, h1)
	}
	if year == "" {
		year = reBAYear.FindString(bodyText)
	}

	// IMDB: sameAs da página; OMDb/TVmaze só se ela não tiver
	imdb := meta.imdb
	if imdb == "" {
		imdb = reBAIMDbID.FindString(bodyText)
	}
	if imdb == "" && h1 != "" {
		imdb = fetchIMDBByTitle(h1, year)
	}
	// Normaliza para URL completa
	if imdb != "" && !strings.HasPrefix(imdb, "http") {
		imdb = "https://www.imdb.com/title/" + imdb + "/"
	}

	// Audio da página (ficha "Idioma:"). O rótulo de cada botão decide depois.
	audio := findAudioFromText(h1)
	qualidade := ""
	doc.Find("td.text-light.fw-bold").Each(func(_ int, s *goquery.Selection) {
		label := strings.TrimSpace(s.Text())
		switch {
		case strings.EqualFold(label, "Idioma:"):
			lang := normalizeAccents(strings.ToLower(strings.TrimSpace(s.Next().Text())))
			if strings.Contains(lang, "portugu") || strings.Contains(lang, "dual") {
				audio = append(audio, schema.AudioPortuguese)
			}
			if strings.Contains(lang, "ingl") || strings.Contains(lang, "english") || strings.Contains(lang, "dual") {
				audio = append(audio, schema.AudioEnglish)
			}
		case strings.EqualFold(label, "Qualidade:") && qualidade == "":
			// Pega só a primeira (ex: "720p" de "720p\nBRRip")
			qualidade = strings.TrimSpace(s.Next().Text())
			if i := strings.IndexAny(qualidade, "\n\r"); i > 0 {
				qualidade = strings.TrimSpace(qualidade[:i])
			}
		}
	})
	if len(audio) == 0 {
		// Sem ficha: palavras soltas na página
		allTextNorm := normalizeAccents(strings.ToLower(h1 + " " + bodyText))
		if strings.Contains(allTextNorm, "legendado") || strings.Contains(allTextNorm, "english") {
			audio = append(audio, schema.AudioEnglish)
		}
		if strings.Contains(allTextNorm, "dublado") || strings.Contains(allTextNorm, "portugues") || strings.Contains(allTextNorm, "dual") {
			audio = append(audio, schema.AudioPortuguese)
		}
	}

	// Size da página: só vale quando ela cita um único tamanho
	sizes := slices.Compact(findSizesFromText(doc.Find("body").Text()))
	size := ""
	if len(sizes) == 1 {
		size = sizes[0]
	}

	// Date
	date := getPublishedDateFromMeta(doc)

	magnets := readBaixarApidoMagnets(doc)
	if len(magnets) == 0 {
		return nil, nil
	}

	var results []schema.IndexedTorrent
	for _, m := range magnets {
		magnetLink := normalizeMagnet(m.link)
		magnetParsed, err := magnet.ParseMagnetUri(magnetLink)
		if err != nil {
			continue
		}

		magnetDN := ""
		if dn := reBAMagnetDN.FindStringSubmatch(magnetLink); len(dn) > 1 {
			magnetDN, _ = url.QueryUnescape(dn[1])
		}
		if isSubtitleOnlyMagnet(magnetDN) {
			continue
		}

		// Título do magnet - limpa prefixo do site
		releaseTitle := magnetParsed.DisplayName
		if releaseTitle == "" {
			releaseTitle = h1
		}
		releaseTitle = cleanBaixarApidoTitle(releaseTitle)

		// Sem resolução no nome: a do botão, senão a da ficha
		if !reBAHasResolution.MatchString(releaseTitle) {
			if res := reBAResolution.FindString(m.label); res != "" {
				releaseTitle = releaseTitle + " " + strings.ToLower(res)
			} else if qualidade != "" {
				releaseTitle = releaseTitle + " " + qualidade
			}
		}

		// O "-LEGENDADO-" do dn sai na limpeza; o rótulo do botão devolve,
		// para quem lê o nome (o guindex mostra "Subs")
		if strings.Contains(strings.ToUpper(m.label), "LEGENDADO") && !strings.Contains(strings.ToUpper(releaseTitle), "LEGENDADO") {
			releaseTitle = releaseTitle + " LEGENDADO"
		}

		// Se o dn do magnet for abreviado (ex: "T l s S03E13") ou só tiver
		// episódio/qualidade, prepende o h1 da página
		if (isAbbreviatedTitle(releaseTitle) || needsTitlePrefix(releaseTitle)) && h1 != "" {
			cleanH1 := cleanBaixarApidoTitle(h1)
			if cleanH1 != "" && !strings.Contains(strings.ToLower(releaseTitle), strings.ToLower(cleanH1)) {
				releaseTitle = cleanH1 + " " + releaseTitle
			}
		}

		// Audio do magnet: nome do release e rótulo do botão (DUAL ÁUDIO,
		// DUBLADO, LEGENDADO) valem mais que a página
		magnetAudio := getAudioFromTitle(releaseTitle+" "+m.label, audio)

		// Title processado - usa processTitle como torrent-dos-filmes
		titleProcessed := processTitle(h1, magnetAudio)

		// Size: o que o magnet diz de si (xl=, nome), o botão, a página
		mySize := releaseSize(magnetLink, releaseTitle)
		if mySize == "" {
			mySize = m.size
		}
		if mySize == "" && (size != "" || len(magnets) == 1) {
			mySize = size
		}

		results = append(results, schema.IndexedTorrent{
			Title:         releaseTitle,
			OriginalTitle: titleProcessed,
			Details:       link,
			Year:          year,
			IMDB:          imdb,
			Audio:         magnetAudio,
			MagnetLink:    magnetLink,
			Date:          date,
			InfoHash:      magnetParsed.InfoHash.String(),
			Trackers:      magnetParsed.Trackers,
			Size:          mySize,
		})
	}

	return results, nil
}

// normalizeAccents remove acentos para comparação
func normalizeAccents(s string) string {
	return baixarApidoAccents.Replace(s)
}

var baixarApidoAccents = strings.NewReplacer(
	"á", "a", "à", "a", "ã", "a", "â", "a",
	"é", "e", "ê", "e", "í", "i", "ó", "o", "ô", "o", "õ", "o",
	"ú", "u", "ç", "c",
	"Á", "A", "À", "A", "Ã", "A", "Â", "A",
	"É", "E", "Ê", "E", "Í", "I", "Ó", "O", "Ô", "O", "Õ", "O",
	"Ú", "U", "Ç", "C",
)

// sizeNearMagnet returns the size written on a magnet's button: in the link
// itself or in the closest element around it that names exactly one size.
// An element naming several sizes holds other releases too, so the search
// stops there.
func sizeNearMagnet(a *goquery.Selection) string {
	node := a
	for depth := 0; depth < 4 && node.Length() > 0 && !node.Is("body"); depth++ {
		sizes := slices.Compact(findSizesFromText(node.Text()))
		switch len(sizes) {
		case 1:
			return sizes[0]
		case 0:
			node = node.Parent()
		default:
			return ""
		}
	}
	return ""
}

func cleanBaixarApidoTitle(raw string) string {
	if raw == "" {
		return ""
	}
	s := html.UnescapeString(raw)

	// Remove extensão
	s = reBAExtension.ReplaceAllString(s, "")

	// Normaliza pontos, underscores e +
	s = strings.NewReplacer(".", " ", "_", " ", "+", " ").Replace(s)

	// Remove prefixos de sites
	for _, re := range baixarApidoSitePrefixes {
		s = re.ReplaceAllString(s, "")
	}

	// Padrão genérico: remove qualquer "*TORRENT COM" ou "*TORRENTS COM" no início
	s = reBAGenericPrefix.ReplaceAllString(s, "")

	// Remove [UNSAFE]
	s = reBAUnsafe.ReplaceAllString(s, "")

	// Remove "EPISODIO XX" no início
	s = reBAEpisodePrefix.ReplaceAllString(s, "")

	// Remove tipo arquivo no início
	s = reBAFileTypeStart.ReplaceAllString(s, "")

	// Remove LEGENDADO/DUBLADO no início
	s = reBAAudioStart.ReplaceAllString(s, "")

	// Remove tags de qualidade no início
	s = stripReleaseTagPrefix(s)

	// Preserva tags com hífen
	placeholders := map[string]string{}
	for i, t := range baixarApidoHyphenTags {
		ph := fmt.Sprintf("⟦%d⟧", i)
		if strings.Contains(strings.ToUpper(s), t.tag) {
			s = t.re.ReplaceAllString(s, ph)
			placeholders[ph] = t.tag
		}
	}

	s = strings.ReplaceAll(s, "-", " ")
	s = reWhitespace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)

	for ph, tag := range placeholders {
		s = strings.ReplaceAll(s, ph, tag)
	}

	return strings.TrimSpace(s)
}

// isAbbreviatedTitle detecta títulos como "T l s S03E13" (dn abreviado do magnet)
func isAbbreviatedTitle(title string) bool {
	words := strings.Fields(title)
	if len(words) == 0 {
		return true
	}
	first := words[0]
	// Primeira palavra ≤ 2 letras e não é número/qualidade/season
	return len(first) <= 2 && !reBAAbbrevFirst.MatchString(first)
}

// needsTitlePrefix verifica se o título parece só ter info de episódio/qualidade
func needsTitlePrefix(title string) bool {
	return reBANeedsPrefix.MatchString(title)
}
