package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/sync/singleflight"

	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/magnet"
	"github.com/module195/torrent-indexer/schema"
	goscrape "github.com/module195/torrent-indexer/scrape"
	"github.com/module195/torrent-indexer/utils"
)

var torrentFilmesHDs = IndexerMeta{
	Label:       "torrentfilmeshds",
	URL:         utils.GetIndexerURLFromEnv("INDEXER_TORRENTFILMESHDS_URL", "https://www.torrentfilmehds.org"),
	SearchURL:   "?s=",
	PagePattern: "page/%s/",
}

// singleflight para coalescer requests idênticos concorrentes
var tfhdsSF singleflight.Group

// Cache de resultado por query (TTL curto, complementa o cache de documentos do Redis)
const tfhdsResultCacheTTL = 5 * time.Minute

// Tempo máximo de uma busca compartilhada via singleflight
const tfhdsFetchTimeout = 2 * time.Minute

var imdbQueryRegex = regexp.MustCompile(`^tt\d{7,8}$`)

// isIMDBQuery checks if the query is an IMDB ID (e.g. "tt1234567")
func isIMDBQuery(q string) bool {
	return imdbQueryRegex.MatchString(strings.TrimSpace(q))
}
func (i *Indexer) HandlerTorrentFilmesHDsIndexer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		i.metrics.IndexerDuration.WithLabelValues("torrentfilmeshds").Observe(time.Since(start).Seconds())
	}()

	ctx := r.Context()
	q := r.URL.Query().Get("q")
	page := r.URL.Query().Get("page")
	if page == "" {
		page = "1"
	}

	// Skip IMDB queries - sites BR don't index by IMDB ID
	if isIMDBQuery(q) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Response{Results: []schema.IndexedTorrent{}, Count: 0})
		return
	}

	// Chave de cache/singleflight: query + página.
	// O cache guarda o resultado BRUTO (antes dos post-processors), pois os
	// post-processors dependem de parâmetros do request que não estão na chave.
	cacheKey := fmt.Sprintf("tfhds:raw:%s:%s", q, page)

	cacheStatus := "MISS"

	// 1. Tenta cache de resultado (Redis)
	raw, err := i.redis.Get(ctx, cacheKey)
	if err == nil && len(raw) > 0 {
		i.metrics.CacheHits.WithLabelValues("tfhds_result").Inc()
		cacheStatus = "HIT"
	} else {
		i.metrics.CacheMisses.WithLabelValues("tfhds_result").Inc()

		// 2. Singleflight: só uma execução por cacheKey concorrente.
		// O contexto não é cancelado se o primeiro cliente desconectar, para não
		// derrubar os demais requests que compartilham o resultado.
		v, err, shared := tfhdsSF.Do(cacheKey, func() (interface{}, error) {
			fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tfhdsFetchTimeout)
			defer cancel()
			resp, err := i.fetchTorrentFilmesHDs(fetchCtx, q, page)
			if err != nil {
				return nil, err
			}
			body, err := json.Marshal(resp)
			if err != nil {
				return nil, err
			}
			_ = i.redis.SetWithExpiration(fetchCtx, cacheKey, body, tfhdsResultCacheTTL)
			return body, nil
		})
		if err != nil {
			logging.Error().Err(err).Str("q", q).Msg("Failed to fetch torrentfilmeshds")
			http.Error(w, "Failed to fetch results", http.StatusInternalServerError)
			return
		}
		raw = v.([]byte)
		if shared {
			cacheStatus = "SHARED"
		}
	}

	// cada request decodifica sua própria cópia, evitando data race entre
	// post-processors de requests que compartilharam o singleflight
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		logging.Error().Err(err).Str("q", q).Msg("Failed to decode torrentfilmeshds result")
		http.Error(w, "Failed to fetch results", http.StatusInternalServerError)
		return
	}

	// Aplica post-processors no handler, onde o *http.Request existe
	torrents := resp.Results
	for _, processor := range i.postProcessors {
		torrents = processor(i, r, torrents)
	}
	resp.Results = torrents
	resp.Count = len(torrents)

	body, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "Failed to encode results", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", cacheStatus)
	_, _ = w.Write(body)
}

