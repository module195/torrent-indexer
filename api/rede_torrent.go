package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/magnet"
	"github.com/module195/torrent-indexer/schema"
	goscrape "github.com/module195/torrent-indexer/scrape"
	"github.com/module195/torrent-indexer/utils"
)

var rede_torrent = IndexerMeta{
	Label:       "rede_torrent",
	URL:         utils.GetIndexerURLFromEnv("INDEXER_REDE_TORRENT_URL", "https://redestorrents.com/"),
	SearchURL:   "index.php?busca=",
	PagePattern: "pagina/%s/",
}

// redeTorrentPageConcurrency limita quantas páginas de título são abertas ao
// mesmo tempo (a busca traz 20).
const redeTorrentPageConcurrency = 6

func (i *Indexer) HandlerRedeTorrentIndexer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	metadata := rede_torrent

	defer func() {
		i.metrics.IndexerDuration.WithLabelValues(metadata.Label).Observe(time.Since(start).Seconds())
		i.metrics.IndexerRequests.WithLabelValues(metadata.Label).Inc()
	}()

	ctx := r.Context()
	q := r.URL.Query().Get("q")
	page := r.URL.Query().Get("page")

	targetURL := redeTorrentListingURL(metadata, q, page)

	logging.InfoWithRequest(r).Str("target_url", targetURL).Msg("Processing indexer request")

	links, err := fetchRedeTorrentLinks(ctx, i, targetURL)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
		return
	}

	// Fallback: se não achou nada e é busca, tenta queries simplificadas
	if len(links) == 0 && q != "" && (page == "" || page == "1") {
		for _, altQ := range buildRedeTorrentFallbackQueries(q) {
			altURL := redeTorrentListingURL(metadata, altQ, "")
			logging.Info().
				Str("original_q", q).
				Str("fallback_q", altQ).
				Str("url", altURL).
				Msg("Trying fallback search")

			altLinks, err := fetchRedeTorrentLinks(ctx, i, altURL)
			if err != nil {
				continue
			}
			if len(altLinks) > 0 {
				links = altLinks
				break
			}
		}
	}

	if len(links) == 0 {
		_ = i.requester.ExpireDocument(ctx, targetURL)
	}

	sem := make(chan struct{}, redeTorrentPageConcurrency)
	indexedTorrents := utils.ParallelFlatMap(links, func(link string) ([]schema.IndexedTorrent, error) {
		sem <- struct{}{}
		defer func() { <-sem }()
		return getTorrentsRedeTorrent(ctx, i, link, targetURL)
	})

	postProcessedTorrents := indexedTorrents
	for _, processor := range i.postProcessors {
		postProcessedTorrents = processor(i, r, postProcessedTorrents)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Response{
		Results:      postProcessedTorrents,
		Count:        len(postProcessedTorrents),
		IndexedCount: len(indexedTorrents),
	})
}

// redeTorrentListingURL monta a URL da busca ou da listagem. A busca vai para
// index.php?busca=q, que é a 1ª página de /pesquisa/q/; as seguintes só
// existem em /pesquisa/q/pagina/N/. O espaço vai como "+": com "%20" o
// site respondeu 403.
func redeTorrentListingURL(metadata IndexerMeta, q, page string) string {
	base := strings.TrimRight(metadata.URL, "/") + "/"
	paged := page != "" && page != "1"
	switch {
	case q != "" && paged:
		return base + "pesquisa/" + url.QueryEscape(strings.ToLower(q)) + "/" + fmt.Sprintf(metadata.PagePattern, page)
	case q != "":
		return base + metadata.SearchURL + url.QueryEscape(q)
	case paged:
		return base + fmt.Sprintf(metadata.PagePattern, page)
	}
	return base
}

func fetchRedeTorrentLinks(ctx context.Context, i *Indexer, targetURL string) ([]string, error) {
	// a busca e a home mudam o tempo todo: fora do cache de 24 h
	doc, err := getListingDocument(ctx, i, targetURL, "")
	if err != nil {
		return nil, err
	}
	return extractRedeTorrentLinks(doc), nil
}

