package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/felipemarinho97/torrent-indexer/logging"
	"github.com/felipemarinho97/torrent-indexer/magnet"
	"github.com/felipemarinho97/torrent-indexer/schema"
	goscrape "github.com/felipemarinho97/torrent-indexer/scrape"
	"github.com/felipemarinho97/torrent-indexer/utils"
)

var torrentFilmesHDs = IndexerMeta{
	Label:       "torrentfilmeshds",
	URL:         utils.GetIndexerURLFromEnv("INDEXER_TORRENTFILMESHDS_URL", "https://www.torrentfilmeshds.org"),
	SearchURL:   "?s=",
	PagePattern: "page/%s/",
}

func (i *Indexer) HandlerTorrentFilmesHDsIndexer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()
	defer func() {
		i.metrics.IndexerDuration.WithLabelValues("torrentfilmeshds").Observe(time.Since(start).Seconds())
	}()

	q := r.URL.Query().Get("q")
	page := r.URL.Query().Get("page")
	if page == "" {
		page = "1"
	}

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
		Str("url", r.URL.String()).
		Str("target_url", targetURL).
		Str("method", r.Method).
		Msg("Processing indexer request")

	doc, err := getDocument(ctx, i, targetURL, torrentFilmesHDs.URL)
	if err != nil {
		logging.Error().Err(err).Str("url", targetURL).Msg("Failed to get document")
		http.Error(w, "Failed to fetch page", http.StatusInternalServerError)
		return
	}

	links := extractTorrentFilmesHDsLinks(doc)
	if len(links) == 0 {
		logging.Info().Str("url", targetURL).Msg("No links found")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []schema.IndexedTorrent{},
			"count":   0,
		})
		return
	}

	results := utils.ParallelFlatMap(links, 10, func(link string) []schema.IndexedTorrent {
		return getTorrentsTorrentFilmesHDs(ctx, i, link)
	})

	results = i.postProcess(ctx, results)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"results":       results,
		"count":         len(results),
		"indexed_count": len(links),
	})
}

func extractTorrentFilmesHDsLinks(doc *goquery.Document) []string {
	seen := make(map[string]bool)
	var links []string

	doc.Find(".item > a[href]").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" {
			return
		}
		if seen[href] {
			return
		}
		// Skip navigation links
		if strings.Contains(href, "/genero/") || strings.Contains(href, "/page/") ||
			strings.Contains(href, "/category/") || strings.Contains(href, "/tag/") ||
			strings.Contains(href, "/author/") || strings.Contains(href, "xmlrpc") {
			return
		}
		// Only keep post links (have more than base domain)
		if strings.Count(href, "/") <= 3 {
			return
		}
		seen[href] = true
		links = append(links, href)
	})

	return links
}

func getTorrentsTorrentFilmesHDs(ctx context.Context, i *Indexer, link string) []schema.IndexedTorrent {
	doc, err := getDocument(ctx, i, link, torrentFilmesHDs.URL)
	if err != nil {
		logging.Error().Err(err).Str("url", link).Msg("Failed to get details page")
		return nil
	}

	title := strings.TrimSpace(doc.Find("h1, .entry-title, title").First().Text())
	if title == "" {
		title = strings.TrimSpace(doc.Find("title").Text())
	}

	year := findYearFromText(title, title)
	if year == "" {
		year = findYearFromText(doc.Find(".conteudo, .entry-content, article").Text(), title)
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

	contentText := doc.Find(".conteudo, .entry-content, article, .post-content").Text()
	audioFromContent := findAudioFromText(contentText)
	sizes := utils.StableUniq(findSizesFromText(contentText))

	date := getPublishedDateFromMeta(doc)

	// Extract magnets from link.php pages
	var magnets []string
	doc.Find("a[href*='link.php']").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" {
			return
		}
		fullURL := href
		if strings.HasPrefix(href, "/") {
			fullURL = torrentFilmesHDs.URL + href
		}
		mag := resolveLinkPHP(ctx, i, fullURL)
		if mag != "" {
			magnets = append(magnets, mag)
		}
	})

	// Also check for direct magnets
	doc.Find("a[href^='magnet:']").Each(func(i int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if href != "" {
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

var magnetRegex = regexp.MustCompile(`magnet:\?[^"'\s<>]+`)

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
		// Clean HTML entities
		mag := matches[0]
		mag = strings.ReplaceAll(mag, "&", "&")
		return mag
	}

	return ""
}
