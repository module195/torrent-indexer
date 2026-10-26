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

// The site moves to a new "-vNN" domain from time to time and redirects the
// old one (v26 → v27 with a 301), so an outdated default keeps working.
var starck_filmes = IndexerMeta{
	Label:       "starck_filmes",
	URL:         utils.GetIndexerURLFromEnv("INDEXER_STARCK_FILMES_URL", "https://starckfilmes-v27.com/"),
	SearchURL:   "?s=",
	PagePattern: "page/%s/",
}

func (i *Indexer) HandlerStarckFilmesIndexer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	metadata := starck_filmes

	defer func() {
		i.metrics.IndexerDuration.WithLabelValues(metadata.Label).Observe(time.Since(start).Seconds())
		i.metrics.IndexerRequests.WithLabelValues(metadata.Label).Inc()
	}()

	ctx := r.Context()
	// supported query params: q, page, filter_results
	q := r.URL.Query().Get("q")
	page := r.URL.Query().Get("page")
	if page == "" {
		page = "1"
	}

	baseURL := strings.TrimRight(metadata.URL, "/") + "/"
	var targetURL string
	if page != "1" {
		targetURL = baseURL + fmt.Sprintf(metadata.PagePattern, page)
	} else {
		targetURL = baseURL
	}
	if q != "" {
		targetURL += metadata.SearchURL + url.QueryEscape(q)
	}

	logging.InfoWithRequest(r).Str("target_url", targetURL).Msg("Processing indexer request")

	// Search results and the home change all the time: keep them out of the
	// 24 h document cache, or new posts take a day to show up.
	doc, err := getListingDocument(ctx, i, targetURL, baseURL)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		err = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		if err != nil {
			logging.ErrorWithRequest(r).Err(err).Msg("Failed to encode error response")
		}
		i.metrics.IndexerErrors.WithLabelValues(metadata.Label).Inc()
		return
	}

	links := extractStarckFilmesLinks(doc)
	if len(links) == 0 {
		logging.DebugWithRequest(r).Str("url", targetURL).Msg("No links found")
	}

	// extract each torrent link
	indexedTorrents := utils.ParallelFlatMap(links, func(link string) ([]schema.IndexedTorrent, error) {
		return getTorrentStarckFilmes(ctx, i, link, baseURL)
	})

	// Apply post-processors
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

// extractStarckFilmesLinks returns the title pages listed on a home or search
// page: the cards of the catalog, without the carousel on top (.slide-item)
// and the "Você pode gostar" suggestions.
func extractStarckFilmesLinks(doc *goquery.Document) []string {
	var links []string
	seen := make(map[string]bool)
	doc.Find(".item").Each(func(_ int, s *goquery.Selection) {
		if s.Closest(".slide-item, .post-suggestion").Length() > 0 {
			return
		}
		link, exists := s.Find("div.sub-item > a").Attr("href")
		if !exists || link == "" || !strings.Contains(link, "/catalog/") {
			return
		}
		if seen[link] {
			return
		}
		seen[link] = true
		links = append(links, link)
	})
	return links
}