// extractRedeTorrentLinks devolve as páginas de título de uma listagem: os
// cards (a.cover-link). O domínio tem "torrent" no nome, então um seletor
// por href pegaria também o menu (home, /filmes/, /qualidade/4k/...).
func extractRedeTorrentLinks(doc *goquery.Document) []string {
	var links []string
	seen := map[string]bool{}
	add := func(_ int, s *goquery.Selection) {
		href := strings.TrimSpace(s.AttrOr("href", ""))
		if href == "" || seen[href] {
			return
		}
		seen[href] = true
		links = append(links, href)
	}
	doc.Find("a.cover-link[href]").Each(add)
	if len(links) == 0 {
		// Fallback antigo
		doc.Find(".capa_lista a").Each(add)
	}
	return links
}

// fetchIMDBByTitle busca IMDB por título usando APIs gratuitas
func fetchIMDBByTitle(title, year string) string {
	// Limpa título: remove tags de qualidade, áudio, formato
	cleanTitle := regexp.MustCompile(`(?i)\s*[-:]?\s*(torrent|download|dublado|legendado|dual|audio|mkv|mp4|1080p|720p|4k|bluray|web-dl|webrip|camrip|x264|x265|hevc|aac|5\.1|remake).*$`).ReplaceAllString(title, "")
	cleanTitle = regexp.MustCompile(`(?i)\s*\b(19|20)\d{2}\b\s*`).ReplaceAllString(cleanTitle, " ")
	cleanTitle = regexp.MustCompile(`\s+`).ReplaceAllString(strings.TrimSpace(cleanTitle), " ")
	cleanTitle = strings.Trim(cleanTitle, " -:")
	if cleanTitle == "" || len(cleanTitle) < 2 {
		return ""
	}

	client := &http.Client{Timeout: 5 * time.Second}

	// Tenta OMDb com ano
	if year != "" {
		omdbURL := fmt.Sprintf("https://www.omdbapi.com/?t=%s&y=%s&apikey=trilogy", url.QueryEscape(cleanTitle), year)
		if resp, err := client.Get(omdbURL); err == nil {
			defer resp.Body.Close()
			var result struct {
				IMDBID string `json:"imdbID"`
				Title  string `json:"Title"`
			}
			if json.NewDecoder(resp.Body).Decode(&result) == nil && result.IMDBID != "" {
				// Valida se o título retornado é similar
				if strings.Contains(strings.ToLower(result.Title), strings.ToLower(cleanTitle[:min(len(cleanTitle), 10)])) {
					return "https://www.imdb.com/title/" + result.IMDBID + "/"
				}
			}
		}
	}

	// Tenta OMDb sem ano
	omdbURL := fmt.Sprintf("https://www.omdbapi.com/?t=%s&apikey=trilogy", url.QueryEscape(cleanTitle))
	if resp, err := client.Get(omdbURL); err == nil {
		defer resp.Body.Close()
		var result struct {
			IMDBID string `json:"imdbID"`
			Title  string `json:"Title"`
		}
		if json.NewDecoder(resp.Body).Decode(&result) == nil && result.IMDBID != "" {
			if strings.Contains(strings.ToLower(result.Title), strings.ToLower(cleanTitle[:min(len(cleanTitle), 10)])) {
				return "https://www.imdb.com/title/" + result.IMDBID + "/"
			}
		}
	}

	// Fallback: TVmaze (series)
	tvmazeURL := fmt.Sprintf("https://api.tvmaze.com/singlesearch/shows?q=%s", url.QueryEscape(cleanTitle))
	if resp, err := client.Get(tvmazeURL); err == nil {
		defer resp.Body.Close()
		var result struct {
			Name      string `json:"name"`
			Externals struct {
				IMDB string `json:"imdb"`
			} `json:"externals"`
		}
		if json.NewDecoder(resp.Body).Decode(&result) == nil && result.Externals.IMDB != "" {
			if strings.Contains(strings.ToLower(result.Name), strings.ToLower(cleanTitle[:min(len(cleanTitle), 10)])) {
				return "https://www.imdb.com/title/" + result.Externals.IMDB + "/"
			}
		}
	}

	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func getTorrentsRedeTorrent(ctx context.Context, i *Indexer, link, referer string) ([]schema.IndexedTorrent, error) {
	doc, err := getDocument(ctx, i, link, referer)
	if err != nil {
		return nil, err
	}

	page, releases := parseRedeTorrentPage(doc)
	if page.Title == "" {
		return nil, fmt.Errorf("could not extract title from %s", link)
	}
	// sem magnet não vale a consulta ao OMDb
	if len(releases) == 0 {
		return nil, nil
	}

	imdbLink := page.IMDB
	if imdbLink == "" {
		imdbLink = fetchIMDBByTitle(redeTorrentIMDBSearch(page))
	}
	if imdbLink != "" && !strings.HasPrefix(imdbLink, "http") {
		imdbLink = "https://www.imdb.com/title/" + imdbLink + "/"
	}

	var indexedTorrents []schema.IndexedTorrent
	var chanIndexedTorrent = make(chan schema.IndexedTorrent)

	for _, rel := range releases {
		go func(rel redeTorrentRelease) {
			peer, seed, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, rel.InfoHash, rel.Trackers)
			if err != nil {
				peer, seed = 0, 0
			}
			if rel.Size == "" {
				go func() {
					_, _ = i.magnetMetadataAPI.FetchMetadata(ctx, rel.MagnetLink)
				}()
			}

			chanIndexedTorrent <- schema.IndexedTorrent{
				Title:         rel.Title,
				OriginalTitle: processTitle(page.Title, rel.Audio),
				Details:       link,
				Year:          page.Year,
				IMDB:          imdbLink,
				Audio:         rel.Audio,
				MagnetLink:    rel.MagnetLink,
				Date:          page.Date,
				InfoHash:      rel.InfoHash,
				Trackers:      rel.Trackers,
				LeechCount:    peer,
				SeedCount:     seed,
				Size:          rel.Size,
			}
		}(rel)
	}

	for range releases {
		indexedTorrents = append(indexedTorrents, <-chanIndexedTorrent)
	}

	return indexedTorrents, nil
}

