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

	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/magnet"
	"github.com/module195/torrent-indexer/schema"
	goscrape "github.com/module195/torrent-indexer/scrape"
	"github.com/module195/torrent-indexer/utils"
)

var rede_torrent = IndexerMeta{
	Label:       "rede_torrent",
	URL:         utils.GetIndexerURLFromEnv("INDEXER_REDE_TORRENT_URL", "https://redestorrents.com/"),
	SearchURL:   "index.php?busca=",
	PagePattern: "pagina/%s/",
}

func (i *Indexer) HandlerRedeTorrentIndexer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	metadata := rede_torrent

	defer func() {
		i.metrics.IndexerDuration.WithLabelValues(metadata.Label).Observe(time.Since(start).Seconds())
		i.metrics.IndexerRequests.WithLabelValues(metadata.Label).Inc()
	}()

	ctx := r.Context()
	q := r.URL.Query().Get("q")
	page := r.URL.Query().Get("page")

	// Build initial URL
	targetURL := metadata.URL
	if q != "" {
		targetURL = fmt.Sprintf("%s%s%s", metadata.URL, metadata.SearchURL, url.QueryEscape(q))
	} else if page != "" {
		targetURL = fmt.Sprintf(fmt.Sprintf("%s%s", metadata.URL, metadata.PagePattern), page)
	}

	logging.InfoWithRequest(r).Str("target_url", targetURL).Msg("Processing indexer request")

	links, err := fetchRedeTorrentLinks(ctx, i, targetURL)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
		return
	}

	// Fallback: se não achou nada e é busca, tenta queries simplificadas
	if len(links) == 0 && q != "" && page == "" {
		for _, altQ := range buildRedeTorrentFallbackQueries(q) {
			altURL := fmt.Sprintf("%s%s%s", metadata.URL, metadata.SearchURL, url.QueryEscape(altQ))
			logging.Info().
				Str("original_q", q).
				Str("fallback_q", altQ).
				Str("url", altURL).
				Msg("Trying fallback search")

			altLinks, err := fetchRedeTorrentLinks(ctx, i, altURL)
			if err != nil {
				continue
			}
			if len(altLinks) > 0 {
				links = altLinks
				logging.Info().
					Str("fallback_q", altQ).
					Int("links", len(links)).
					Msg("Fallback search found results")
				break
			}
		}
	}

	if len(links) == 0 {
		_ = i.requester.ExpireDocument(ctx, targetURL)
	}

	indexedTorrents := utils.ParallelFlatMap(links, func(link string) ([]schema.IndexedTorrent, error) {
		return getTorrentsRedeTorrent(ctx, i, link, targetURL)
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

func fetchRedeTorrentLinks(ctx context.Context, i *Indexer, targetURL string) ([]string, error) {
	resp, err := i.requester.GetDocument(ctx, targetURL)
	if err != nil {
		return nil, err
	}
	defer resp.Close()

	doc, err := goquery.NewDocumentFromReader(resp)
	if err != nil {
		return nil, err
	}

	var links []string
	seen := map[string]bool{}

	// Novo seletor (site atual)
	doc.Find("a[href*='torrent']").Each(func(_ int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" || seen[href] {
			return
		}
		// Filtra links de navegação e pesquisa
		if strings.Contains(href, "/genero/") || strings.Contains(href, "/pagina/") ||
			strings.Contains(href, "/pesquisa/") || strings.Contains(href, "?") {
			return
		}
		seen[href] = true
		links = append(links, href)
	})

	// Fallback: a.cover-link
	if len(links) == 0 {
		doc.Find("a.cover-link").Each(func(_ int, s *goquery.Selection) {
			href, exists := s.Attr("href")
			if !exists || href == "" || seen[href] {
				return
			}
			seen[href] = true
			links = append(links, href)
		})
	}

	// Fallback antigo
	if len(links) == 0 {
		doc.Find(".capa_lista a").Each(func(_ int, s *goquery.Selection) {
			href, exists := s.Attr("href")
			if !exists || href == "" || seen[href] {
				return
			}
			seen[href] = true
			links = append(links, href)
		})
	}

	return links, nil
}

// fetchIMDBByTitle busca IMDB por título usando APIs gratuitas
func fetchIMDBByTitle(title, year string) string {
	// Limpa título: remove tags de qualidade, áudio, formato
	cleanTitle := regexp.MustCompile(`(?i)\s*[-:]?\s*(torrent|download|dublado|legendado|dual|audio|mkv|mp4|1080p|720p|4k|bluray|web-dl|webrip|camrip|x264|x265|hevc|aac|5\.1|remake).*$`).ReplaceAllString(title, "")
	cleanTitle = regexp.MustCompile(`(?i)\s*\b(19|20)\d{2}\b\s*`).ReplaceAllString(cleanTitle, " ")
	cleanTitle = regexp.MustCompile(`\s+`).ReplaceAllString(strings.TrimSpace(cleanTitle), " ")
	cleanTitle = strings.Trim(cleanTitle, " -:")
	if cleanTitle == "" || len(cleanTitle) < 2 {
		return ""
	}

	client := &http.Client{Timeout: 5 * time.Second}

	// Tenta OMDb com ano
	if year != "" {
		omdbURL := fmt.Sprintf("https://www.omdbapi.com/?t=%s&y=%s&apikey=trilogy", url.QueryEscape(cleanTitle), year)
		if resp, err := client.Get(omdbURL); err == nil {
			defer resp.Body.Close()
			var result struct {
				IMDBID string `json:"imdbID"`
				Title  string `json:"Title"`
			}
			if json.NewDecoder(resp.Body).Decode(&result) == nil && result.IMDBID != "" {
				// Valida se o título retornado é similar
				if strings.Contains(strings.ToLower(result.Title), strings.ToLower(cleanTitle[:min(len(cleanTitle), 10)])) {
					return "https://www.imdb.com/title/" + result.IMDBID + "/"
				}
			}
		}
	}

	// Tenta OMDb sem ano
	omdbURL := fmt.Sprintf("https://www.omdbapi.com/?t=%s&apikey=trilogy", url.QueryEscape(cleanTitle))
	if resp, err := client.Get(omdbURL); err == nil {
		defer resp.Body.Close()
		var result struct {
			IMDBID string `json:"imdbID"`
			Title  string `json:"Title"`
		}
		if json.NewDecoder(resp.Body).Decode(&result) == nil && result.IMDBID != "" {
			if strings.Contains(strings.ToLower(result.Title), strings.ToLower(cleanTitle[:min(len(cleanTitle), 10)])) {
				return "https://www.imdb.com/title/" + result.IMDBID + "/"
			}
		}
	}

	// Fallback: TVmaze (series)
	tvmazeURL := fmt.Sprintf("https://api.tvmaze.com/singlesearch/shows?q=%s", url.QueryEscape(cleanTitle))
	if resp, err := client.Get(tvmazeURL); err == nil {
		defer resp.Body.Close()
		var result struct {
			Name      string `json:"name"`
			Externals struct {
				IMDB string `json:"imdb"`
			} `json:"externals"`
		}
		if json.NewDecoder(resp.Body).Decode(&result) == nil && result.Externals.IMDB != "" {
			if strings.Contains(strings.ToLower(result.Name), strings.ToLower(cleanTitle[:min(len(cleanTitle), 10)])) {
				return "https://www.imdb.com/title/" + result.Externals.IMDB + "/"
			}
		}
	}

	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func getTorrentsRedeTorrent(ctx context.Context, i *Indexer, link, referer string) ([]schema.IndexedTorrent, error) {
	var indexedTorrents []schema.IndexedTorrent
	doc, err := getDocument(ctx, i, link, referer)
	if err != nil {
		return nil, err
	}

	// Title: h1 ou title tag
	title := strings.TrimSpace(doc.Find("h1").First().Text())
	if title == "" {
		title = strings.TrimSpace(doc.Find("title").Text())
		title = regexp.MustCompile(`(?i)\s*[-|].*$`).ReplaceAllString(title, "")
		title = strings.TrimSpace(title)
	}
	if title == "" {
		return nil, fmt.Errorf("could not extract title from %s", link)
	}

	// Year: de .info-pill time ou do texto
	year := ""
	doc.Find(".info-pill time[itemprop='datePublished']").Each(func(_ int, s *goquery.Selection) {
		if year == "" {
			year = strings.TrimSpace(s.Text())
		}
	})
	if year == "" {
		yearRe := regexp.MustCompile(`\b(19|20)\d{2}\b`)
		if m := yearRe.FindString(doc.Text()); m != "" {
			year = m
		}
	}

	// Date
	date := getPublishedDateFromMeta(doc)

	// Magnets: .download-row a[href^='magnet:']
	var magnetLinks []string
	doc.Find(".download-row a[href^='magnet:'], a[href^='magnet:']").Each(func(_ int, s *goquery.Selection) {
		if href, exists := s.Attr("href"); exists && href != "" {
			magnetLinks = append(magnetLinks, href)
		}
	})

	// Audio: do título, original_title e body
	var audio []schema.Audio
	bodyText := doc.Text()
	audio = findAudioFromText(title + " " + bodyText)
	
	// Detecta áudio pelo título se vazio
	if len(audio) == 0 {
		titleLower := strings.ToLower(title + " " + bodyText)
		if strings.Contains(titleLower, "dual") {
			audio = append(audio, "Português", "Inglês")
		} else if strings.Contains(titleLower, "dublado") || strings.Contains(titleLower, "brazilian") ||
			strings.Contains(titleLower, "pt-br") || strings.Contains(titleLower, "portugues") {
			audio = append(audio, "Português")
		} else if strings.Contains(titleLower, "legendado") || strings.Contains(titleLower, "eng") ||
			strings.Contains(titleLower, "english") {
			audio = append(audio, "Inglês")
		}
	}

	// Size: do texto
	var size []string
	doc.Find(".download-name, .download-row, p, span").Each(func(_ int, s *goquery.Selection) {
		size = append(size, findSizesFromText(s.Text())...)
	})

	// IMDB
	imdbLink := ""
	doc.Find("a[href*='imdb.com']").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if imdb, err := getIMDBLink(href); err == nil {
			imdbLink = imdb
		}
	})

	// Fallback: busca IMDB por título se vazio
	if imdbLink == "" && title != "" {
		imdbLink = fetchIMDBByTitle(title, year)
	}

	if len(magnetLinks) == 0 {
		return nil, nil
	}

	var chanIndexedTorrent = make(chan schema.IndexedTorrent)

	for it, magnetLink := range magnetLinks {
		it := it
		go func(it int, magnetLink string) {
			magnetLink = strings.TrimSpace(magnetLink)
			magnetLink = strings.ReplaceAll(magnetLink, "&", "&")
			magnetLink = strings.ReplaceAll(magnetLink, "magnet:? ", "magnet:?")

			magnetParsed, err := magnet.ParseMagnetUri(magnetLink)
			if err != nil {
				logging.Error().Err(err).Str("magnet_link", magnetLink).Msg("Failed to parse magnet URI")
				chanIndexedTorrent <- schema.IndexedTorrent{}
				return
			}

			releaseTitle := magnetParsed.DisplayName
			if releaseTitle == "" {
				releaseTitle = title
			}
			infoHash := magnetParsed.InfoHash.String()
			trackers := magnetParsed.Trackers
			magnetAudio := getAudioFromTitle(releaseTitle, audio)

			// Fallback: detecta áudio pelo título se vazio
			if len(magnetAudio) == 0 {
				titleLower := strings.ToLower(releaseTitle + " " + title)
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

			peer, seed, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, infoHash, trackers)
			if err != nil {
				peer, seed = 0, 0
			}

			titleProcessed := processTitle(title, magnetAudio)

			var mySize string
			if it < len(size) {
				mySize = size[it]
			}
			// Fallback: size do magnet xl=
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
