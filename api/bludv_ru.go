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
		i.metrics.IndexerDuration.WithLabelValues(bludvRu.Label).Observe(time.Since(start).Seconds())
		i.metrics.IndexerRequests.WithLabelValues(bludvRu.Label).Inc()
	}()

	ctx := r.Context()
	q := r.URL.Query().Get("q")
	page := r.URL.Query().Get("page")
	if page == "" {
		page = "1"
	}

	// The site finds a post by its IMDb ID, since every post links its IMDb
	// page and the WordPress search reads the post body.
	imdbID := ""
	if id := strings.ToLower(strings.TrimSpace(q)); reIMDbID.MatchString(id) {
		imdbID, q = id, id
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

	// Search results and the home change all the time: keep them out of the
	// 24 h document cache, or new posts take a day to show up.
	doc, err := getListingDocument(ctx, i, targetURL, bludvRu.URL)
	if err != nil {
		logging.Error().Err(err).Str("url", targetURL).Msg("Failed to get document")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		i.metrics.IndexerErrors.WithLabelValues(bludvRu.Label).Inc()
		return
	}

	links := extractBludvRuLinks(doc)

	// Fallback inteligente
	if len(links) == 0 && q != "" && imdbID == "" && page == "1" {
		for _, altQ := range buildSmartFallbackQueries(q) {
			altURL := bludvRu.URL + bludvRu.SearchURL + url.QueryEscape(altQ)
			logging.Info().
				Str("original_q", q).
				Str("fallback_q", altQ).
				Str("url", altURL).
				Msg("Trying fallback search")

			altDoc, err := getListingDocument(ctx, i, altURL, bludvRu.URL)
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
	if imdbID != "" {
		for k := range indexedTorrents {
			if indexedTorrents[k].IMDB == "" {
				indexedTorrents[k].IMDB = "https://www.imdb.com/title/" + imdbID + "/"
			}
		}
	}

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

	page, releases := parseBludvRuPage(doc)
	if len(releases) == 0 {
		return nil, nil
	}

	var indexedTorrents []schema.IndexedTorrent
	var chanIndexedTorrent = make(chan schema.IndexedTorrent)

	for _, rel := range releases {
		go func(rel bludvRuRelease) {
			peer, seed, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, rel.InfoHash, rel.Trackers)
			if err != nil {
				peer, seed = 0, 0
			}

			chanIndexedTorrent <- schema.IndexedTorrent{
				Title:         rel.Title,
				OriginalTitle: page.Title,
				Details:       link,
				Year:          page.Year,
				IMDB:          page.IMDB,
				Audio:         rel.Audio,
				MagnetLink:    rel.MagnetLink,
				Date:          page.Date,
				InfoHash:      rel.InfoHash,
				Trackers:      rel.Trackers,
				LeechCount:    peer,
				SeedCount:     seed,
				Size:          rel.Size,
			}
		}(rel)
	}

	for range releases {
		indexedTorrents = append(indexedTorrents, <-chanIndexedTorrent)
	}

	return indexedTorrents, nil
}

// bludvRuPage is what a post says about the whole title.
type bludvRuPage struct {
	Title, Year, IMDB string
	Date              time.Time
}

// bludvRuRelease is one magnet of a post, with what its label says about it.
type bludvRuRelease struct {
	Title, MagnetLink, InfoHash, Size string
	Trackers                          []string
	Audio                             []schema.Audio
}

// parseBludvRuPage reads a post. Each magnet sits in a paragraph whose bold
// text labels it ("WEB-DL 1080p x265 (1.58 GB)", "Episódio 03:"), under a
// section header paragraph ("VERSÃO MKV LEGENDADO"). The post's own
// "Idioma:"/"Tamanho:" fields cover every release at once, so the label and
// section win over them.
func parseBludvRuPage(doc *goquery.Document) (bludvRuPage, []bludvRuRelease) {
	// The header block may hold the date, year and tags below the title;
	// only its first line is the title.
	title := strings.TrimSpace(strings.SplitN(strings.TrimSpace(doc.Find("h1, .title, .entry-title").First().Text()), "\n", 2)[0])
	if title == "" {
		title = strings.TrimSpace(doc.Find("title").Text())
	}
	page := bludvRuPage{Title: title, Year: reYearWord.FindString(title), Date: getPublishedDateFromMeta(doc)}

	doc.Find("a[href*='imdb.com']").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if imdb, err := getIMDBLink(href); err == nil {
			page.IMDB = imdb
		}
	})

	var audio []schema.Audio
	var sizes []string
	doc.Find(".conteudo p, .entry-content p, article p, .post p").Each(func(_ int, s *goquery.Selection) {
		if s.Find("a[href^='magnet:']").Length() > 0 {
			return // a release label, not the post's info block
		}
		text := strings.TrimSpace(s.Text())
		audio = append(audio, findAudioFromText(text)...)
		sizes = append(sizes, findSizesFromText(text)...)
		if y := findYearFromText(text, title); y != "" && page.Year == "" {
			page.Year = y
		}
	})
	sizes = utils.StableUniq(sizes)

	seen := map[string]bool{}
	var anchors []*goquery.Selection
	doc.Find("a[href^='magnet:']").Each(func(_ int, s *goquery.Selection) {
		href := strings.TrimSpace(s.AttrOr("href", ""))
		if href == "" || seen[href] {
			return
		}
		seen[href] = true
		anchors = append(anchors, s)
	})

	var releases []bludvRuRelease
	for _, a := range anchors {
		magnetLink := strings.NewReplacer("\n", "", "\r", "").Replace(strings.TrimSpace(a.AttrOr("href", "")))
		magnetParsed, err := magnet.ParseMagnetUri(magnetLink)
		if err != nil {
			logging.Error().Err(err).Str("magnet_link", magnetLink).Msg("Failed to parse magnet URI")
			continue
		}
		label, section := bludvRuMagnetLabel(a)
		hint := bludvRuLabelAudio(section + " " + label)

		releaseTitle := magnetParsed.DisplayName
		audioTitle := releaseTitle
		if hint != "" && !hasAudioMarker(releaseTitle) {
			audioTitle += " " + hint
		}

		size := releaseSize(magnetLink, releaseTitle)
		if size == "" {
			size = sizeFromButtonLabel(label)
		}
		if size == "" && len(anchors) == 1 && len(sizes) == 1 {
			size = sizes[0]
		}

		releases = append(releases, bludvRuRelease{
			Title:      withLabelTags(releaseTitle, label),
			MagnetLink: magnetLink,
			InfoHash:   magnetParsed.InfoHash.String(),
			Trackers:   magnetParsed.Trackers,
			Audio:      getAudioFromTitle(audioTitle, audio),
			Size:       size,
		})
	}
	return page, releases
}