// redeTorrentPage é o que a página diz do título inteiro.
type redeTorrentPage struct {
	Title, OriginalTitle, Year, IMDB string
	Series                           bool
	Date                             time.Time
}

// redeTorrentRelease é uma linha de download da página.
type redeTorrentRelease struct {
	Title, MagnetLink, InfoHash, Size string
	Trackers                          []string
	Audio                             []schema.Audio
}

var (
	reRedeTitleTail   = regexp.MustCompile(`(?i)\s*(torrent|download|dublado|legendado|dual|audio).*$`)
	reRedeYearParens  = regexp.MustCompile(`\s*\(\d{4}\)\s*`)
	reRedeIMDBID      = regexp.MustCompile(`tt\d{7,8}`)
	reRedeSeasonTitle = regexp.MustCompile(`(?i)[\s:–-]*\b(?:\d{1,2}\s*[ªaº]?\s*temporada|season\s*\d{1,2}|s\d{1,2})\b.*$`)
)

// parseRedeTorrentPage lê uma página de título. Os dados do título estão no
// JSON-LD (Movie ou TVSeries: name, sameAs com o IMDb, datePublished) e na
// ficha (.spec-text: "<small>Idioma / Áudio</small><strong>Português /
// Inglês</strong>", sem dois-pontos). Cada versão é uma .download-row com o
// rótulo (.download-name) e o magnet.
func parseRedeTorrentPage(doc *goquery.Document) (redeTorrentPage, []redeTorrentRelease) {
	var page redeTorrentPage
	var ldName, ldYear string
	doc.Find(`script[type="application/ld+json"]`).Each(func(_ int, s *goquery.Selection) {
		var ld struct {
			Type          string `json:"@type"`
			Name          string `json:"name"`
			SameAs        string `json:"sameAs"`
			CopyrightYear string `json:"copyrightYear"`
		}
		if json.Unmarshal([]byte(s.Text()), &ld) != nil {
			return
		}
		switch ld.Type {
		case "Movie", "TVSeries":
			ldName = strings.TrimSpace(ld.Name)
			ldYear = ld.CopyrightYear
			page.IMDB = reRedeIMDBID.FindString(ld.SameAs)
			page.Series = ld.Type == "TVSeries"
		}
	})

	// Title: h1 ou title tag - limpa para melhor similarity
	title := strings.TrimSpace(doc.Find("h1").First().Text())
	if title == "" {
		title = strings.TrimSpace(doc.Find("title").Text())
		title = regexp.MustCompile(`(?i)\s*[-|].*$`).ReplaceAllString(title, "")
	}
	title = reRedeTitleTail.ReplaceAllString(title, "")
	title = strings.TrimSpace(reRedeYearParens.ReplaceAllString(title, " "))
	if ldName != "" {
		title = ldName
	}
	page.Title = html.UnescapeString(title)
	page.OriginalTitle = strings.TrimSpace(doc.Find("p.original-title").First().Text())

	page.Year = strings.TrimSpace(doc.Find(".info-pill time[itemprop='datePublished']").First().Text())
	if page.Year == "" {
		page.Year = ldYear
	}
	if page.Year == "" {
		page.Year = reYearWord.FindString(page.Title)
	}
	if page.IMDB == "" {
		if href, ok := doc.Find("a[href*='imdb.com/title']").First().Attr("href"); ok {
			page.IMDB = reRedeIMDBID.FindString(href)
		}
	}
	page.Date = getPublishedDateFromMeta(doc)

	var audio []schema.Audio
	var sizes []string
	doc.Find(".spec-text").Each(func(_ int, s *goquery.Selection) {
		label := strings.ToLower(s.Find("small").Text())
		value := strings.TrimSpace(s.Find("strong").Text())
		switch {
		case strings.Contains(label, "idioma") || strings.Contains(label, "áudio"):
			for _, lang := range strings.Split(value, "/") {
				if a := schema.GetAudioFromString(strings.TrimSpace(lang)); a != nil {
					audio = append(audio, *a)
				}
			}
		case strings.Contains(label, "tamanho"):
			sizes = append(sizes, findSizesFromText(value)...)
		}
	})

	type row struct{ magnetLink, label string }
	var rows []row
	seen := map[string]bool{}
	addRow := func(a *goquery.Selection, label string) {
		href := strings.TrimSpace(a.AttrOr("href", ""))
		if !strings.HasPrefix(href, "magnet:") || seen[href] {
			return
		}
		seen[href] = true
		rows = append(rows, row{href, strings.Join(strings.Fields(label), " ")})
	}
	doc.Find(".download-row").Each(func(_ int, s *goquery.Selection) {
		label := s.Find(".download-name").Text()
		s.Find("a[href^='magnet:']").Each(func(_ int, a *goquery.Selection) { addRow(a, label) })
	})
	doc.Find("a[href^='magnet:']").Each(func(_ int, a *goquery.Selection) { addRow(a, "") })

	pageTitle := cleanRedeTorrentTitle(page.Title)
	var releases []redeTorrentRelease
	for k, rw := range rows {
		magnetLink := strings.NewReplacer("magnet:? ", "magnet:?", "&amp;", "&").Replace(rw.magnetLink)
		magnetParsed, err := magnet.ParseMagnetUri(magnetLink)
		if err != nil {
			logging.Error().Err(err).Str("magnet_link", magnetLink).Msg("Failed to parse magnet URI")
			continue
		}

		releaseTitle := cleanRedeTorrentTitle(magnetParsed.DisplayName)
		if releaseTitle == "" {
			releaseTitle = pageTitle
		}
		// dn só com episódio/qualidade ("S02E01 1080p", "T l s S03E13"): põe o título
		if (isAbbreviatedTitle(releaseTitle) || needsTitlePrefix(releaseTitle)) && pageTitle != "" &&
			!strings.Contains(strings.ToLower(releaseTitle), strings.ToLower(pageTitle)) {
			releaseTitle = pageTitle + " " + releaseTitle
		}
		// dn colado ("OndeEstaWandaS02E01WEB-DL1080px264DUAL5.1"): o título da
		// página sem a temporada mais o episódio; o rótulo completa a qualidade
		if isGluedRelease(releaseTitle) {
			if ep := reGluedMarker.FindString(releaseTitle); ep != "" && pageTitle != "" {
				name := cleanRedeTorrentTitle(strings.TrimSpace(reRedeSeasonTitle.ReplaceAllString(page.Title, "")))
				releaseTitle = name + " " + ep
			}
		}
		releaseTitle = withLabelTags(releaseTitle, rw.label)

		audioTitle := releaseTitle
		if hint := bludvRuLabelAudio(rw.label); hint != "" && !hasAudioMarker(releaseTitle) {
			audioTitle += " " + hint
		}

		size := releaseSize(magnetLink, releaseTitle)
		if size == "" {
			size = sizeFromButtonLabel(rw.label)
		}
		// tamanho da ficha pelo índice só com um por magnet; senão ele iria
		// para um release de outra qualidade
		if size == "" && len(sizes) == len(rows) {
			size = sizes[k]
		}

		releases = append(releases, redeTorrentRelease{
			Title:      releaseTitle,
			MagnetLink: magnetLink,
			InfoHash:   magnetParsed.InfoHash.String(),
			Trackers:   magnetParsed.Trackers,
			Audio:      getAudioFromTitle(audioTitle, audio),
			Size:       size,
		})
	}
	return page, releases
}

