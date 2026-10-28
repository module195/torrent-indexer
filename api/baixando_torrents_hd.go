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

	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/magnet"
	"github.com/module195/torrent-indexer/schema"
	goscrape "github.com/module195/torrent-indexer/scrape"
	"github.com/module195/torrent-indexer/utils"
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

// extractMagnetFromContent procura magnet em texto/HTML de várias formas
func extractMagnetFromContent(content string) string {
	// Magnet direto
	if m := regexp.MustCompile(`magnet:\?[^"'\s<>]+`).FindString(content); m != "" {
		return m
	}

	// JS location / window.location
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?:window\.)?location(?:\.href)?\s*=\s*['"](magnet:[^'"]+)['"]`),
		regexp.MustCompile(`(?:window\.)?location\.replace\s*\(\s*['"](magnet:[^'"]+)['"]\s*\)`),
		regexp.MustCompile(`document\.location\s*=\s*['"](magnet:[^'"]+)['"]`),
		regexp.MustCompile(`href\s*=\s*['"](magnet:[^'"]+)['"]`),
	}
	for _, re := range patterns {
		if m := re.FindStringSubmatch(content); len(m) > 1 {
			return m[1]
		}
	}

	return ""
}

// resolveGateLink tenta seguir a cadeia de gates de forma mais agressiva
func resolveGateLink(ctx context.Context, i *Indexer, gateURL, referer string) string {
	visited := make(map[string]bool)
	current := gateURL

	for step := 0; step < 6; step++ {
		if visited[current] {
			break
		}
		visited[current] = true

		resp, err := i.requester.GetDocument(ctx, current)
		if err != nil {
			logging.Debug().Err(err).Str("url", current).Int("step", step).Msg("Gate request failed")
			break
		}

		body, err := io.ReadAll(resp)
		resp.Close()
		if err != nil {
			break
		}

		content := string(body)

		// Tenta extrair magnet do conteúdo atual
		if magnet := extractMagnetFromContent(content); magnet != "" {
			logging.Debug().Str("from", current).Int("step", step).Msg("Magnet extracted")
			return magnet
		}

		doc, err := goquery.NewDocumentFromReader(strings.NewReader(content))
		if err != nil {
			break
		}

		// Procura link magnet direto
		if href, exists := doc.Find("a[href^='magnet:']").First().Attr("href"); exists && href != "" {
			return href
		}

		// Meta refresh
		if meta, exists := doc.Find("meta[http-equiv='refresh'], meta[http-equiv='Refresh']").Attr("content"); exists {
			lower := strings.ToLower(meta)
			if idx := strings.Index(lower, "url="); idx != -1 {
				redirect := strings.TrimSpace(meta[idx+4:])
				redirect = strings.Trim(redirect, `"'`)
				if strings.HasPrefix(redirect, "magnet:") {
					return redirect
				}
				if strings.HasPrefix(redirect, "http") {
					current = redirect
					continue
				}
			}
		}

		// Procura próximo hop (botão "Vamos começar", Continuar, etc.)
		nextURL := ""
		doc.Find("a[href]").EachWithBreak(func(_ int, s *goquery.Selection) bool {
			href, _ := s.Attr("href")
			text := strings.ToLower(strings.TrimSpace(s.Text()))
			if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") {
				return true
			}
			// Prioriza botões de continuar / começar / jump
			if strings.Contains(text, "vamos começar") || strings.Contains(text, "continuar") ||
				strings.Contains(text, "começar") || strings.Contains(href, "/jump/") ||
				strings.Contains(href, "bolsofeliz") || strings.Contains(href, "x.gd") ||
				strings.Contains(href, "garimpou") {
				nextURL = href
				return false
			}
			return true
		})

		if nextURL == "" {
			// Pega o primeiro link externo que não seja do mesmo domínio
			doc.Find("a[href^='http']").EachWithBreak(func(_ int, s *goquery.Selection) bool {
				href, _ := s.Attr("href")
				if href != "" && !strings.Contains(href, "baixandotorrentshd") {
					nextURL = href
					return false
				}
				return true
			})
		}

		if nextURL == "" {
			logging.Debug().Str("url", current).Int("step", step).Msg("No next hop found")
			break
		}

		// Resolve URL relativa
		if !strings.HasPrefix(nextURL, "http") {
			base, err := url.Parse(current)
			if err == nil {
				ref, err := url.Parse(nextURL)
				if err == nil {
					nextURL = base.ResolveReference(ref).String()
				}
			}
		}

		logging.Debug().Str("from", current).Str("to", nextURL).Int("step", step).Msg("Following gate hop")
		current = nextURL
	}

	logging.Debug().Str("gate", gateURL).Msg("Failed to resolve magnet after all hops")
	return ""
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
				logging.Debug().Err(err).Str("info_hash", infoHash).Msg("Failed to get leechers and seeders")
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
