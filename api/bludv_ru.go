package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/felipemarinho97/torrent-indexer/logging"
	"github.com/felipemarinho97/torrent-indexer/magnet"
	"github.com/felipemarinho97/torrent-indexer/schema"
	goscrape "github.com/felipemarinho97/torrent-indexer/scrape"
	"github.com/felipemarinho97/torrent-indexer/utils"
)

var bludvRu = IndexerMeta{
	Label:       "bludv_ru",
	URL:         utils.GetIndexerURLFromEnv("INDEXER_BLUDV_RU_URL", "https://bludv.ru"),
	SearchURL:   "?s=",
	PagePattern: "page/%s/",
}

func (i *Indexer) HandlerBludvRuIndexer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()
	defer func() {
		i.metrics.IndexerDuration.WithLabelValues("bludv_ru").Observe(time.Since(start).Seconds())
	}()

	q := r.URL.Query().Get("q")
	page := r.URL.Query().Get("page")
	if page == "" {
		page = "1"
	}

	var targetURL string
	if q != "" {
		targetURL = bludvRu.URL + bludvRu.SearchURL + url.QueryEscape(q)
		if page != "1" {
			targetURL += "&paged=" + page
		}
	} else if page != "1" {
		targetURL = bludvRu.URL + fmt.Sprintf(bludvRu.PagePattern, page)
	} else {
		targetURL = bludvRu.URL
	}

	logging.Info().
		Str("url", r.URL.String()).
		Str("target_url", targetURL).
		Str("method", r.Method).
		Msg("Processing indexer request")

	doc, err := getDocument(ctx, i, targetURL, bludvRu.URL)
	if err != nil {
		logging.Error().Err(err).Str("url", targetURL).Msg("Failed to get document")
		http.Error(w, "Failed to fetch page", http.StatusInternalServerError)
		return
	}

	links := extractBludvRuLinks(doc)
	if len(links) == 0 {
		logging.Info().Str("url", targetURL).Msg("No links found")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []schema.IndexedTorrent{},
			"count":   0,
		})
		return
	}

	results := utils.ParallelFlatMap(links, 10, func(link string) []schema.IndexedTorrent {
		return getTorrentsBludvRu(ctx, i, link)
	})

	results = i.postProcess(ctx, results)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"results":       results,
		"count":         len(results),
		"indexed_count": len(links),
	})
}

func extractBludvRuLinks(doc *goquery.Document) []string {
	seen := make(map[string]bool)
	var links []string

	doc.Find(".post .title a[href]").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" {
			return
		}
		if seen[href] {
			return
		}
		if strings.Contains(href, "/category/") || strings.Contains(href, "/tag/") ||
			strings.Contains(href, "/author/") || strings.Contains(href, "/page/") ||
			strings.Contains(href, "xmlrpc") || strings.Contains(href, "/feed/") {
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

func getTorrentsBludvRu(ctx context.Context, i *Indexer, link string) []schema.IndexedTorrent {
	doc, err := getDocument(ctx, i, link, bludvRu.URL)
	if err != nil {
		logging.Error().Err(err).Str("url", link).Msg("Failed to get details page")
		return nil
	}

	title := strings.TrimSpace(doc.Find("h1, .title, .entry-title").First().Text())
	if title == "" {
		title = strings.TrimSpace(doc.Find("title").Text())
	}

	year := findYearFromText(title, title)
	if year == "" {
		year = findYearFromText(doc.Find(".conteudo, .entry-content, article, .post").Text(), title)
	}

	var imdbLink string
	doc.Find("a[href*='imdb.com']").EachWithBreak(func(i int, s *goquery.Selection) bool {
		href, _ := s.Attr("href")
		if href != "" {
			imdbLink, _ = getIMDBLink(href)
			return false
		}
		return true
	})

	contentText := doc.Find(".conteudo, .entry-content, article, .post-content, .post").Text()
	audioFromContent := findAudioFromText(contentText)
	sizes := utils.StableUniq(findSizesFromText(contentText))

	date := getPublishedDateFromMeta(doc)

	// Extract direct magnets
	var magnets []string
	doc.Find("a[href^='magnet:']").Each(func(i int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if href != "" {
			// Clean HTML entities
			href = strings.ReplaceAll(href, "&", "&")
			magnets = append(magnets, href)
		}
	})

	magnets = utils.StableUniq(magnets)
	if len(magnets) == 0 {
		return nil
	}

	var results []schema.IndexedTorrent
	for idx, magLink := range magnets {
		parsed, err := magnet.ParseMagnetUri(magLink)
		if err != nil {
			logging.Warn().Err(err).Str("magnet", magLink).Msg("Failed to parse magnet")
			continue
		}

		audio := getAudioFromTitle(parsed.DisplayName, audioFromContent)
		size := ""
		if idx < len(sizes) {
			size = sizes[idx]
		}

		leech, seed := goscrape.GetLeechsAndSeeds(parsed.InfoHash)

		results = append(results, schema.IndexedTorrent{
			Title:         parsed.DisplayName,
			OriginalTitle: title,
			Details:       link,
			Year:          year,
			IMDB:          imdbLink,
			Audio:         audio,
			MagnetLink:    magLink,
			Date:          date,
			InfoHash:      parsed.InfoHash,
			Trackers:      parsed.Trackers,
			LeechCount:    leech,
			SeedCount:     seed,
			Size:          size,
		})
	}

	return results
}