// redeTorrentIMDBSearch escolhe título e ano para achar o IMDb quando a página
// não traz: o título original sem a temporada ("Lupin S04" -> "Lupin",
// "Wheres Wanda? Season 2" -> "Wheres Wanda?"). Em série o ano é o da
// temporada, não o da estreia, então vai sem ano.
func redeTorrentIMDBSearch(page redeTorrentPage) (title, year string) {
	title = page.OriginalTitle
	if title == "" {
		title = page.Title
	}
	if page.Series {
		return strings.TrimSpace(reRedeSeasonTitle.ReplaceAllString(title, "")), ""
	}
	return title, page.Year
}

// buildRedeTorrentFallbackQueries gera buscas alternativas quando a original falha.
func buildRedeTorrentFallbackQueries(q string) []string {
	var queries []string
	seen := map[string]bool{}

	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		queries = append(queries, s)
	}

	// Remove números de temporada/episódio
	cleaned := regexp.MustCompile(`(?i)\s*s\d{1,2}e\d{1,2}\b`).ReplaceAllString(q, "")
	cleaned = regexp.MustCompile(`(?i)\s*s\d{1,2}\b`).ReplaceAllString(cleaned, "")
	cleaned = regexp.MustCompile(`(?i)\s*\d{1,2}x\d{1,2}\b`).ReplaceAllString(cleaned, "")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned != q {
		add(cleaned)
	}

	// Primeira palavra (pula artigos: buscar "the" ou "o" sozinho traz o site inteiro)
	parts := strings.Fields(q)
	if len(parts) > 1 {
		first := parts[0]
		if reLeadingArticle.MatchString(first+" ") && len(parts) > 2 {
			first = parts[1]
		}
		if !reLeadingArticle.MatchString(first + " ") {
			add(first)
		}
	}

	// Duas primeiras palavras
	if len(parts) > 2 {
		add(strings.Join(parts[:2], " "))
	}

	return queries
}