func getTorrentStarckFilmes(ctx context.Context, i *Indexer, link, referer string) ([]schema.IndexedTorrent, error) {
	doc, err := getDocument(ctx, i, link, referer)
	if err != nil {
		return nil, err
	}

	page, releases := parseStarckFilmesPage(doc)
	if len(releases) == 0 {
		return nil, nil
	}
	page.Date = starckFilmesDateFromLink(link)

	// The pages carry no IMDb link: look it up by the original title.
	imdbLink := ""
	searchTitle := page.OriginalTitle
	if searchTitle == "" {
		searchTitle = page.Title
	}
	if searchTitle != "" {
		imdbLink = fetchIMDBByTitle(searchTitle, page.Year)
	}

	var indexedTorrents []schema.IndexedTorrent
	var chanIndexedTorrent = make(chan schema.IndexedTorrent)

	for _, rel := range releases {
		go func(rel starckFilmesRelease) {
			peer, seed, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, rel.InfoHash, rel.Trackers)
			if err != nil {
				logging.Error().Err(err).Str("info_hash", rel.InfoHash).Msg("Failed to get leechers and seeders")
			}
			if rel.Size == "" {
				go func() {
					_, _ = i.magnetMetadataAPI.FetchMetadata(ctx, rel.MagnetLink)
				}()
			}

			chanIndexedTorrent <- schema.IndexedTorrent{
				Title:         rel.Title,
				OriginalTitle: processTitle(page.Title, rel.Audio),
				Details:       link,
				Year:          page.Year,
				IMDB:          imdbLink,
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

// starckFilmesPage is what a title page says about the whole title.
type starckFilmesPage struct {
	Title, OriginalTitle, Year string
	Date                       time.Time
}

// starckFilmesRelease is one download button of a title page.
type starckFilmesRelease struct {
	Title, MagnetLink, InfoHash, Size string
	Trackers                          []string
	Audio                             []schema.Audio
}

// parseStarckFilmesPage reads a title page. The info block is a list of
// "<span>Label:</span><span>value</span>" pairs; each version is a button in
// .post-buttons with its own label ("Dual Áudio MKV Download 1080p (3.52 GB)").
// The button links to an ad redirector whose xid parameter is the magnet link,
// obfuscated (older pages used a plain magnet: href or a data-u attribute).
func parseStarckFilmesPage(doc *goquery.Document) (starckFilmesPage, []starckFilmesRelease) {
	capa := doc.Find(".post .capa")
	page := starckFilmesPage{Title: strings.TrimSpace(capa.Find(".post-description > h2").First().Text())}

	var audio []schema.Audio
	var sizes []string
	capa.Find(".post-description p").Each(func(_ int, s *goquery.Selection) {
		spans := s.Find("span")
		if spans.Length() < 2 {
			return
		}
		label := strings.TrimSuffix(strings.TrimSpace(spans.First().Text()), ":")
		value := strings.TrimSpace(spans.Eq(1).Text())
		text := label + ": " + value
		switch label {
		case "Nome Original":
			page.OriginalTitle = value
		case "Lançamento":
			// collections show a range ("1997-2009"): keep the first year
			page.Year = reYearWord.FindString(value)
		}
		audio = append(audio, findAudioFromText(text)...)
		sizes = append(sizes, findSizesFromText(text)...)
	})
	if page.Year == "" {
		page.Year = reYearWord.FindString(page.Title)
	}
	sizes = utils.StableUniq(sizes)

	type button struct{ magnetLink, label string }
	var buttons []button
	seen := map[string]bool{}
	doc.Find(".post-buttons a").Each(func(_ int, s *goquery.Selection) {
		magnetLink := starckFilmesButtonMagnet(s)
		if magnetLink == "" || seen[magnetLink] {
			return
		}
		seen[magnetLink] = true
		label := strings.Join(strings.Fields(s.Closest(".btn-down").Find(".text").Text()), " ")
		buttons = append(buttons, button{magnetLink, label})
	})

	var releases []starckFilmesRelease
	for k, b := range buttons {
		// some trackers come with "announce" machine-translated to "anunciar"
		magnetLink := reTranslatedAnnounce.ReplaceAllString(b.magnetLink, "${1}announce$2")
		magnetParsed, err := magnet.ParseMagnetUri(magnetLink)
		if err != nil {
			logging.Error().Err(err).Str("magnet_link", magnetLink).Msg("Failed to parse magnet URI")
			continue
		}

		releaseTitle := strings.TrimSpace(magnetParsed.DisplayName)
		audioTitle := releaseTitle
		if hint := bludvRuLabelAudio(b.label); hint != "" && !hasAudioMarker(releaseTitle) {
			audioTitle += " " + hint
		}

		size := releaseSize(magnetLink, releaseTitle)
		if size == "" {
			size = sizeFromButtonLabel(b.label)
		}
		if size == "" && len(sizes) == len(buttons) {
			size = sizes[k]
		}

		releases = append(releases, starckFilmesRelease{
			Title:      withLabelTags(releaseTitle, b.label),
			MagnetLink: magnetLink,
			InfoHash:   magnetParsed.InfoHash.String(),
			Trackers:   magnetParsed.Trackers,
			Audio:      getAudioFromTitle(audioTitle, audio),
			Size:       size,
		})
	}
	return page, releases
}

// starckFilmesButtonMagnet returns the magnet link behind a download button,
// or "".
func starckFilmesButtonMagnet(a *goquery.Selection) string {
	href := strings.TrimSpace(a.AttrOr("href", ""))
	if strings.HasPrefix(href, "magnet:") {
		return strings.NewReplacer("magnet:? ", "magnet:?", "magnet:?%20", "magnet:?").Replace(href)
	}
	if xid := reStarckXID.FindStringSubmatch(href); xid != nil {
		raw, err := url.QueryUnescape(xid[1])
		if err != nil {
			raw = xid[1]
		}
		magnetLink, err := utils.DecodeStarckXID(raw)
		if err != nil {
			logging.Warn().Err(err).Str("href", href).Msg("Failed to decode xid, skipping")
			return ""
		}
		return magnetLink
	}
	if dataU, ok := a.Attr("data-u"); ok {
		magnetLink, err := utils.DecodeStarckDataU(dataU)
		if err != nil {
			logging.Warn().Err(err).Str("data_u", dataU).Msg("Failed to decode data-u attribute, skipping")
			return ""
		}
		return magnetLink
	}
	return ""
}

// reStarckXID reads the xid parameter as is: url.Values would turn a "+" of
// the base64 into a space.
var reStarckXID = regexp.MustCompile(`[?&]xid=([^&#]+)`)

// reStarckSlugDate matches the post date at the end of a title URL
// (".../homem-aranha-um-novo-dia-2026-05-10-2026/").
var reStarckSlugDate = regexp.MustCompile(`(\d{2}-\d{2}-\d{4})/?$`)

// starckFilmesDateFromLink reads the post date (dd-mm-yyyy) at the end of the
// slug. The generic parser tries yyyy-mm-dd first and would read the year of
// the title plus the day and month ("2026-05-10") whenever the day is 12 or
// less.
func starckFilmesDateFromLink(link string) time.Time {
	if m := reStarckSlugDate.FindStringSubmatch(strings.TrimSpace(link)); m != nil {
		if date, err := time.Parse("02-01-2006", m[1]); err == nil {
			return date.UTC()
		}
	}
	return getPublishedDateFromRawString(link)
}
