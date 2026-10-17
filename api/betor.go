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

var betor = IndexerMeta{
	Label:     "betor",
	URL:       utils.GetIndexerURLFromEnv("INDEXER_BETOR_URL", "https://catalogo.betor.top/"),
	SearchURL: "/search/?q=",
}

func (i *Indexer) HandlerBetorIndexer(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	metadata := betor

	defer func() {
		i.metrics.IndexerDuration.WithLabelValues(metadata.Label).Observe(time.Since(start).Seconds())
		i.metrics.IndexerRequests.WithLabelValues(metadata.Label).Inc()
	}()

	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	page := r.URL.Query().Get("page")

	var targetURL string
	base := strings.TrimRight(metadata.URL, "/")
	if q != "" {
		targetURL = base + "/search/?q=" + url.QueryEscape(q)
	} else if page != "" && page != "1" {
		targetURL = base + "/filmes-e-series/page/" + page + "/"
	} else {
		targetURL = base + "/filmes-e-series/"
	}

	logging.InfoWithRequest(r).
		Str("target_url", targetURL).
		Msg("Processing betor indexer request")

	resp, err := i.requester.GetDocument(ctx, targetURL)
	if err != nil {
		logging.ErrorWithRequest(r).Err(err).Str("url", targetURL).Msg("Failed to fetch betor page")
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

	var items []*goquery.Selection
	// Prefer data-attribute based items
	doc.Find("div.item[data-torrent-magnet-uri], div.item[data-torrent-name]").Each(func(_ int, s *goquery.Selection) {
		items = append(items, s)
	})

	// Fallback for older/alternative markup
	if len(items) == 0 {
		doc.Find(".items-list .item, .results .item, .item").Each(func(_ int, s *goquery.Selection) {
			if _, ok := s.Attr("data-torrent-magnet-uri"); ok {
				items = append(items, s)
			}
		})
	}

	var indexedTorrents []schema.IndexedTorrent
	for _, item := range items {
		ixt, err := parseBetorItem(ctx, i, item)
		if err != nil || ixt.InfoHash == "" {
			continue
		}
		indexedTorrents = append(indexedTorrents, ixt)
	}

	// Apply post-processors
	for _, processor := range i.postProcessors {
		indexedTorrents = processor(i, r, indexedTorrents)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Response{
		Results:      indexedTorrents,
		Count:        len(indexedTorrents),
		IndexedCount: len(indexedTorrents),
	})
}

func parseBetorItem(ctx context.Context, i *Indexer, item *goquery.Selection) (schema.IndexedTorrent, error) {
	var ixt schema.IndexedTorrent

	magnetURI, _ := item.Attr("data-torrent-magnet-uri")
	if magnetURI == "" {
		return ixt, fmt.Errorf("no magnet URI")
	}

	torrentName, _ := item.Attr("data-torrent-name")
	languages, _ := item.Attr("data-torrent-languages")
	sizeStr, _ := item.Attr("data-torrent-size")
	seedsStr, _ := item.Attr("data-torrent-num-seeds")
	peersStr, _ := item.Attr("data-torrent-num-peers")
	imdbID, _ := item.Attr("data-item-imdb-id")
	_, _ = item.Attr("data-item-tmdb-id")     // available if needed later
	_, _ = item.Attr("data-item-type")        // "movie" or "tv"
	providerURL, _ := item.Attr("data-provider-url")
	insertedAt, _ := item.Attr("data-torrent-inserted-at")

	// Parse size (bytes)
	var sizeBytes int64
	if sizeStr != "" {
		sizeBytes, _ = strconv.ParseInt(sizeStr, 10, 64)
	}

	// Parse seeds/peers
	seeds, _ := strconv.Atoi(seedsStr)
	peers, _ := strconv.Atoi(peersStr)

	// Parse magnet
	mag, err := magnet.ParseMagnetUri(magnetURI)
	if err != nil {
		return ixt, fmt.Errorf("parse magnet: %w", err)
	}

	infoHash := mag.InfoHash.String()
	releaseTitle := mag.DisplayName
	if releaseTitle == "" {
		releaseTitle = torrentName
	}

	// Clean known site prefixes from the release title (matches CleanupTitleWebsites behavior)
	releaseTitle = cleanBetorTitle(releaseTitle)
	torrentName = cleanBetorTitle(torrentName)

	// Year extraction: try data attribute first, then findYearFromText pattern, then regex
	year := ""
	if y := findYearFromText(torrentName, torrentName); y != "" {
		year = y
	} else if m := regexp.MustCompile(`\b(19|20)\d{2}\b`).FindString(torrentName); m != "" {
		year = m
	}

	// Languages
	audio := parseBetorLanguages(languages)

	// IMDb
	imdbLink := ""
	if imdbID != "" {
		imdbLink = "https://www.imdb.com/title/" + imdbID + "/"
	}

	// Date
	var date time.Time
	if insertedAt != "" {
		// Try common formats
		for _, layout := range []string{
			"2006-01-02 15:04:05.999999",
			"2006-01-02 15:04:05",
			"2006-01-02T15:04:05",
		} {
			if t, err := time.Parse(layout, insertedAt); err == nil {
				date = t
				break
			}
		}
	}

	// Prefer data attributes; fallback to scrape if empty
	seedCount := seeds
	peerCount := peers
	if seedCount == 0 && peerCount == 0 {
		peerCount, seedCount, _ = goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, infoHash, mag.Trackers)
	}

	processedTitle := appendAudioISO639_2Code(torrentName, audio)

	ixt = schema.IndexedTorrent{
		Title:         releaseTitle,
		OriginalTitle: processedTitle,
		Details:       providerURL,
		Year:          year,
		IMDB:          imdbLink,
		Audio:         audio,
		MagnetLink:    magnetURI,
		Date:          date,
		InfoHash:      infoHash,
		Trackers:      mag.Trackers,
		LeechCount:    peerCount,
		SeedCount:     seedCount,
		Size:          formatBetorSize(sizeBytes),
	}

	return ixt, nil
}

