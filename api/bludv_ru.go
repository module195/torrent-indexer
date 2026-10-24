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

	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/magnet"
	"github.com/module195/torrent-indexer/schema"
	goscrape "github.com/module195/torrent-indexer/scrape"
	"github.com/module195/torrent-indexer/utils"
)

var bludvRu = IndexerMeta{
	Label:       "bludv_ru",
	URL:         utils.GetIndexerURLFromEnv("INDEXER_BLUDV_RU_URL", "https://bludv.ru"),
	SearchURL:   "?s=",
	PagePattern: "page/%s/",
}

func (i *Indexer) HandlerBludvRuIndexer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		i.metrics.IndexerDuration.WithLabelValues("bludv_ru").Observe(time.Since(start).Seconds())
	}()

	ctx := r.Context()
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

	// Fallback inteligente
	if len(links) == 0 && q != "" && page == "1" {
		for _, altQ := range buildSmartFallbackQueries(q) {
			altURL := bludvRu.URL + bludvRu.SearchURL + url.QueryEscape(altQ)
			logging.Info().
				Str("original_q", q).
				Str("fallback_q", altQ).
				Str("url", altURL).
				Msg("Trying fallback search")

			altDoc, err := getDocument(ctx, i, altURL, bludvRu.URL)
			if err != nil {
				continue
			}
			links = extractBludvRuLinks(altDoc)
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
		_ = json.NewEncoder(w).Encode(Response{
			Results: []schema.IndexedTorrent{},
			Count:   0,
		})
		return
	}

	indexedTorrents := utils.ParallelFlatMap(links, func(link string) ([]schema.IndexedTorrent, error) {
		return getTorrentsBludvRu(ctx, i, link)
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

func extractBludvRuLinks(doc *goquery.Document) []string {
	seen := make(map[string]bool)
	var links []string

	doc.Find(".post .title a[href]").Each(func(_ int, s *goquery.Selection) {
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

func getTorrentsBludvRu(ctx context.Context, i *Indexer, link string) ([]schema.IndexedTorrent, error) {
	doc, err := getDocument(ctx, i, link, bludvRu.URL)
	if err != nil {
		return nil, err
	}

	title := strings.TrimSpace(doc.Find("h1, .title, .entry-title").First().Text())
	if title == "" {
		title = strings.TrimSpace(doc.Find("title").Text())
	}

	year := reYearWord.FindString(title)

	imdbLink := ""
	doc.Find("a[href*='imdb.com']").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if imdb, err := getIMDBLink(href); err == nil {
			imdbLink = imdb
		}
	})

	var audio []schema.Audio
	var size []string
	doc.Find(".conteudo p, .entry-content p, article p, .post p").Each(func(_ int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())
		audio = append(audio, findAudioFromText(text)...)
		size = append(size, findSizesFromText(text)...)
		if y := findYearFromText(text, title); y != "" && year == "" {
			year = y
		}
	})
	size = utils.StableUniq(size)

	var magnetLinks []string
	doc.Find("a[href^='magnet:']").Each(func(_ int, s *goquery.Selection) {
		if href, exists := s.Attr("href"); exists && href != "" {
			href = strings.ReplaceAll(href, "&", "&")
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
			magnetLink = strings.TrimSpace(magnetLink)
			magnetLink = strings.ReplaceAll(magnetLink, "\n", "")
			magnetLink = strings.ReplaceAll(magnetLink, "\r", "")
			magnetLink = strings.ReplaceAll(magnetLink, "&", "&")

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
			if len(size) == len(magnetLinks) {
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
