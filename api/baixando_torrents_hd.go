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
	"github.com/felipemarinho97/torrent-indexer/magnet"
	"github.com/felipemarinho97/torrent-indexer/schema"
	goscrape "github.com/felipemarinho97/torrent-indexer/scrape"
	"github.com/felipemarinho97/torrent-indexer/utils"
)

var baixandoTorrentsHD = IndexerMeta{
	Label:       "baixando_torrents_hd",
	URL:         utils.GetIndexerURLFromEnv("INDEXER_BAIXANDO_TORRENTS_HD_URL", "https://baixandotorrentshd.org/"),
	SearchURL:   "",
	PagePattern: "page/%s",
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
	if page == "" {
		page = "1"
	}

	var doc *goquery.Document
	var err error
	var targetURL string

	if q != "" {
		targetURL = fmt.Sprintf("%s?s=%s", metadata.URL, url.QueryEscape(q))
		if page != "1" {
			targetURL += "&paged=" + page
		}
		resp, err := i.requester.GetDocument(ctx, targetURL)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
			return
		}
		defer resp.Close()
		doc, err = goquery.NewDocumentFromReader(resp)
	} else {
		targetURL = metadata.URL
		if page != "1" {
			targetURL = fmt.Sprintf("%s%s", targetURL, fmt.Sprintf(metadata.PagePattern, page))
		}
		resp, err := i.requester.GetDocument(ctx, targetURL)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
			return
		}
		defer resp.Close()
		doc, err = goquery.NewDocumentFromReader(resp)
	}

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
		return
	}

	var links []string
	doc.Find("article, .post, .grid-item, .movie-item").Each(func(_ int, s *goquery.Selection) {
		a, exists := s.Find("a").First().Attr("href")
		if exists {
			links = append(links, a)
		}
	})

	if len(links) == 0 {
		return
	}

	indexedTorrents := utils.ParallelFlatMap(links, func(link string) ([]schema.IndexedTorrent, error) {
		return getTorrentsBaixandoTorrentsHD(ctx, i, link)
	})

	postProcessed := indexedTorrents
	for _, processor := range i.postProcessors {
		postProcessed = processor(i, r, postProcessed)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Response{
		Results:      postProcessed,
		Count:        len(postProcessed),
		IndexedCount: len(indexedTorrents),
	})
}

func getTorrentsBaixandoTorrentsHD(ctx context.Context, i *Indexer, link string) ([]schema.IndexedTorrent, error) {
	doc, err := getDocument(ctx, i, link, "")
	if err != nil {
		return nil, err
	}

	title := strings.TrimSpace(doc.Find("h1, .entry-title").First().Text())
	year := ""
	yearMatch := regexp.MustCompile(`(\d{4})`).FindStringSubmatch(title)
	if len(yearMatch) > 0 {
		year = yearMatch[1]
	}

	imdbLink := ""
	doc.Find("a[href*='imdb.com']").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if strings.Contains(href, "title/tt") {
			imdbLink = href
		}
	})

	var audio []schema.Audio
	var size string
	doc.Find(".info-item, .meta-item, tr td").Each(func(_ int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())
		if strings.Contains(text, "Idioma") || strings.Contains(text, "Audio") {
			audio = append(audio, findAudioFromText(text)...)
		}
		sizeMatch := regexp.MustCompile(`(\d+[,.]\d+\s*(GB|MB))`).FindStringSubmatch(text)
		if len(sizeMatch) > 0 {
			size = sizeMatch[0]
		}
	})

	downloadLink := ""
	doc.Find("a[href*='/download/'], a[href*='download/']").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if !strings.Contains(href, "baixar") && downloadLink == "" {
			downloadLink = href
		}
	})

	var magnetLinks []string
	if downloadLink != "" {
		downloadDoc, err := getDocument(ctx, i, downloadLink, link)
		if err == nil {
			downloadDoc.Find("a[href^='magnet:']").Each(func(_ int, s *goquery.Selection) {
				magnetLink, _ := s.Attr("href")
				magnetLinks = append(magnetLinks, magnetLink)
			})
		}
	}

	if len(magnetLinks) == 0 {
		doc.Find("a[href^='magnet:']").Each(func(_ int, s *goquery.Selection) {
			magnetLink, _ := s.Attr("href")
			magnetLinks = append(magnetLinks, magnetLink)
		})
	}

	if len(magnetLinks) == 0 {
		return nil, nil
	}

	var indexedTorrents []schema.IndexedTorrent
	for _, magnetLink := range magnetLinks {
		magnetParsed, err := magnet.ParseMagnetUri(magnetLink)
		if err != nil {
			continue
		}

		infoHash := magnetParsed.InfoHash.String()
		trackers := magnetParsed.Trackers

		peer, seed, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, infoHash, trackers)
		if err != nil {
			peer, seed = 0, 0
		}

		magnetAudio := getAudioFromTitle(magnetParsed.DisplayName, audio)

		ixt := schema.IndexedTorrent{
			Title:         magnetParsed.DisplayName,
			OriginalTitle: title,
			Details:       link,
			Year:          year,
			IMDB:          imdbLink,
			Audio:         magnetAudio,
			MagnetLink:    magnetLink,
			InfoHash:      infoHash,
			Trackers:      trackers,
			SeedCount:     seed,
			LeechCount:    peer,
			Size:          size,
		}
		indexedTorrents = append(indexedTorrents, ixt)
	}

	return indexedTorrents, nil
}
