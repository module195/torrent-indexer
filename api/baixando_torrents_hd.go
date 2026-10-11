package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

var baixandoTorrentsHD = IndexerMeta{
	Label:       "baixando_torrents_hd",
	URL:         utils.GetIndexerURLFromEnv("INDEXER_BAIXANDO_TORRENTS_HD_URL", "https://baixandotorrentshd.org/"),
	SearchURL:   "?s=",
	PagePattern: "page/%s/",
}

func (i *Indexer) HandlerBaixandoTorrentsHDIndexer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	metadata := baixandoTorrentsHD

	defer func() {
		i.metrics.IndexerDuration.WithLabelValues(metadata.Label).Observe(time.Since(start).Seconds())
		i.metrics.IndexerRequests.WithLabelValues(metadata.Label).Inc()
	}()

	ctx := r.Context()
	q := r.URL.Query().Get("q")
	page := r.URL.Query().Get("page")

	q = url.QueryEscape(q)
	targetURL := metadata.URL

	if q != "" {
		targetURL = fmt.Sprintf("%s%s%s", metadata.URL, metadata.SearchURL, q)
		if page != "" && page != "1" {
			targetURL += "&paged=" + page
		}
	} else if page != "" && page != "1" {
		targetURL = fmt.Sprintf("%s%s", metadata.URL, fmt.Sprintf(metadata.PagePattern, page))
	}

	logging.InfoWithRequest(r).Str("target_url", targetURL).Msg("Processing indexer request")
	resp, err := i.requester.GetDocument(ctx, targetURL)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		err = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		if err != nil {
			logging.ErrorWithRequest(r).Err(err).Msg("Failed to encode error response")
		}
		i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
		return
	}
	defer resp.Close()

	doc, err := goquery.NewDocumentFromReader(resp)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		err = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		if err != nil {
			logging.ErrorWithRequest(r).Err(err).Msg("Failed to encode error response")
		}
		i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
		return
	}

	var links []string
	seen := make(map[string]bool)

	doc.Find(".lyra-lista .lyra-post a[href], .lyra-post-link[href]").Each(func(_ int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" || seen[href] {
			return
		}
		if strings.Contains(href, "/page/") || strings.Contains(href, "/category/") ||
			strings.Contains(href, "/tag/") || strings.Contains(href, "/author/") {
			return
		}
		seen[href] = true
		links = append(links, href)
	})

	if len(links) == 0 {
		_ = i.requester.ExpireDocument(ctx, targetURL)
		logging.DebugWithRequest(r).Str("url", targetURL).Msg("No links found, expiring cache")
	}

	indexedTorrents := utils.ParallelFlatMap(links, func(link string) ([]schema.IndexedTorrent, error) {
		return getTorrentsBaixandoTorrentsHD(ctx, i, link, targetURL)
	})

	postProcessedTorrents := indexedTorrents
	for _, processor := range i.postProcessors {
		postProcessedTorrents = processor(i, r, postProcessedTorrents)
	}

	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(Response{
		Results:      postProcessedTorrents,
		Count:        len(postProcessedTorrents),
		IndexedCount: len(indexedTorrents),
	})
	if err != nil {
		logging.Error().Err(err).Msg("Failed to encode response")
	}
}

