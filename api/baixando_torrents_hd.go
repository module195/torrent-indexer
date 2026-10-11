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
	if page == "" {
		page = "1"
	}

	var targetURL string
	if q != "" {
		targetURL = fmt.Sprintf("%s%s%s", metadata.URL, metadata.SearchURL, url.QueryEscape(q))
		if page != "1" {
			targetURL += "&paged=" + page
		}
	} else {
		targetURL = metadata.URL
		if page != "1" {
			targetURL = fmt.Sprintf("%s%s", metadata.URL, fmt.Sprintf(metadata.PagePattern, page))
		}
	}

	resp, err := i.requester.GetDocument(ctx, targetURL)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
		return
	}
	defer resp.Close()

	doc, err := goquery.NewDocumentFromReader(resp)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
		return
	}

	// Coleta apenas links da seção de resultados da busca
	var links []string
	seen := make(map[string]bool)

	doc.Find(".lyra-lista .lyra-post a[href], .lyra-post-link[href]").Each(func(_ int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" || seen[href] {
			return
		}
		// Ignora links que claramente não são posts de conteúdo
		if strings.Contains(href, "/page/") || strings.Contains(href, "/category/") || strings.Contains(href, "/tag/") {
			return
		}
		seen[href] = true
		links = append(links, href)
	})

	if len(links) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Response{Results: []schema.IndexedTorrent{}, Count: 0})
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

	// Título principal
	title := strings.TrimSpace(doc.Find("h1.lyra-h1, h1, .entry-title").First().Text())
	if title == "" {
		title = strings.TrimSpace(doc.Find("title").Text())
	}

	year := ""
	if m := regexp.MustCompile(`(\d{4})`).FindStringSubmatch(title); len(m) > 0 {
		year = m[1]
	}

	// IMDb
	imdbLink := ""
	doc.Find("a[href*='imdb.com']").Each(func(_ int, s *goquery.Selection) {
		if href, _ := s.Attr("href"); strings.Contains(href, "title/tt") {
			imdbLink = href
		}
	})

	// Áudio e tamanho a partir da ficha
	var audio []schema.Audio
	var size string
	doc.Find(".lyra-ficha li, .lyra-rel-idioma, .info-item, .meta-item").Each(func(_ int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())
		audio = append(audio, findAudioFromText(text)...)
		if m := regexp.MustCompile(`(\d+[,.]\d+\s*(GB|MB|GiB|MiB))`).FindStringSubmatch(text); len(m) > 0 && size == "" {
			size = m[0]
		}
	})

	// Coleta links de download (classe específica do site)
	var downloadLinks []string
	doc.Find("a.lyra-baixar[href], a[href*='/gate/go/']").Each(func(_ int, s *goquery.Selection) {
		if href, exists := s.Attr("href"); exists && href != "" {
			downloadLinks = append(downloadLinks, href)
		}
	})

	// Tenta magnets diretos (caso existam)
	var magnetLinks []string
	doc.Find("a[href^='magnet:']").Each(func(_ int, s *goquery.Selection) {
		if href, _ := s.Attr("href"); href != "" {
			magnetLinks = append(magnetLinks, href)
		}
	})

	// Se não encontrou magnet, tenta seguir o gate (limitado)
	if len(magnetLinks) == 0 && len(downloadLinks) > 0 {
		// Nota: o gate do site exige interação / redirecionamentos múltiplos.
		// Aqui apenas registramos o link de gate como “magnet” provisório
		// ou tentamos uma única vez. Em produção pode ser necessário
		// um tratamento mais robusto (FlareSolverr + múltiplos redirects).
		for _, dl := range downloadLinks {
			if strings.Contains(dl, "/gate/go/") {
				// Opcional: tentar resolver o gate aqui
				// Por enquanto deixamos o link original para não travar
				magnetLinks = append(magnetLinks, dl)
			}
		}
	}

	if len(magnetLinks) == 0 {
		return nil, nil
	}

	var indexedTorrents []schema.IndexedTorrent
	for _, magnetLink := range magnetLinks {
		// Só processa se for realmente um magnet
		if !strings.HasPrefix(magnetLink, "magnet:") {
			continue
		}

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

		ixt := schema.IndexedTorrent{
			Title:         magnetParsed.DisplayName,
			OriginalTitle: title,
			Details:       link,
			Year:          year,
			IMDB:          imdbLink,
			Audio:         getAudioFromTitle(magnetParsed.DisplayName, audio),
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