// bludvRuMagnetLabel returns the text that labels a magnet link in its own
// paragraph (what comes before it, after any earlier magnet) and the nearest
// paragraph above without magnets in the same block (blocks end at <hr>).
func bludvRuMagnetLabel(a *goquery.Selection) (label, section string) {
	p := a.Closest("p")
	if p.Length() == 0 {
		return strings.TrimSpace(a.Text()), ""
	}

	var buf strings.Builder
	p.Contents().EachWithBreak(func(_ int, c *goquery.Selection) bool {
		if c.Is("a[href^='magnet:']") {
			if c.Nodes[0] == a.Nodes[0] {
				label = buf.String()
				return false
			}
			buf.Reset()
			return true
		}
		buf.WriteString(c.Text())
		buf.WriteString(" ")
		return true
	})
	if strings.TrimSpace(label) == "" {
		// the magnet is nested deeper (inside <strong>, <span>): fall back to the bold text
		label = p.Find("strong, b").First().Text()
	}
	// the link text tells the resolution in series posts ("Episódio 03: <a>1080p</a>")
	label = strings.Join(strings.Fields(label+" "+a.Text()), " ")

	for prev := p.Prev(); prev.Length() > 0 && !prev.Is("hr"); prev = prev.Prev() {
		if prev.Is("p") && prev.Find("a[href^='magnet:']").Length() == 0 {
			if t := strings.Join(strings.Fields(prev.Text()), " "); t != "" {
				section = t
				break
			}
		}
	}
	return label, section
}

// bludvRuLabelAudio turns what a label or section says about the audio into a
// release-name marker getAudioFromTitle understands, or "".
func bludvRuLabelAudio(text string) string {
	l := " " + reNonAlphanumeric.ReplaceAllString(normalizeForMatch(text), " ") + " "
	switch {
	case strings.Contains(l, " nacional "):
		return "NACIONAL"
	case strings.Contains(l, " dual "):
		return "DUAL"
	case strings.Contains(l, " dublado "):
		return "DUBLADO"
	case strings.Contains(l, " legendado "):
		return "LEGENDADO"
	}
	return ""
}

// hasAudioMarker reports whether a release name already says its audio, using
// the same words as getAudioFromTitle.
func hasAudioMarker(releaseTitle string) bool {
	words := " " + strings.TrimSpace(reNonAlphanumeric.ReplaceAllString(normalizeForMatch(stripAudioSuffix(releaseTitle)), " ")) + " "
	for _, m := range append(append([]string{}, ptAudioMarkers...), subtitleMarkers...) {
		if strings.Contains(words, " "+m+" ") {
			return true
		}
	}
	for w := range otherAudioWords {
		if strings.Contains(words, " "+w+" ") {
			return true
		}
	}
	return false
}

// reLabelTags matches the quality tags a label may tell that a release name
// lacks: two YTS releases share "Mortal Kombat II (2026) [1080p] [YTS.BZ]"
// and only the label says one of them is x265.
var reLabelTags = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|480p|4k|x265|x264|hevc|hdr10\+?|hdr|dv|web-?dl|web-?rip|bluray|brrip)(?:\b|$)`)

// withLabelTags appends to releaseTitle the label's quality tags it lacks.
func withLabelTags(releaseTitle, label string) string {
	have := strings.ToLower(reNonAlphanumeric.ReplaceAllString(normalizeForMatch(releaseTitle), ""))
	var extra []string
	for _, tag := range reLabelTags.FindAllString(label, -1) {
		key := strings.ToLower(reNonAlphanumeric.ReplaceAllString(normalizeForMatch(tag), ""))
		switch {
		case strings.Contains(have, key):
			continue
		case key == "4k" && strings.Contains(have, "2160p"), key == "2160p" && strings.Contains(have, "4k"):
			continue
		case key == "hdr" && strings.Contains(have, "hdr10"):
			continue
		}
		have += key
		extra = append(extra, tag)
	}
	if len(extra) == 0 {
		return releaseTitle
	}
	return releaseTitle + " " + strings.Join(extra, " ")
}