// resolveGateLink tenta extrair o magnet a partir de um link de gate
func resolveGateLink(ctx context.Context, i *Indexer, gateURL, referer string) string {
	resp, err := i.requester.GetDocument(ctx, gateURL)
	if err != nil {
		logging.Debug().Err(err).Str("gate", gateURL).Msg("Failed to resolve gate")
		return ""
	}
	defer resp.Close()

	body, err := io.ReadAll(resp)
	if err != nil {
		return ""
	}

	content := string(body)

	// 1. Magnet direto no corpo
	if m := regexp.MustCompile(`magnet:\?[^"'\s<>]+`).FindString(content); m != "" {
		logging.Debug().Str("gate", gateURL).Str("magnet", m[:min(40, len(m))]).Msg("Magnet found in body")
		return m
	}

	// 2. Parsear HTML
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(content))
	if err != nil {
		return ""
	}

	// 2a. Link magnet
	if href, exists := doc.Find("a[href^='magnet:']").First().Attr("href"); exists && href != "" {
		logging.Debug().Str("gate", gateURL).Msg("Magnet found in <a> tag")
		return href
	}

	// 2b. Meta refresh
	if meta, exists := doc.Find("meta[http-equiv='refresh'], meta[http-equiv='Refresh']").Attr("content"); exists {
		if parts := strings.SplitN(strings.ToLower(meta), "url=", 2); len(parts) == 2 {
			redirect := strings.TrimSpace(parts[1])
			if strings.HasPrefix(redirect, "magnet:") {
				logging.Debug().Str("gate", gateURL).Msg("Magnet found in meta refresh")
				return redirect
			}
		}
	}

	// 2c. JavaScript window.location ou location.href
	if m := regexp.MustCompile(`(?:window\.)?location(?:\.href)?\s*=\s*['"](magnet:[^'"]+)['"]`).FindStringSubmatch(content); len(m) > 1 {
		logging.Debug().Str("gate", gateURL).Msg("Magnet found in JS location")
		return m[1]
	}

	// 2d. data-magnet ou outros atributos comuns
	if magnet, exists := doc.Find("[data-magnet], [data-href]").Attr("data-magnet"); exists && strings.HasPrefix(magnet, "magnet:") {
		return magnet
	}

	logging.Debug().
		Str("gate", gateURL).
		Int("body_len", len(content)).
		Str("body_sample", content[:min(300, len(content))]).
		Msg("No magnet found in gate response")

	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func getTorrentsBaixandoTorrentsHD(ctx context.Context, i *Indexer, link, referer string) ([]schema.IndexedTorrent, error) {
	var indexedTorrents []schema.IndexedTorrent
	doc, err := getDocument(ctx, i, link, referer)
	if err != nil {
		return nil, err
	}

	title := strings.TrimSpace(doc.Find("h1.lyra-h1, h1, .entry-title").First().Text())
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
	doc.Find(".lyra-ficha li, .lyra-rel-idioma, .lyra-rel-info").Each(func(_ int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())
		audio = append(audio, findAudioFromText(text)...)
		size = append(size, findSizesFromText(text)...)
		if y := findYearFromText(text, title); y != "" && year == "" {
			year = y
		}
	})
	size = utils.StableUniq(size)

	var magnetLinks []string

	// Magnets diretos
	doc.Find("a[href^='magnet:']").Each(func(_ int, s *goquery.Selection) {
		if href, exists := s.Attr("href"); exists && href != "" {
			magnetLinks = append(magnetLinks, href)
		}
	})

	// Resolver gates se não houver magnet direto
	if len(magnetLinks) == 0 {
		doc.Find("a.lyra-baixar[href], a[href*='/gate/go/']").Each(func(_ int, s *goquery.Selection) {
			if href, exists := s.Attr("href"); exists && href != "" {
				if magnet := resolveGateLink(ctx, i, href, link); magnet != "" {
					magnetLinks = append(magnetLinks, magnet)
				}
			}
		})
	}

	if len(magnetLinks) == 0 {
		return nil, nil
	}

	date := getPublishedDateFromMeta(doc)

	var chanIndexedTorrent = make(chan schema.IndexedTorrent)

	for it, magnetLink := range magnetLinks {
		it := it
		go func(it int, magnetLink string) {
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
				logging.Error().Err(err).Str("info_hash", infoHash).Msg("Failed to get leechers and seeders")
				peer, seed = 0, 0
			}

			processedTitle := processTitle(title, magnetAudio)

			var mySize string
			if len(size) == len(magnetLinks) {
				mySize = size[it]
			}
			if mySize == "" && i.magnetMetadataAPI != nil {
				go func() {
					_, _ = i.magnetMetadataAPI.FetchMetadata(ctx, magnetLink)
				}()
			}

			ixt := schema.IndexedTorrent{
				Title:         releaseTitle,
				OriginalTitle: processedTitle,
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
