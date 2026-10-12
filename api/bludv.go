package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
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

var bludv = IndexerMeta{
	Label:       "bludv",
	URL:         utils.GetIndexerURLFromEnv("INDEXER_BLUDV_URL", "https://bludvfilmes1.xyz/"),
	SearchURL:   "?s=",
	PagePattern: "page/%s",
}

func (i *Indexer) HandlerBluDVIndexer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	metadata := bludv

	defer func() {
		i.metrics.IndexerDuration.WithLabelValues(metadata.Label).Observe(time.Since(start).Seconds())
		i.metrics.IndexerRequests.WithLabelValues(metadata.Label).Inc()
	}()

	// Desativa o indexer se a variável estiver definida
	if os.Getenv("INDEXER_BLUDV_DISABLED") == "true" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Response{
			Results: []schema.IndexedTorrent{},
			Count:   0,
		})
		return
	}

	ctx := r.Context()
	q := r.URL.Query().Get("q")
	page := r.URL.Query().Get("page")

	q = url.QueryEscape(q)
	targetURL := metadata.URL

	if page != "" && page != "1" {
		targetURL = fmt.Sprintf("%s%s", metadata.URL, fmt.Sprintf(metadata.PagePattern, page))
	} else if q != "" {
		targetURL = fmt.Sprintf("%s%s%s", metadata.URL, metadata.SearchURL, q)
	}

	logging.InfoWithRequest(r).Str("target_url", targetURL).Msg("Processing indexer request")

	resp, err := i.requester.GetDocument(ctx, targetURL)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
		return
	}
	defer resp.Close()

	doc, err := goquery.NewDocumentFromReader(resp)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
		return
	}

	var links []string
	doc.Find(".post").Each(func(_ int, s *goquery.Selection) {
		if link, exists := s.Find("div.title > a").Attr("href"); exists && link != "" {
			links = append(links, link)
		}
	})

	if len(links) == 0 {
		_ = i.requester.ExpireDocument(ctx, targetURL)
	}

	indexedTorrents := utils.ParallelFlatMap(links, func(link string) ([]schema.IndexedTorrent, error) {
		return getTorrentsBluDV(ctx, i, link, targetURL)
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

func getTorrentsBluDV(ctx context.Context, i *Indexer, link, referer string) ([]schema.IndexedTorrent, error) {
	doc, err := getDocument(ctx, i, link, referer)
	if err != nil {
		return nil, err
	}

	article := doc.Find(".post")
	title := strings.Replace(article.Find(".title > h1").Text(), " - Download", "", -1)
	textContent := article.Find("div.content")
	date := getPublishedDate(doc)

	var magnetLinks []string

	// 1. Magnets diretos
	textContent.Find("a[href^='magnet']").Each(func(_ int, s *goquery.Selection) {
		if href, exists := s.Attr("href"); exists {
			magnetLinks = append(magnetLinks, href)
		}
	})

	// 2. Adware antigo (DecodeAdLink)
	adwareDomains := []string{
		"https://www.seuvideo.xyz",
		"https://www.systemads.org",
		"https://superadsgo.xyz",
		"https://promobr.online",
		"https://valedesconto.shop",
	}

	for _, domain := range adwareDomains {
		textContent.Find(fmt.Sprintf("a[href^='%s']", domain)).Each(func(_ int, s *goquery.Selection) {
			href, _ := s.Attr("href")
			parsed, err := url.Parse(href)
			if err != nil {
				return
			}
			id := parsed.Query().Get("id")
			if id == "" {
				return
			}
			decoded, err := utils.DecodeAdLink(id)
			if err != nil {
				return
			}
			if strings.HasPrefix(decoded, "magnet:") {
				magnetLinks = append(magnetLinks, decoded)
			}
		})
	}

	// 3. Novo gate /gate/go/
	textContent.Find("a[href*='/gate/go/']").Each(func(_ int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists {
			return
		}
		magnet, err := resolveBluDVGate(ctx, i, href, link)
		if err != nil {
			logging.Debug().Err(err).Str("href", href).Msg("Failed to resolve BluDV gate")
			return
		}
		if magnet != "" {
			magnetLinks = append(magnetLinks, magnet)
		}
	})

	magnetLinks = utils.StableUniq(magnetLinks)

	if len(magnetLinks) == 0 {
		return nil, nil
	}

	var audio []schema.Audio
	var year string
	var sizes []string

	article.Find("div.content p").Each(func(_ int, s *goquery.Selection) {
		text := s.Text()
		audio = append(audio, findAudioFromText(text)...)
		if y := findYearFromText(text, title); y != "" {
			year = y
		}
		sizes = append(sizes, findSizesFromText(text)...)
	})
	sizes = utils.StableUniq(sizes)

	imdbLink := ""
	article.Find("div.content a").Each(func(_ int, s *goquery.Selection) {
		if href, _ := s.Attr("href"); href != "" {
			if imdb, err := getIMDBLink(href); err == nil {
				imdbLink = imdb
			}
		}
	})

	var indexedTorrents []schema.IndexedTorrent
	chanIndexedTorrent := make(chan schema.IndexedTorrent)

	for it, magnetLink := range magnetLinks {
		it := it
		go func(it int, magnetLink string) {
    // Limpa espaço comum em magnets malformados
    magnetLink = strings.TrimSpace(magnetLink)
    magnetLink = strings.ReplaceAll(magnetLink, "magnet:? ", "magnet:?")
    magnetLink = strings.ReplaceAll(magnetLink, "magnet:?%20", "magnet:?")

    magnet, err := magnet.ParseMagnetUri(magnetLink)
    if err != nil {
        logging.Error().Err(err).Str("magnet_link", magnetLink).Msg("Failed to parse magnet URI")
        return // ou continue sem quebrar
    }

			releaseTitle := magnetParsed.DisplayName
			infoHash := magnetParsed.InfoHash.String()
			trackers := magnetParsed.Trackers
			magnetAudio := getAudioFromTitle(releaseTitle, audio)

			peer, seed, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, infoHash, trackers)
			if err != nil {
				peer, seed = 0, 0
			}

			processedTitle := processTitle(title, magnetAudio)

			var mySize string
			if len(sizes) == len(magnetLinks) {
				mySize = sizes[it]
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

	for range magnetLinks {
		it := <-chanIndexedTorrent
		if it.InfoHash != "" {
			indexedTorrents = append(indexedTorrents, it)
		}
	}

	return indexedTorrents, nil
}

// resolveBluDVGate tenta extrair o magnet a partir do link /gate/go/
func resolveBluDVGate(ctx context.Context, i *Indexer, gateURL, referer string) (string, error) {
	doc, err := getDocument(ctx, i, gateURL, referer)
	if err != nil {
		return "", err
	}

	// 1. Magnet direto na página do gate
	var found string
	doc.Find("a[href^='magnet']").Each(func(_ int, s *goquery.Selection) {
		if href, exists := s.Attr("href"); exists && found == "" {
			found = href
		}
	})
	if found != "" {
		return found, nil
	}

	// 2. Links promobr.online → parâmetro u=
	doc.Find("a[href*='promobr.online']").Each(func(_ int, s *goquery.Selection) {
		if found != "" {
			return
		}
		href, _ := s.Attr("href")
		parsed, err := url.Parse(href)
		if err != nil {
			return
		}
		u := parsed.Query().Get("u")
		if u == "" {
			return
		}
		decoded, err := utils.DecodeAdLink(u)
		if err == nil && strings.HasPrefix(decoded, "magnet:") {
			found = decoded
		}
	})
	if found != "" {
		return found, nil
	}

	// 3. Qualquer link com parâmetro id= e DecodeAdLink
	doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		if found != "" {
			return
		}
		href, _ := s.Attr("href")
		parsed, err := url.Parse(href)
		if err != nil {
			return
		}
		id := parsed.Query().Get("id")
		if id == "" {
			return
		}
		decoded, err := utils.DecodeAdLink(id)
		if err == nil && strings.HasPrefix(decoded, "magnet:") {
			found = decoded
		}
	})

	// 4. Regex no HTML bruto
	if found == "" {
		html, _ := doc.Html()
		if m := regexp.MustCompile(`magnet:\?xt=urn:btih:[a-zA-Z0-9]+`).FindString(html); m != "" {
			found = m
		}
	}

	return found, nil
}

func getPublishedDate(document *goquery.Document) time.Time {
	datePublished := strings.TrimSpace(document.Find("meta[property='article:published_time']").AttrOr("content", ""))
	if datePublished != "" {
		date, err := time.Parse(time.RFC3339, datePublished)
		if err == nil {
			return date
		}
	}
	return time.Time{}
}