func parseBetorLanguages(languages string) []schema.Audio {
	if languages == "" {
		return nil
	}
	parts := strings.Split(languages, ",")
	var result []schema.Audio
	seen := map[schema.Audio]bool{}

	for _, lang := range parts {
		lang = strings.TrimSpace(strings.ToLower(lang))
		var name schema.Audio
		switch lang {
		case "pt", "pt-br", "ptbr":
			name = "Português"
		case "en", "eng", "english":
			name = "Inglês"
		case "es", "spa", "spanish", "pt-pt":
			name = "Espanhol"
		default:
			continue
		}
		if !seen[name] {
			seen[name] = true
			result = append(result, name)
		}
	}
	return result
}

func formatBetorSize(bytes int64) string {
	if bytes <= 0 {
		return ""
	}
	const gb = 1024 * 1024 * 1024
	const mb = 1024 * 1024
	if bytes >= gb {
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(gb))
	}
	return fmt.Sprintf("%.0f MB", float64(bytes)/float64(mb))
}

// cleanBetorTitle removes common Brazilian torrent site prefixes that appear in titles.
// Complements the global CleanupTitleWebsites post-processor.
func cleanBetorTitle(title string) string {
	if title == "" {
		return title
	}
	// Remove common site prefixes (case-insensitive)
	prefixes := []string{
		`(?i)^BAIXARAPIDO\.COM[-.]`,
		`(?i)^COMANDO1\.COM[-.]`,
		`(?i)^BLUDV[-.]`,
		`(?i)^STARCKFILMES[-.]`,
		`(?i)^REDETORRENT[-.]`,
		`(?i)^TORRENTDOSFILMES[-.]`,
		`(?i)^WWW\.[A-Z0-9.-]+\.[A-Z]{2,}[-.]`,
		`(?i)^\[(?:EVITE|BAIXE|DOWNLOAD|WWW)[^\]]*\]\s*`,
	}
	for _, p := range prefixes {
		title = regexp.MustCompile(p).ReplaceAllString(title, "")
	}
	// Collapse multiple dots/spaces
	title = regexp.MustCompile(`[.\s]{2,}`).ReplaceAllString(title, " ")
	return strings.TrimSpace(title)
}