// fetchTorrentFilmesHDs faz o trabalho pesado (busca + scrape) isolado do handler HTTP.
func (i *Indexer) fetchTorrentFilmesHDs(ctx context.Context, q, page string) (Response, error) {
	var targetURL string
	if q != "" {
		targetURL = torrentFilmesHDs.URL + torrentFilmesHDs.SearchURL + url.QueryEscape(q)
		if page != "1" {
			targetURL += "&paged=" + page
		}
	} else if page != "1" {
		targetURL = torrentFilmesHDs.URL + fmt.Sprintf(torrentFilmesHDs.PagePattern, page)
	} else {
		targetURL = torrentFilmesHDs.URL
	}

	logging.Info().
		Str("target_url", targetURL).
		Str("q", q).
		Msg("Processing indexer request")

	doc, err := getDocument(ctx, i, targetURL, torrentFilmesHDs.URL)
	if err != nil {
		return Response{}, fmt.Errorf("failed to get document: %w", err)
	}

	links := extractTorrentFilmesHDsLinks(doc)

	// Fallback: se não achou nada e é uma busca, tenta queries simplificadas
	if len(links) == 0 && q != "" && page == "1" {
		for _, altQ := range buildTorrentFilmesHDsFallbackQueries(q) {
			altURL := torrentFilmesHDs.URL + torrentFilmesHDs.SearchURL + url.QueryEscape(altQ)
			logging.Info().
				Str("original_q", q).
				Str("fallback_q", altQ).
				Str("url", altURL).
				Msg("Trying fallback search")

			altDoc, err := getDocument(ctx, i, altURL, torrentFilmesHDs.URL)
			if err != nil {
				continue
			}
			links = extractTorrentFilmesHDsLinks(altDoc)
			if len(links) > 0 {
				logging.Info().
					Str("fallback_q", altQ).
					Int("links", len(links)).
					Msg("Fallback search found results")
				break
			}
		}
	}

	if len(links) == 0 {
		logging.Info().Str("url", targetURL).Msg("No links found")
		return Response{Results: []schema.IndexedTorrent{}, Count: 0}, nil
	}

	indexedTorrents := utils.ParallelFlatMap(links, func(link string) ([]schema.IndexedTorrent, error) {
		return getTorrentsTorrentFilmesHDs(ctx, i, link)
	})

	// Post-processors são aplicados no handler (onde o *http.Request existe)
	return Response{
		Results:      indexedTorrents,
		Count:        len(indexedTorrents),
		IndexedCount: len(indexedTorrents),
	}, nil
}

// buildTorrentFilmesHDsFallbackQueries gera buscas alternativas quando a original falha.
// Prioriza remoção de SxxEyy e não encurta abaixo de 3 palavras significativas.
func buildTorrentFilmesHDsFallbackQueries(original string) []string {
	return buildSmartFallbackQueries(original)
}

func extractTorrentFilmesHDsLinks(doc *goquery.Document) []string {
	seen := make(map[string]bool)
	var links []string

	doc.Find(".item > a[href]").Each(func(_ int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" {
			return
		}
		if seen[href] {
			return
		}
		if strings.Contains(href, "/genero/") || strings.Contains(href, "/page/") ||
			strings.Contains(href, "/category/") || strings.Contains(href, "/tag/") ||
			strings.Contains(href, "/author/") || strings.Contains(href, "xmlrpc") {
			return
		}
		if strings.Count(href, "/") <= 3 {
			return
		}
		seen[href] = true
		links = append(links, href)
	})

	return links
}

// cleanTorrentFilmesHDsTitle limpa o dn= do magnet para um título legível.
// Preserva tags com hífen (WEB-DL, DUAL-SF, etc.) e remove sufixo (brazilian).
func cleanTorrentFilmesHDsTitle(raw string) string { //nolint:unused // ainda não chamado; mantido para uso futuro
	if raw == "" {
		return ""
	}
	s := raw

	// Preserva tags conhecidas com hífen
	knownHyphen := []string{"WEB-DL", "WEB-RIP", "BD-RIP", "BD-R", "HD-RIP", "HD-TV", "DUAL-AUDIO", "DUAL-SF", "MULTI-AUDIO", "CAM-RIP"}
	placeholders := map[string]string{}
	for i, tag := range knownHyphen {
		ph := fmt.Sprintf("⟦%d⟧", i)
		if strings.Contains(strings.ToUpper(s), tag) {
			s = regexp.MustCompile(`(?i)`+regexp.QuoteMeta(tag)).ReplaceAllString(s, ph)
			placeholders[ph] = tag
		}
	}

	// Remove extensão de arquivo
	s = regexp.MustCompile(`(?i)\.(mkv|mp4|avi|torrent)$`).ReplaceAllString(s, "")

	// Limpa separadores
	s = strings.ReplaceAll(s, ".", " ")
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, "-", " ")
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)

	// Restaura tags com hífen
	for ph, tag := range placeholders {
		s = strings.ReplaceAll(s, ph, tag)
	}

	// Remove sufixo (brazilian)
	s = regexp.MustCompile(`(?i)\s*\(brazilian\)\s*$`).ReplaceAllString(s, "")

	return strings.TrimSpace(s)
}

