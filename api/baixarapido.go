package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
	PagePattern: "/pesquisa/%s/page/%s/",
}

func (i *Indexer) HandlerBaixarApidoIndexer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		i.metrics.IndexerDuration.WithLabelValues("baixarapido").Observe(time.Since(start).Seconds())
	}()

	ctx := r.Context()
	q := r.URL.Query().Get("q")
	page := r.URL.Query().Get("page")
	if page == "" {
		page = "1"
	}

	var targetURL string
	if q != "" {
		// Remove trailing slash do base e leading slash do SearchURL para evitar //
		base := strings.TrimRight(baixarapido.URL, "/")
		// O site usa + para espaços
		encoded := strings.ReplaceAll(url.PathEscape(q), "%20", "+")
		targetURL = base + "/pesquisa/" + encoded + "/"
		if page != "1" {
			targetURL = base + fmt.Sprintf("/pesquisa/%s/page/%s/", encoded, page)
		}
	} else {
		targetURL = strings.TrimRight(baixarapido.URL, "/")
	}

	logging.Info().
		Str("url", r.URL.String()).
		Str("target_url", targetURL).
		Str("method", r.Method).
		Msg("Processing indexer request")

	doc, err := getDocument(ctx, i, targetURL, baixarapido.URL)
	if err != nil {
		logging.Error().Err(err).Str("url", targetURL).Msg("Failed to get document")
		http.Error(w, "Failed to fetch page", http.StatusInternalServerError)
		return
	}

	links := extractBaixarApidoLinks(doc)
	logging.Info().
		Int("links_found", len(links)).
		Str("url", targetURL).
		Msg("Extracted links")

	// Fallback: se não achou nada e é uma busca, tenta queries simplificadas
	if len(links) == 0 && q != "" && page == "1" {
		for _, altQ := range buildBaixarApidoFallbackQueries(q) {
			base := strings.TrimRight(baixarapido.URL, "/")
			encoded := strings.ReplaceAll(url.PathEscape(altQ), "%20", "+")
			altURL := base + "/pesquisa/" + encoded + "/"
			logging.Info().
				Str("original_q", q).
				Str("fallback_q", altQ).
				Str("url", altURL).
				Msg("Trying fallback search")

			altDoc, err := getDocument(ctx, i, altURL, baixarapido.URL)
			if err != nil {
				continue
			}
			links = extractBaixarApidoLinks(altDoc)
			if len(links) > 0 {
				break
			}
		}
	}

	var allTorrents []schema.IndexedTorrent
	for _, link := range links {
		torrents, err := getTorrentsBaixarApido(ctx, i, link)
		if err != nil {
			logging.Error().Err(err).Str("link", link).Msg("Failed to get torrents")
			continue
		}
		allTorrents = append(allTorrents, torrents...)
	}

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

	// Aplica post-processors (similarity, etc)
	postProcessedTorrents := allTorrents
	for _, processor := range i.postProcessors {
		postProcessedTorrents = processor(i, r, postProcessedTorrents)
	}

	resp := Response{
		Count:        len(postProcessedTorrents),
		IndexedCount: len(allTorrents),
		Results:      postProcessedTorrents,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func buildBaixarApidoFallbackQueries(original string) []string {
	var queries []string
	// Remove ano
	noYear := regexp.MustCompile(`\b(19|20)\d{2}\b`).ReplaceAllString(original, "")
	noYear = strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(noYear, " "))
	if noYear != "" && noYear != original {
		queries = append(queries, noYear)
	}
	// Primeiras 3 palavras
	words := strings.Fields(original)
	if len(words) > 3 {
		queries = append(queries, strings.Join(words[:3], " "))
	}
	return queries
}

func extractBaixarApidoLinks(doc *goquery.Document) []string {
	var links []string
	seen := map[string]bool{}
	doc.Find("article.movie-card").Each(func(_ int, s *goquery.Selection) {
		// O link é o pai do article
		parent := s.Parent()
		if parent.Is("a") {
			href, exists := parent.Attr("href")
			if exists && href != "" && !seen[href] {
				seen[href] = true
				links = append(links, href)
			}
		}
	})
	return links
}

