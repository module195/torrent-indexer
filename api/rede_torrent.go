package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
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
	SearchURL:   "?s=",
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
		// Filtra links de navegação
		if strings.Contains(href, "/genero/") || strings.Contains(href, "/pagina/") {
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
	// Limpa título
	cleanTitle := regexp.MustCompile(`(?i)\s*(torrent|download|dublado|legendado|dual|audio|mkv|mp4|1080p|720p|4k|bluray|web-dl|webrip|camrip).*$`).ReplaceAllString(title, "")
	cleanTitle = regexp.MustCompile(`\s+`).ReplaceAllString(strings.TrimSpace(cleanTitle), " ")
	if cleanTitle == "" {
		return ""
	}

	// Tenta OMDb primeiro (movies)
	omdbURL := fmt.Sprintf("https://www.omdbapi.com/?t=%s&apikey=trilogy", url.QueryEscape(cleanTitle))
	if year != "" {
		omdbURL += "&y=" + year
	}
	if resp, err := http.Get(omdbURL); err == nil {
		defer resp.Body.Close()
		var result struct {
			IMDBID string `json:"imdbID"`
		}
		if json.NewDecoder(resp.Body).Decode(&result) == nil && result.IMDBID != "" {
			return "https://www.imdb.com/title/" + result.IMDBID + "/"
		}
	}

	// Fallback: TVmaze (series)
	tvmazeURL := fmt.Sprintf("https://api.tvmaze.com/singlesearch/shows?q=%s", url.QueryEscape(cleanTitle))
	if resp, err := http.Get(tvmazeURL); err == nil {
		defer resp.Body.Close()
		var result struct {
			Externals struct {
				IMDB string `json:"imdb"`
			} `json:"externals"`
		}
		if json.NewDecoder(resp.Body).Decode(&result) == nil && result.Externals.IMDB != "" {
			return "https://www.imdb.com/title/" + result.Externals.IMDB + "/"
		}
	}

	return ""
}
func buildRedeTorrentFallbackQueries(original string) []string {
	var out []string
	seen := map[string]bool{}

	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || s == original || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	// 1. Remove dois-pontos e separadores
	cleaned := strings.NewReplacer(":", " ", "|", " ", "–", " ", "—", " ").Replace(original)
	cleaned = regexp.MustCompile(`\s+`).ReplaceAllString(cleaned, " ")
	add(cleaned)

	// 2. Remove padrões de episódio/temporada
	noEp := regexp.MustCompile(`(?i)\s*(S\d{1,2}E\d{1,2}|S\d{1,2}|temporada\s*\d+|episodio\s*\d+|episódio\s*\d+|season\s*\d+|episode\s*\d+)\s*`).ReplaceAllString(cleaned, " ")
	noEp = regexp.MustCompile(`\s+`).ReplaceAllString(strings.TrimSpace(noEp), " ")
	add(noEp)

	// 3. Primeiras palavras
	words := strings.Fields(noEp)
	if len(words) > 3 {
		add(strings.Join(words[:3], " "))
	}
	if len(words) > 2 {
		add(strings.Join(words[:2], " "))
	}

	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

// cleanRedeTorrentTitle extrai um título legível do dn= do magnet.
// Remove prefixos de site (BAIXARAPIDO.COM, etc.), tags técnicas e duplicações.
func cleanRedeTorrentTitle(raw string) string {
	if raw == "" {
		return ""
	}
	s := raw

	// Remove prefixos comuns de sites
	prefixes := []string{
		"BAIXARAPIDO.COM-.", "BAIXARAPIDO.COM-", "BAIXARAPIDO.COM.",
		"REDETORRENT.COM-.", "REDETORRENT.COM-",
		"COMANDOTORRENTS-.", "COMANDOTORRENTS-",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(strings.ToUpper(s), strings.ToUpper(p)) {
			s = s[len(p):]
			break
		}
	}

	// Remove sequências de tags técnicas no início (EPISODIO-05.WEB-DL.1080P.MKV.5.1.-DUBLADO-DUAL-AUDIO-.)
	// Mantém a parte que parece nome (contém letras misturadas, não só tags)
	parts := strings.Split(s, ".")
	var keepFrom int
	for i, p := range parts {
		up := strings.ToUpper(p)
		// Tags conhecidas
		if up == "WEB-DL" || up == "WEBDL" || up == "1080P" || up == "720P" || up == "4K" ||
			up == "MKV" || up == "MP4" || up == "5.1" || up == "DUBLADO" || up == "DUAL-AUDIO" ||
			up == "DUAL" || up == "LEGENDADO" || up == "EPISODIO" || strings.HasPrefix(up, "EPISODIO-") ||
			strings.HasPrefix(up, "S0") || up == "BLURAY" || up == "HDTV" || up == "X264" || up == "X265" ||
			up == "HEVC" || up == "AAC" || up == "DDP" || up == "ATMOS" || up == "-" || up == "" {
			keepFrom = i + 1
			continue
		}
		// Se chegou aqui, provavelmente é o nome
		break
	}
	if keepFrom > 0 && keepFrom < len(parts) {
		s = strings.Join(parts[keepFrom:], ".")
	}

	// Limpa separadores, mas preserva tags conhecidas com hífen
	knownHyphen := []string{"WEB-DL", "WEB-RIP", "BD-RIP", "BD-R", "HD-RIP", "HD-TV", "DDP-5.1", "DDP5.1", "DUAL-AUDIO", "MULTI-AUDIO"}
	placeholders := map[string]string{}
	for i, tag := range knownHyphen {
		ph := fmt.Sprintf("⟦%d⟧", i)
		if strings.Contains(strings.ToUpper(s), tag) {
			s = regexp.MustCompile(`(?i)`+regexp.QuoteMeta(tag)).ReplaceAllString(s, ph)
			placeholders[ph] = tag
		}
	}

	s = strings.ReplaceAll(s, ".", " ")
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, "-", " ")
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)

	// Restaura tags com hífen
	for ph, tag := range placeholders {
		s = strings.ReplaceAll(s, ph, tag)
	}

	// Remove sufixo (brazilian) se existir
	s = regexp.MustCompile(`(?i)\s*\(brazilian\)\s*$`).ReplaceAllString(s, "")

	return strings.TrimSpace(s)
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

	// Audio: do título e do body
	var audio []schema.Audio
	bodyText := doc.Text()
	audio = findAudioFromText(title + " " + bodyText)

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
				titleLower := strings.ToLower(releaseTitle)
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