func getTorrentsTorrentFilmesHDs(ctx context.Context, i *Indexer, link string) ([]schema.IndexedTorrent, error) {
	doc, err := getDocument(ctx, i, link, torrentFilmesHDs.URL)
	if err != nil {
		return nil, err
	}

	// Título: prioriza .titulo (o h1 do site é o logo)
	title := strings.TrimSpace(doc.Find(".titulo").First().Text())
	if title == "" {
		// Fallback: h1 que não seja o logo
		doc.Find("h1").Each(func(_ int, s *goquery.Selection) {
			if title != "" {
				return
			}
			cls, _ := s.Attr("class")
			if strings.Contains(cls, "logo") {
				return
			}
			title = strings.TrimSpace(s.Text())
		})
	}
	if title == "" {
		// Último recurso: title tag, removendo sufixos comuns
		rawTitle := strings.TrimSpace(doc.Find("title").Text())
		title = strings.TrimSuffix(rawTitle, " - TorrentFilmesHDs.org")
		title = strings.TrimSuffix(title, " | TorrentFilmesHDs.org")
		title = strings.TrimSpace(title)
	}

	year := ""
	if m := regexp.MustCompile(`(\d{4})`).FindStringSubmatch(title); len(m) > 0 {
		year = m[1]
	}

	// IMDB
	imdbLink := ""
	// 1. Links diretos
	doc.Find("a[href*='imdb.com/title']").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if m := regexp.MustCompile(`tt\d+`).FindString(href); m != "" {
			imdbLink = m
		}
	})
	// 2. Padrão ttXXXXXXX no texto
	if imdbLink == "" {
		if m := regexp.MustCompile(`tt\d{7,8}`).FindString(doc.Text()); m != "" {
			imdbLink = m
		}
	}
	// 3. Fallback: busca por título (limpa temporada/episódio/tags antes)
	if imdbLink == "" && title != "" {
		searchTitle := title
		searchTitle = regexp.MustCompile(`(?i)\s*\d+[ªºa]?\s*temporada\s*`).ReplaceAllString(searchTitle, " ")
		searchTitle = regexp.MustCompile(`(?i)\s*(torrent|download|dual|áudio|audio|dublado|legendado|brazilian)\s*`).ReplaceAllString(searchTitle, " ")
		searchTitle = regexp.MustCompile(`\([^)]*\)`).ReplaceAllString(searchTitle, " ")
		searchTitle = regexp.MustCompile(`\s+`).ReplaceAllString(strings.TrimSpace(searchTitle), " ")
		if fetched := fetchIMDBByTitle(searchTitle, ""); fetched != "" {
			imdbLink = fetched
		}
	}
	// Normaliza para URL completa
	if imdbLink != "" && !strings.HasPrefix(imdbLink, "http") {
		imdbLink = "https://www.imdb.com/title/" + imdbLink + "/"
	}

	var audio []schema.Audio
	var size []string
	// Busca em mais elementos como torrent-dos-filmes
	doc.Find("p, div, span, li, .conteudo, .entry-content, article").Each(func(_ int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())
		audio = append(audio, findAudioFromText(text)...)
		size = append(size, findSizesFromText(text)...)
		if y := findYearFromText(text, title); y != "" && year == "" {
			year = y
		}
	})
	// Também busca no título
	audio = append(audio, findAudioFromText(title)...)
	size = utils.StableUniq(size)

	// Collect magnets from link.php pages (paralelo)
	var linkPHPURLs, linkPHPLabels []string
	baseURL := strings.TrimRight(torrentFilmesHDs.URL, "/")
	doc.Find("a[href*='link.php']").Each(func(_ int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" {
			return
		}
		fullURL := href
		if strings.HasPrefix(href, "/") {
			fullURL = baseURL + href
		}
		linkPHPURLs = append(linkPHPURLs, fullURL)
		linkPHPLabels = append(linkPHPLabels, s.Text())
	})

	// rótulo do botão de cada magnet ("S01.1080p | MKV | Ep.01/05 | Dual Áudio")
	labels := map[string]string{}
	addLabel := func(mag, label string) {
		if _, ok := labels[mag]; !ok {
			labels[mag] = label
		}
	}

	// resolvidos em paralelo, mas na ordem da página (o tamanho é casado por posição)
	var magnetLinks []string
	if len(linkPHPURLs) > 0 {
		resolved := make([]string, len(linkPHPURLs))
		var wg sync.WaitGroup
		for idx, u := range linkPHPURLs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resolved[idx] = resolveLinkPHP(ctx, i, u)
			}()
		}
		wg.Wait()
		for idx, mag := range resolved {
			if mag != "" {
				magnetLinks = append(magnetLinks, mag)
				addLabel(mag, linkPHPLabels[idx])
			}
		}
	}

	// Also check for direct magnets
	doc.Find("a[href^='magnet:']").Each(func(_ int, s *goquery.Selection) {
		if href, exists := s.Attr("href"); exists && href != "" {
			magnetLinks = append(magnetLinks, href)
			addLabel(href, s.Text())
		}
	})

	magnetLinks = utils.StableUniq(magnetLinks)
	if len(magnetLinks) == 0 {
		return nil, nil
	}

	date := getPublishedDateFromMeta(doc)
	var indexedTorrents []schema.IndexedTorrent
	var chanIndexedTorrent = make(chan schema.IndexedTorrent)

	for it, magnetLink := range magnetLinks {
		it := it
		go func(it int, magnetLink string) {
			label := labels[magnetLink]
			magnetLink = normalizeMagnet(magnetLink)

			magnetParsed, err := magnet.ParseMagnetUri(magnetLink)
			if err != nil {
				logging.Error().Err(err).Str("magnet_link", magnetLink).Msg("Failed to parse magnet URI")
				chanIndexedTorrent <- schema.IndexedTorrent{}
				return
			}

			// Título do magnet sem limpar (mantém pontos como torrent-dos-filmes)
			releaseTitle := magnetParsed.DisplayName
			if releaseTitle == "" {
				// Fallback manual: extrai dn= da URL do magnet
				if m := regexp.MustCompile(`(?i)[?&]dn=([^&]+)`).FindStringSubmatch(magnetLink); len(m) > 1 {
					releaseTitle, _ = url.QueryUnescape(m[1])
					releaseTitle = strings.ReplaceAll(releaseTitle, "+", " ")
					releaseTitle = strings.TrimSpace(releaseTitle)
				}
			}
			if releaseTitle == "" {
				releaseTitle = title
			}

			// o site publica dn colado ("TedLassoS01E01-05720p"), que vira episódio errado
			// no parser; reconstrói pelo rótulo do botão ou descarta
			if isGluedRelease(releaseTitle) {
				rebuilt := releaseFromButtonLabel(title, label)
				if rebuilt == "" {
					logging.Debug().Str("title", releaseTitle).Str("label", label).Msg("Skipping glued release without usable label")
					chanIndexedTorrent <- schema.IndexedTorrent{}
					return
				}
				releaseTitle = rebuilt
			}

			infoHash := magnetParsed.InfoHash.String()
			trackers := magnetParsed.Trackers

			magnetAudio := getAudioFromTitle(releaseTitle, audio)

			// Fallback: detecta áudio pelo título se vazio
			if len(magnetAudio) == 0 {
				titleLower := strings.ToLower(releaseTitle)
				// Dual = Português + Inglês
				if strings.Contains(titleLower, "dual") {
					magnetAudio = append(magnetAudio, "Português", "Inglês")
				} else if strings.Contains(titleLower, "brazilian") || strings.Contains(titleLower, "pt-br") ||
					strings.Contains(titleLower, "portugues") || strings.Contains(titleLower, "dublado") {
					magnetAudio = append(magnetAudio, "Português")
				} else if strings.Contains(titleLower, "legendado") || strings.Contains(titleLower, "eng") ||
					strings.Contains(titleLower, "english") {
					magnetAudio = append(magnetAudio, "Inglês")
				}
			}

			// Title processado - usa processTitle como torrent-dos-filmes
			titleProcessed := processTitle(title, magnetAudio)

			peer, seed, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, infoHash, trackers)
			if err != nil {
				peer, seed = 0, 0
			}

			var mySize string
			if len(size) == len(magnetLinks) {
				mySize = size[it]
			}
			// Fallback: xl= do magnet (exact length em bytes)
			if mySize == "" {
				if xl := magnetParsed.Params.Get("xl"); xl != "" {
					if n, err := strconv.ParseInt(xl, 10, 64); err == nil && n > 0 {
						mySize = utils.FormatBytes(n)
					}
				}
			}

			ixt := schema.IndexedTorrent{
				Title:         releaseTitle,
				OriginalTitle: titleProcessed,
				Details:       link,
				Year:          year,
				IMDB:          imdbLink,
				Audio:         magnetAudio,
				MagnetLink:    magnetLink,
				Date:          date,
				InfoHash:      infoHash,
				Trackers:      trackers,
				LeechCount:    peer,
				SeedCount:     seed,
				Size:          mySize,
			}
			chanIndexedTorrent <- ixt
		}(it, magnetLink)
	}

	for i := 0; i < len(magnetLinks); i++ {
		it := <-chanIndexedTorrent
		if it.InfoHash != "" {
			indexedTorrents = append(indexedTorrents, it)
		}
	}

	return indexedTorrents, nil
}

