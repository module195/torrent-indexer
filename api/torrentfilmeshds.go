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
	URL:         utils.GetIndexerURLFromEnv("INDEXER_TORRENTFILMESHDS_URL", "https://www.torrentfilmeshds.org"),
	SearchURL:   "?s=",
	PagePattern: "page/%s/",
}

// singleflight para coalescer requests idênticos concorrentes
var tfhdsSF singleflight.Group

// Cache de resultado por query (TTL curto, complementa o cache de documentos do Redis)
const tfhdsResultCacheTTL = 5 * time.Minute
// isIMDBQuery checks if the query is an IMDB ID (e.g. "tt1234567")
func isIMDBQuery(q string) bool {
	return regexp.MustCompile(`^tt\d{7,8}$`).MatchString(strings.TrimSpace(q))
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

	// Chave de cache/singleflight: query + página
	cacheKey := fmt.Sprintf("tfhds:result:%s:%s", q, page)

	// 1. Tenta cache de resultado (Redis)
	if cached, err := i.redis.Get(ctx, cacheKey); err == nil && len(cached) > 0 {
		i.metrics.CacheHits.WithLabelValues("tfhds_result").Inc()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "HIT")
		_, _ = w.Write(cached)
		return
	}
	i.metrics.CacheMisses.WithLabelValues("tfhds_result").Inc()

	// 2. Singleflight: só uma execução por cacheKey concorrente
	v, err, shared := tfhdsSF.Do(cacheKey, func() (interface{}, error) {
		return i.fetchTorrentFilmesHDs(ctx, q, page)
	})
	if err != nil {
		logging.Error().Err(err).Str("q", q).Msg("Failed to fetch torrentfilmeshds")
		http.Error(w, "Failed to fetch results", http.StatusInternalServerError)
		return
	}

	resp := v.(Response)

	// Aplica post-processors no handler, onde o *http.Request existe
	torrents := resp.Results
	for _, processor := range i.postProcessors {
		torrents = processor(i, r, torrents)
	}
	resp.Results = torrents
	resp.Count = len(torrents)

	// 3. Serializa e cacheia o resultado
	body, err := json.Marshal(resp)
	if err == nil {
		_ = i.redis.Set(ctx, cacheKey, body)
	}

	w.Header().Set("Content-Type", "application/json")
	if shared {
		w.Header().Set("X-Cache", "SHARED")
	} else {
		w.Header().Set("X-Cache", "MISS")
	}
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
// Prioriza versões sem pontuação e sem indicadores de episódio/temporada.
func buildTorrentFilmesHDsFallbackQueries(original string) []string {
	var out []string
	seen := map[string]bool{}

	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || s == original || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	// 1. Remove dois-pontos e outros separadores
	cleaned := strings.NewReplacer(":", " ", "|", " ", "–", " ", "—", " ").Replace(original)
	cleaned = regexp.MustCompile(`\s+`).ReplaceAllString(cleaned, " ")
	add(cleaned)

	// 2. Remove padrões de episódio/temporada
	noEp := regexp.MustCompile(`(?i)\s*(S\d{1,2}E\d{1,2}|S\d{1,2}|temporada\s*\d+|episodio\s*\d+|episódio\s*\d+|season\s*\d+|episode\s*\d+)\s*`).ReplaceAllString(cleaned, " ")
	noEp = regexp.MustCompile(`\s+`).ReplaceAllString(strings.TrimSpace(noEp), " ")
	add(noEp)

	// 3. Só as primeiras 2-3 palavras significativas (útil para títulos longos)
	words := strings.Fields(noEp)
	if len(words) > 3 {
		add(strings.Join(words[:3], " "))
	}
	if len(words) > 2 {
		add(strings.Join(words[:2], " "))
	}

	// Limita a 3 tentativas para não sobrecarregar
	if len(out) > 3 {
		out = out[:3]
	}
	return out
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
func cleanTorrentFilmesHDsTitle(raw string) string {
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

	// Collect magnets from link.php pages
	var magnetLinks []string
	doc.Find("a[href*='link.php']").Each(func(_ int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" {
			return
		}
		fullURL := href
		if strings.HasPrefix(href, "/") {
			fullURL = torrentFilmesHDs.URL + href
		}
		if mag := resolveLinkPHP(ctx, i, fullURL); mag != "" {
			magnetLinks = append(magnetLinks, mag)
		}
	})

	// Also check for direct magnets
	doc.Find("a[href^='magnet:']").Each(func(_ int, s *goquery.Selection) {
		if href, exists := s.Attr("href"); exists && href != "" {
			magnetLinks = append(magnetLinks, href)
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
			if it < len(size) {
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