func getTorrentsBaixarApido(ctx context.Context, i *Indexer, link string) ([]schema.IndexedTorrent, error) {
	doc, err := getDocument(ctx, i, link, baixarapido.URL)
	if err != nil {
		return nil, err
	}

	h1 := strings.TrimSpace(doc.Find("h1").First().Text())
	if h1 == "" {
		return nil, fmt.Errorf("no title found at %s", link)
	}
	h1 = regexp.MustCompile(`(?i)\s*torrent.*$`).ReplaceAllString(h1, "")
	h1 = strings.TrimSpace(h1)

	// Year - usa findYearFromText como torrent-dos-filmes
	year := findYearFromText(doc.Text(), h1)
	if year == "" {
		// Fallback: regex direto
		if m := regexp.MustCompile(`\((\d{4})\)`).FindStringSubmatch(h1); len(m) > 1 {
			year = m[1]
		}
		if year == "" {
			if m := regexp.MustCompile(`\b(19|20)\d{2}\b`).FindStringSubmatch(doc.Text()); len(m) > 0 {
				year = m[0]
			}
		}
	}

	// IMDB
	imdb := ""
	// 1. Links diretos
	doc.Find("a[href*='imdb.com/title']").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if m := regexp.MustCompile(`tt\d+`).FindString(href); m != "" {
			imdb = m
		}
	})
	// 2. Padrão ttXXXXXXX no texto
	if imdb == "" {
		if m := regexp.MustCompile(`tt\d{7,8}`).FindString(doc.Text()); m != "" {
			imdb = m
		}
	}
	// 3. Fallback: busca por título
	if imdb == "" && h1 != "" {
		if fetched := fetchIMDBByTitle(h1, year); fetched != "" {
			imdb = fetched
		}
	}
	// Normaliza para URL completa
	if imdb != "" && !strings.HasPrefix(imdb, "http") {
		imdb = "https://www.imdb.com/title/" + imdb + "/"
	}

	// Audio - usa findAudioFromText como torrent-dos-filmes
	var audio []schema.Audio
	doc.Find("p, div, span, li").Each(func(i int, s *goquery.Selection) {
		audio = append(audio, findAudioFromText(s.Text())...)
	})
	audio = append(audio, findAudioFromText(h1)...)

	// Fallback: mapeia keywords manualmente
	bodyText := doc.Text()
	allText := strings.ToLower(h1 + " " + bodyText)
	hasPortugues := false
	hasIngles := false
	for _, a := range audio {
		if a == "Português" {
			hasPortugues = true
		}
		if a == "Inglês" {
			hasIngles = true
		}
	}
	// Legendado/eng = Inglês
	if !hasIngles && (strings.Contains(allText, "legendado") || strings.Contains(allText, "eng") || strings.Contains(allText, "english")) {
		audio = append(audio, "Inglês")
	}
	// Dublado/brazilian/pt-br/dual = Português
	if !hasPortugues && (strings.Contains(allText, "dublado") || strings.Contains(allText, "brazilian") || strings.Contains(allText, "pt-br") || strings.Contains(allText, "portugues") || strings.Contains(allText, "dual")) {
		audio = append(audio, "Português")
	}

	// Size - usa findSizesFromText como torrent-dos-filmes
	var sizes []string
	doc.Find("p, div, span").Each(func(i int, s *goquery.Selection) {
		sizes = append(sizes, findSizesFromText(s.Text())...)
	})
	size := ""
	if len(sizes) > 0 {
		size = sizes[0]
	}

	// Date
	date := getPublishedDateFromMeta(doc)

	// Magnets
	var magnetLinks []string
	doc.Find("a[href^='magnet:']").Each(func(_ int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if exists && href != "" {
			magnetLinks = append(magnetLinks, href)
		}
	})

	if len(magnetLinks) == 0 {
		return nil, nil
	}

	var results []schema.IndexedTorrent
	for _, magnetLink := range magnetLinks {
		magnetLink = normalizeMagnet(magnetLink)
		magnetParsed, err := magnet.ParseMagnetUri(magnetLink)
		if err != nil {
			continue
		}

		// Título do magnet - limpa prefixo do site
		releaseTitle := magnetParsed.DisplayName
		if releaseTitle == "" {
			releaseTitle = h1
		}
		// Remove prefixo BAIXARAPIDO.COM-...-
		releaseTitle = regexp.MustCompile(`(?i)^BAIXARAPIDO\.COM-.*?-\.`).ReplaceAllString(releaseTitle, "")
		releaseTitle = strings.TrimSpace(releaseTitle)

		// Audio do magnet - usa getAudioFromTitle como torrent-dos-filmes
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
		titleProcessed := processTitle(h1, magnetAudio)

		// Size do magnet xl= se não encontrou no texto
		mySize := size
		if mySize == "" {
			if xl := regexp.MustCompile(`[?&]xl=(\d+)`).FindStringSubmatch(magnetLink); len(xl) > 1 {
				if bytes, err := strconv.ParseInt(xl[1], 10, 64); err == nil && bytes > 0 {
					gb := float64(bytes) / (1024 * 1024 * 1024)
					if gb >= 1 {
						mySize = fmt.Sprintf("%.2f GB", gb)
					} else {
						mySize = fmt.Sprintf("%.0f MB", float64(bytes)/(1024*1024))
					}
				}
			}
		}

		// Scrape seeders
		seed, leech := 0, 0
		if peer, s, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, magnetParsed.InfoHash.String(), magnetParsed.Trackers); err == nil {
			seed = s
			leech = peer
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
			SeedCount:     seed,
			LeechCount:    leech,
		})
	}

	return results, nil
}

func cleanBaixarApidoTitle(raw string) string {
	if raw == "" {
		return ""
	}
	s := raw

	// Remove extensão
	s = regexp.MustCompile(`(?i)\.(mkv|mp4|avi|torrent)$`).ReplaceAllString(s, "")

	// Normaliza pontos e underscores ANTES de checar prefixos
	s = strings.ReplaceAll(s, ".", " ")
	s = strings.ReplaceAll(s, "_", " ")

	// Remove prefixos de sites
	sitePrefixes := []string{
		"BAIXARAPIDO COM", "SITEDETORRENTS COM", "HIDRATORRENTS ORG",
		"VAMOSTORRENT COM", "TORRENTFILMESHDS", "BLUDV",
	}
	for _, prefix := range sitePrefixes {
		s = regexp.MustCompile(`(?i)^`+regexp.QuoteMeta(prefix)+`\s*`).ReplaceAllString(s, "")
	}

	// Preserva tags com hífen
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