var magnetRegex = regexp.MustCompile(`magnet:\?[^\"'\s<>]+`)

func normalizeMagnet(mag string) string {
	mag = strings.TrimSpace(mag)
	// Decodifica entidades HTML (necessário para extrair dn= e xl=)
	mag = strings.ReplaceAll(mag, "&amp;", "&")
	mag = strings.ReplaceAll(mag, "&#38;", "&")
	mag = strings.ReplaceAll(mag, "\n", "")
	mag = strings.ReplaceAll(mag, "\r", "")
	// Fix missing colon in urn:btih: (goquery strips it)
	mag = regexp.MustCompile(`xt=urnbtih`).ReplaceAllString(mag, "xt=urn:btih:")
	mag = regexp.MustCompile(`xt=urn:btih([^:])`).ReplaceAllString(mag, "xt=urn:btih:$1")
	return mag
}

func resolveLinkPHP(ctx context.Context, i *Indexer, linkURL string) string {
	doc, err := getDocument(ctx, i, linkURL, torrentFilmesHDs.URL)
	if err != nil {
		logging.Warn().Err(err).Str("url", linkURL).Msg("Failed to resolve link.php")
		return ""
	}

	html, err := doc.Html()
	if err != nil {
		return ""
	}

	matches := magnetRegex.FindAllString(html, -1)
	if len(matches) > 0 {
		return normalizeMagnet(matches[0])
	}

	return ""
}

