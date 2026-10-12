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
	defer func() {
		i.metrics.IndexerDuration.WithLabelValues("torrentfilmeshds").Observe(time.Since(start).Seconds())
	}()

	ctx := r.Context()
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
		_ = json.NewEncoder(w).Encode(Response{
			Results: []schema.IndexedTorrent{},
			Count:   0,
		})
		return
	}

	indexedTorrents := utils.ParallelFlatMap(links, func(link string) ([]schema.IndexedTorrent, error) {
		return getTorrentsTorrentFilmesHDs(ctx, i, link)
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

func getTorrentsTorrentFilmesHDs(ctx context.Context, i *Indexer, link string) ([]schema.IndexedTorrent, error) {
	doc, err := getDocument(ctx, i, link, torrentFilmesHDs.URL)
	if err != nil {
		return nil, err
	}

	title := strings.TrimSpace(doc.Find("h1, .entry-title").First().Text())
	if title == "" {
		title = strings.TrimSpace(doc.Find("title").Text())
	}

	year := ""
	if m := regexp.MustCompile(`(\d{4})`).FindStringSubmatch(title); len(m) > 0 {
		year = m[1]
	}

	imdbLink := ""
	doc.Find("a[href*='imdb.com']").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if imdb, err := getIMDBLink(href); err == nil {
			imdbLink = imdb
		}
	})

	var audio []schema.Audio
	var size []string
	doc.Find(".conteudo p, .entry-content p, article p").Each(func(_ int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())
		audio = append(audio, findAudioFromText(text)...)
		size = append(size, findSizesFromText(text)...)
		if y := findYearFromText(text, title); y != "" && year == "" {
			year = y
		}
	})
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

			releaseTitle := magnetParsed.DisplayName
			infoHash := magnetParsed.InfoHash.String()
			trackers := magnetParsed.Trackers

			magnetAudio := getAudioFromTitle(releaseTitle, audio)

			peer, seed, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, infoHash, trackers)
			if err != nil {
				peer, seed = 0, 0
			}

			var mySize string
			if it < len(size) {
				mySize = size[it]
			}

			ixt := schema.IndexedTorrent{
				Title:         releaseTitle,
				OriginalTitle: title,
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

var magnetRegex = regexp.MustCompile(`magnet:\?[^"'\s<>]+`)

func normalizeMagnet(mag string) string {
	mag = strings.TrimSpace(mag)
	mag = strings.ReplaceAll(mag, "&", "&")
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