func cleanRedeTorrentTitle(raw string) string {
	if raw == "" {
		return ""
	}
	s := stripBaixarApidoTagBlock(html.UnescapeString(raw))

	// Remove extensão
	s = regexp.MustCompile(`(?i)\.(mkv|mp4|avi|torrent)$`).ReplaceAllString(s, "")

	// Normaliza pontos e underscores
	s = strings.ReplaceAll(s, ".", " ")
	s = strings.ReplaceAll(s, "_", " ")

	// Remove prefixos de sites
	sitePrefixes := []string{
		"BAIXARAPIDO COM", "SITEDETORRENTS COM", "HIDRATORRENTS ORG",
		"VAMOSTORRENT COM", "TORRENTFILMESHDS", "BLUDV", "REDE TORRENT",
	}
	for _, prefix := range sitePrefixes {
		s = regexp.MustCompile(`(?i)^`+regexp.QuoteMeta(prefix)+`\s*[-.]?\s*`).ReplaceAllString(s, "")
	}

	// Remove padrões de episódio no início (EPISODIO-04, EPISODIOS-01-E-02, E-02, etc)
	episodePrefix := `(?i)^[-.\s]*(EPISODIO[S]?|EP)[\s.-]*\d+([\s.-]*[E]\s*\d+)?[\s.-]*`
	s = regexp.MustCompile(episodePrefix).ReplaceAllString(s, "")
	// Remove restos como "E 02" no início
	s = regexp.MustCompile(`(?i)^[-.\s]*E[\s.-]*\d+[\s.-]*`).ReplaceAllString(s, "")

	// Remove tags de qualidade/áudio no início (múltiplas passadas)
	s = stripReleaseTagPrefix(s)

	// Preserva tags com hífen no meio do título
	knownHyphen := []string{"WEB-DL", "WEB-RIP", "BD-RIP", "DUAL-AUDIO", "CAM-RIP"}
	placeholders := map[string]string{}
	for i, tag := range knownHyphen {
		ph := fmt.Sprintf("⟦%d⟧", i)
		if strings.Contains(strings.ToUpper(s), tag) {
			s = regexp.MustCompile(`(?i)`+regexp.QuoteMeta(tag)).ReplaceAllString(s, ph)
			placeholders[ph] = tag
		}
	}

	s = strings.ReplaceAll(s, "-", " ")
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)

	for ph, tag := range placeholders {
		s = strings.ReplaceAll(s, ph, tag)
	}

	return strings.TrimSpace(s)
}
