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
	exactMatch := false

	if q != "" {
		// Decode first (mesmo padrão do baixarapido)
		decoded, err := url.QueryUnescape(q)
		if err != nil {
			decoded = q
		}
		decoded = strings.ReplaceAll(decoded, "+", " ")
		decoded = strings.TrimSpace(decoded)

		// Remove aspas existentes para evitar ""query""
		if strings.HasPrefix(decoded, `"`) && strings.HasSuffix(decoded, `"`) && len(decoded) > 1 {
			decoded = strings.Trim(decoded, `"`)
			exactMatch = true
		}

		// Exact match: envolve em aspas (estilo baixarapido)
		// Se não for exact, manda sem aspas (site do betor zera muitos resultados com aspas)
		var searchQuery string
		if exactMatch {
			searchQuery = `"` + decoded + `"`
		} else {
			searchQuery = decoded
		}

		targetURL = base + "/search/?q=" + url.QueryEscape(searchQuery)
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

// Após o loop de parse (linha ~96)
if q != "" {
    qNorm := strings.ToLower(q)
    tokens := strings.Fields(qNorm)
    var filtered []schema.IndexedTorrent
    for _, it := range indexedTorrents {
        // Título + URL do provider (slug PT)
        haystack := strings.ToLower(it.Title + " " + it.OriginalTitle + " " + it.Details)
        for _, tok := range tokens {
            if len(tok) > 2 && strings.Contains(haystack, tok) {
                filtered = append(filtered, it)
                break
            }
        }
    }
    // Só aplica se não zerar tudo
    if len(filtered) > 0 {
        indexedTorrents = filtered
    }
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

	// Extract audio BEFORE cleaning title (cleanBetorTitle removes DUAL/DUBLADO/LEGENDADO)
	audio := parseBetorLanguages(languages)
	audio = getAudioFromTitle(torrentName, audio)

	torrentName = cleanBetorTitle(torrentName)

	// Year extraction: try multiple sources
	year := extractBetorYear(torrentName, providerURL, item)

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

// extractBetorYear looks for a 4-digit year in multiple sources:
// 1. torrent name, 2. provider URL, 3. file names, 4. inserted date (fallback).
func extractBetorYear(torrentName, providerURL string, item *goquery.Selection) string {
	yearRe := regexp.MustCompile(`\b(19|20)\d{2}\b`)

	// 1. torrent name
	if y := yearRe.FindString(torrentName); y != "" {
		return y
	}

	// 2. provider URL (often contains year like "...-2026-dual...")
	if y := yearRe.FindString(providerURL); y != "" {
		return y
	}

	// 3. file names from data-torrent-files
	if files, ok := item.Attr("data-torrent-files"); ok && files != "" {
		for _, f := range strings.Split(files, ";") {
			if y := yearRe.FindString(f); y != "" {
				return y
			}
		}
	}

	// 4. Fallback: year from data-torrent-inserted-at (indexing date)
	if inserted, ok := item.Attr("data-torrent-inserted-at"); ok && inserted != "" {
		if y := yearRe.FindString(inserted); y != "" {
			return y
		}
	}

	return ""
}
// DedupByInfoHash removes duplicate torrents by info_hash, merging metadata.
// Keeps the entry with the highest seed count and fills empty fields from others.
// Follows the GuIndex approach of using info_hash as the canonical key.
func DedupByInfoHash(_ *Indexer, _ *http.Request, torrents []schema.IndexedTorrent) []schema.IndexedTorrent {
	type entry struct {
		torrent schema.IndexedTorrent
		idx     int
	}
	best := map[string]*entry{}
	var order []string

	for i, t := range torrents {
		key := strings.ToLower(strings.TrimSpace(t.InfoHash))
		if key == "" {
			// No info_hash: keep as-is with unique key
			key = fmt.Sprintf("__nohash_%d", i)
		}
		if existing, ok := best[key]; ok {
			existing.torrent = mergeTorrent(existing.torrent, t)
		} else {
			best[key] = &entry{torrent: t, idx: i}
			order = append(order, key)
		}
	}

	out := make([]schema.IndexedTorrent, 0, len(order))
	for _, key := range order {
		out = append(out, best[key].torrent)
	}
	return out
}

// mergeTorrent combines two torrents with the same info_hash.
// Prefers higher seed/leech counts and fills empty fields.
func mergeTorrent(a, b schema.IndexedTorrent) schema.IndexedTorrent {
	out := a

	// Prefer higher seed/leech
	if b.SeedCount > out.SeedCount {
		out.SeedCount = b.SeedCount
	}
	if b.LeechCount > out.LeechCount {
		out.LeechCount = b.LeechCount
	}

	// Fill empty string fields
	if out.Title == "" && b.Title != "" {
		out.Title = b.Title
	}
	if out.OriginalTitle == "" && b.OriginalTitle != "" {
		out.OriginalTitle = b.OriginalTitle
	}
	if out.Size == "" && b.Size != "" {
		out.Size = b.Size
	}
	if out.Year == "" && b.Year != "" {
		out.Year = b.Year
	}
	if out.Date.IsZero() && !b.Date.IsZero() {
		out.Date = b.Date
	}
	if out.IMDB == "" && b.IMDB != "" {
		out.IMDB = b.IMDB
	}
	if out.Details == "" && b.Details != "" {
		out.Details = b.Details
	}

	// Union of audio tags
	out.Audio = unionAudio(out.Audio, b.Audio)

	// Union of trackers
	out.Trackers = unionStrings(out.Trackers, b.Trackers)

	return out
}

func unionAudio(a, b []schema.Audio) []schema.Audio {
	seen := map[schema.Audio]bool{}
	var out []schema.Audio
	for _, x := range a {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	for _, x := range b {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range a {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	for _, x := range b {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
// Removes site prefixes, residual .EPISODIO-XX patterns, quality tags, and normalizes separators.
func cleanBetorTitle(raw string) string {
	if raw == "" {
		return ""
	}
	s := raw

	// Remove file extensions
	s = regexp.MustCompile(`(?i)\.(mkv|mp4|avi|torrent)$`).ReplaceAllString(s, "")

	// Normalize separators
	s = strings.ReplaceAll(s, ".", " ")
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, "+", " ")

	// Remove residual .EPISODIO-XX or EPISODIO XX patterns at start (Betor specific)
	s = regexp.MustCompile(`(?i)^[\s.-]*EPISODIO[\s.-]*\d+[\s.-]*`).ReplaceAllString(s, "")

	// Remove known site prefixes (case-insensitive)
	sitePrefixes := []string{
		"BAIXARAPIDO COM", "SITEDETORRENTS COM", "HIDRATORRENTS ORG",
		"VAMOSTORRENT COM", "TORRENTFILMESHDS", "BLUDV",
		"VEMTORRENT COM", "COMOEUBAIXO COM", "VACATORRENT COM",
		"HIDRATORRENT COM", "STARCKFILMES", "TDF",
		"COMANDO TORRENTS", "COMANDO1", "REDE TORRENT",
		"VACA TORRENT", "VIA FILMES", "BAIXANDO TORRENTS",
		"SEM TORRENT", "REDESTORRENTS",
	}
	for _, prefix := range sitePrefixes {
		s = regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(prefix) + `\s*[-.]?\s*`).ReplaceAllString(s, "")
	}

	// Generic pattern: remove any "*TORRENT COM" or "*TORRENTS COM" at start
	s = regexp.MustCompile(`(?i)^[A-Z]+TORRENTS?\s+COM\s*[-.]?\s*`).ReplaceAllString(s, "")

	// Remove [UNSAFE]
	s = regexp.MustCompile(`(?i)^\[UNSAFE\]\s*`).ReplaceAllString(s, "")

	// Remove file type at start
	s = regexp.MustCompile(`(?i)^(MP4|MKV|AVI)\s+`).ReplaceAllString(s, "")

	// Remove audio/encoding type at start
	s = regexp.MustCompile(`(?i)^(LEGENDADO|DUBLADO|DUAL-AUDIO|DUAL)\s+`).ReplaceAllString(s, "")

	// Remove quality tags at start (loop to handle nested cases)
	qualityPrefix := `(?i)^[-.\s]*(WEB[\s-]?DL|WEB[\s-]?RIP|1080P|720P|4K|MKV|MP4|X264|X265|5[\s.]?1|DUBLADO|DUAL[\s-]?AUDIO|LEGENDADO)\s*[-.\s]*`
	for i := 0; i < 5; i++ {
		prev := s
		s = regexp.MustCompile(qualityPrefix).ReplaceAllString(s, "")
		if s == prev {
			break
		}
	}

	// Preserve known hyphenated tags
	knownHyphen := []string{"WEB-DL", "WEB-RIP", "BD-RIP", "DUAL-AUDIO", "CAM-RIP"}
	placeholders := map[string]string{}
	for i, tag := range knownHyphen {
		ph := fmt.Sprintf("⟦%d⟧", i)
		if strings.Contains(strings.ToUpper(s), tag) {
			s = regexp.MustCompile(`(?i)`+regexp.QuoteMeta(tag)).ReplaceAllString(s, ph)
			placeholders[ph] = tag
		}
	}

	// Normalize spaces and hyphens
	s = strings.ReplaceAll(s, "-", " ")
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)

	// Restore preserved tags
	for ph, tag := range placeholders {
		s = strings.ReplaceAll(s, ph, tag)
	}

	return strings.TrimSpace(s)
}
