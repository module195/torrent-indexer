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
	SearchURL:   "index.php?busca=",
	PagePattern: "%s",
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

	// Novo seletor (site atual usa a.cover-link)
	doc.Find("a.cover-link").Each(func(_ int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" || seen[href] {
			return
		}
		seen[href] = true
		links = append(links, href)
	})

	// Fallback antigo (caso o site ainda tenha alguma página no formato anterior)
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

// buildRedeTorrentFallbackQueries gera buscas alternativas quando a original falha.
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

	article := doc.Find(".conteudo")
	// title pattern: "Something - optional balbla (dddd) some shit" - extract "Something" and "dddd"
	titleRe := regexp.MustCompile(`^(.*?)(?: - (.*?))? \((\d{4})\)`)
	titleP := titleRe.FindStringSubmatch(article.Find("h1").Text())
	if len(titleP) < 3 {
		// Fallback: tenta pegar título de outras formas
		h1 := strings.TrimSpace(article.Find("h1").Text())
		if h1 == "" {
			h1 = strings.TrimSpace(doc.Find("h1").First().Text())
		}
		if h1 == "" {
			return nil, fmt.Errorf("could not extract title from %s", link)
		}
		titleP = []string{"", h1, "", ""}
	}
	title := strings.TrimSpace(titleP[1])
	year := ""
	// Year: tenta regex no título, depois no texto
	yearRe := regexp.MustCompile(`\((\d{4})\)|\b(19|20)\d{2}\b`)
	if m := yearRe.FindStringSubmatch(title); len(m) > 1 {
		year = m[1]
	}
	if year == "" {
		if m := yearRe.FindStringSubmatch(article.Text()); len(m) > 1 {
			year = m[1]
		}
	}

	textContent := article.Find(".apenas_itemprop")
	date := getPublishedDateFromMeta(doc)
	magnets := textContent.Find("a[href^=\"magnet\"]")
	var magnetLinks []string
	magnets.Each(func(i int, s *goquery.Selection) {
		magnetLink, _ := s.Attr("href")
		magnetLinks = append(magnetLinks, magnetLink)
	})

	// Também procura magnets em qualquer lugar da página
	if len(magnetLinks) == 0 {
		doc.Find("a[href^='magnet:']").Each(func(_ int, s *goquery.Selection) {
			if href, exists := s.Attr("href"); exists && href != "" {
				magnetLinks = append(magnetLinks, href)
			}
		})
	}

	var audio []schema.Audio
	var size []string
	article.Find("div#informacoes > p").Each(func(i int, s *goquery.Selection) {
		htmlContent, err := s.Html()
		if err != nil {
			logging.Error().
				Str("url", link).
				Err(err).Msg("Failed to get HTML content")
			return
		}

		htmlContent = strings.ReplaceAll(htmlContent, "\n", "")
		htmlContent = strings.ReplaceAll(htmlContent, "\t", "")

		brRe := regexp.MustCompile(`<br\s*\/?>`)
		htmlContent = brRe.ReplaceAllString(htmlContent, "<br>")
		lines := strings.Split(htmlContent, "<br>")

		var text strings.Builder
		for _, line := range lines {
			re := regexp.MustCompile(`<[^>]*>`)
			line = re.ReplaceAllString(line, "")
			line = strings.TrimSpace(line)
			text.WriteString(line + "\n")
		}

		audio = append(audio, findAudioFromText(text.String())...)
		y := findYearFromText(text.String(), title)
		if y != "" {
			year = y
		}
		size = append(size, findSizesFromText(text.String())...)
	})

	imdbLink := ""
	article.Find("a").Each(func(i int, s *goquery.Selection) {
		link, _ := s.Attr("href")
		_imdbLink, err := getIMDBLink(link)
		if err == nil {
			imdbLink = _imdbLink
		}
	})

	// Fallback de áudio: se não encontrou em #informacoes, tenta no título e body
	if len(audio) == 0 {
		bodyText := article.Text()
		if bodyText == "" {
			bodyText = doc.Text()
		}
		audio = findAudioFromText(title + " " + bodyText)
	}

	if len(magnetLinks) == 0 {
		return nil, nil
	}

	var chanIndexedTorrent = make(chan schema.IndexedTorrent)

	for it, magnetLink := range magnetLinks {
		it := it
		go func(it int, magnetLink string) {
			magnetLink = strings.TrimSpace(magnetLink)
			magnetLink = strings.ReplaceAll(magnetLink, "magnet:? ", "magnet:?")
			magnetLink = strings.ReplaceAll(magnetLink, "magnet:?%20", "magnet:?")
			magnetLink = strings.ReplaceAll(magnetLink, "&", "&")

			magnetParsed, err := magnet.ParseMagnetUri(magnetLink)
			if err != nil {
				logging.Error().Err(err).Str("magnet_link", magnetLink).Msg("Failed to parse magnet URI")
				chanIndexedTorrent <- schema.IndexedTorrent{}
				return
			}

			// Título do magnet sem limpar (mantém pontos como torrent-dos-filmes)
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
				if strings.Contains(titleLower, "brazilian") || strings.Contains(titleLower, "pt-br") ||
					strings.Contains(titleLower, "portugues") || strings.Contains(titleLower, "dual") ||
					strings.Contains(titleLower, "dublado") {
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
			if len(size) == len(magnetLinks) {
				mySize = size[it]
			}
			if mySize == "" {
				go func() {
					_, _ = i.magnetMetadataAPI.FetchMetadata(ctx, magnetLink)
				}()
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

	// Ordena: seeders desc, depois year desc, depois title
	sort.SliceStable(indexedTorrents, func(a, b int) bool {
		if indexedTorrents[a].SeedCount != indexedTorrents[b].SeedCount {
			return indexedTorrents[a].SeedCount > indexedTorrents[b].SeedCount
		}
		ya, _ := strconv.Atoi(indexedTorrents[a].Year)
		yb, _ := strconv.Atoi(indexedTorrents[b].Year)
		if ya != yb {
			return ya > yb
		}
		return indexedTorrents[a].Title < indexedTorrents[b].Title
	})

	return indexedTorrents, nil
}
