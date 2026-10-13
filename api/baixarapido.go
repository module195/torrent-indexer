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
		targetURL = baixarapido.URL + baixarapido.SearchURL + url.PathEscape(q) + "/"
		if page != "1" {
			targetURL = baixarapido.URL + fmt.Sprintf(baixarapido.PagePattern, url.PathEscape(q), page)
		}
	} else {
		targetURL = baixarapido.URL
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

	// Fallback: se não achou nada e é uma busca, tenta queries simplificadas
	if len(links) == 0 && q != "" && page == "1" {
		for _, altQ := range buildBaixarApidoFallbackQueries(q) {
			altURL := baixarapido.URL + baixarapido.SearchURL + url.PathEscape(altQ) + "/"
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

	resp := schema.SearchResponse{
		Count:   len(allTorrents),
		Results: allTorrents,
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

	// Year
	yearRe := regexp.MustCompile(`\((\d{4})\)|\b(19|20)\d{2}\b`)
	year := ""
	if m := yearRe.FindStringSubmatch(h1); len(m) > 1 {
		year = m[1]
	}
	if year == "" {
		if m := yearRe.FindStringSubmatch(doc.Text()); len(m) > 1 {
			year = m[1]
		}
	}

	// IMDB
	imdb := ""
	doc.Find("a[href*='imdb.com/title']").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if m := regexp.MustCompile(`tt\d+`).FindString(href); m != "" {
			imdb = m
		}
	})

	// Audio
	bodyText := doc.Text()
	audio := findAudioFromText(h1 + " " + bodyText)

	// Size
	sizeRe := regexp.MustCompile(`(\d+(?:[.,]\d+)?\s*(?:GB|MB|TB))`)
	size := sizeRe.FindString(bodyText)

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

		releaseTitle := cleanBaixarApidoTitle(magnetParsed.DisplayName)
		if releaseTitle == "" {
			releaseTitle = h1
		}

		// Scrape seeders
		seed, leech := 0, 0
		if peer, s, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, magnetParsed.InfoHash.String(), magnetParsed.Trackers); err == nil {
			seed = s
			leech = peer
		}

		results = append(results, schema.IndexedTorrent{
			Title:         releaseTitle,
			OriginalTitle: h1,
			Details:       link,
			Year:          year,
			IMDB:          imdb,
			Audio:         audio,
			MagnetLink:    magnetLink,
			Date:          date,
			InfoHash:      magnetParsed.InfoHash.String(),
			Trackers:      magnetParsed.Trackers,
			Size:          size,
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

	s = strings.ReplaceAll(s, ".", " ")
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, "-", " ")
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)

	for ph, tag := range placeholders {
		s = strings.ReplaceAll(s, ph, tag)
	}

	return strings.TrimSpace(s)
}