var (
	reLabelSeason   = regexp.MustCompile(`(?i)\bS(\d{1,2})\b`)
	reLabelQuality  = regexp.MustCompile(`(?i)\b(\d{3,4}p|4k)\b`)
	reLabelEpisodes = regexp.MustCompile(`(?i)\bEp\.?\s*(\d{1,3})(?:\s*[/-]\s*(\d{1,3}))?`)
	reShowSeason    = regexp.MustCompile(`(?i)\s*\d+\s*[ªºa°]?\s*temporada.*$`)
	reShowYear      = regexp.MustCompile(`\s*\(\d{4}\).*$`)
	reNonNameChars  = regexp.MustCompile(`[^\p{L}\p{N}]+`)
)

// isGluedRelease reporta um dn sem separadores em volta do SxxEyy.
func isGluedRelease(title string) bool {
	return reGluedMarkerBefore.MatchString(title) || reGluedMarkerAfter.MatchString(title)
}

// releaseFromButtonLabel monta um nome de release a partir do título da página
// ("Ted Lasso 1ª Temporada (2020)") e do rótulo do botão do magnet
// ("S01.1080p | MKV | Ep.01/05 | Dual Áudio" -> "Ted.Lasso.S01E01-E05.1080p.MKV.DUAL").
// Retorna "" quando o rótulo não diz a temporada.
func releaseFromButtonLabel(pageTitle, label string) string {
	season := reLabelSeason.FindStringSubmatch(label)
	if season == nil {
		return ""
	}
	name := reShowYear.ReplaceAllString(reShowSeason.ReplaceAllString(pageTitle, ""), "")
	name = strings.Trim(reNonNameChars.ReplaceAllString(name, "."), ".")
	if name == "" {
		return ""
	}

	num := func(s string) int { n, _ := strconv.Atoi(s); return n }
	marker := fmt.Sprintf("S%02d", num(season[1]))
	if ep := reLabelEpisodes.FindStringSubmatch(label); ep != nil {
		marker += fmt.Sprintf("E%02d", num(ep[1]))
		if ep[2] != "" {
			marker += fmt.Sprintf("-E%02d", num(ep[2]))
		}
	}

	parts := []string{name, marker}
	if q := reLabelQuality.FindString(label); q != "" {
		parts = append(parts, q)
	}
	lower := strings.ToLower(label)
	for _, tag := range []string{"mkv", "mp4"} {
		if strings.Contains(lower, tag) {
			parts = append(parts, strings.ToUpper(tag))
		}
	}
	if strings.Contains(lower, "dual") {
		parts = append(parts, "DUAL")
	}
	return strings.Join(parts, ".")
}
