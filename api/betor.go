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

	imdbPattern := reIMDbID
	isIMDb := imdbPattern.MatchString(strings.ToLower(q))

	// Bloqueia buscas por texto — só permite IMDB ID
	if q != "" && !isIMDb {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Response{
			Results:      []schema.IndexedTorrent{},
			Count:        0,
			IndexedCount: 0,
		})
		return
	}

	// Monta a URL
	if isIMDb {
		// Página dedicada por IMDb (mais estável e precisa)
		targetURL = base + "/imdb/" + strings.ToLower(q) + "/"
	} else if q != "" {
		// Busca normal (caso no futuro queira permitir texto)
		decoded, err := url.QueryUnescape(q)
		if err != nil {
			decoded = q
		}
		decoded = strings.ReplaceAll(decoded, "+", " ")
		decoded = strings.TrimSpace(decoded)

		exactMatch := false
		if strings.HasPrefix(decoded, `"`) && strings.HasSuffix(decoded, `"`) && len(decoded) > 1 {
			decoded = strings.Trim(decoded, `"`)
			exactMatch = true
		}

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

	// Extract expected title from page <title> for relevance filtering
	// Example: "Homem-Aranha: Um Novo Dia - Catálogo BeTor" → "Homem-Aranha: Um Novo Dia"
	pageTitle := strings.TrimSpace(doc.Find("title").First().Text())
	expectedTitle := pageTitle
	if idx := strings.Index(pageTitle, " - "); idx > 0 {
		expectedTitle = strings.TrimSpace(pageTitle[:idx])
	}
	expectedNorm := normalizeForMatchBetor(expectedTitle)

	// non-nil so an empty result encodes as [] rather than null
	indexedTorrents := make([]schema.IndexedTorrent, 0, len(items))
	for _, item := range items {
		ixt, err := parseBetorItem(ctx, i, item)
		if err != nil || ixt.InfoHash == "" {
			continue
		}

		// Relevance filter: discard results that don't reasonably match the page title
		// This removes polluted entries (Supergirl, O Diabo Veste Prada 2, etc.)
		// that the catalog incorrectly associates with this IMDb ID.
		if expectedNorm != "" && !isRelevantToTitleBetor(ixt.Title, expectedNorm) && !isRelevantToTitleBetor(ixt.OriginalTitle, expectedNorm) {
			continue
		}

		indexedTorrents = append(indexedTorrents, ixt)
	}

	// Apply post-processors
	for _, processor := range i.postProcessors {
		indexedTorrents = processor(i, r, indexedTorrents)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Response{
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

	// Prefer the clean data-torrent-name from the catalog over the dirty magnet dn=
	// The catalog name is already well-formed; magnet DisplayName usually has site prefixes and junk.
	var releaseTitle string
	if torrentName != "" {
		releaseTitle = torrentName
	} else {
		releaseTitle = mag.DisplayName
	}

	// Extract audio BEFORE cleaning title (cleanBetorTitle removes DUAL/DUBLADO/LEGENDADO)
	audio := parseBetorLanguages(languages)
	audio = getAudioFromTitle(torrentName, audio)

	// Light cleaning only (keep quality tags that are useful)
	releaseTitle = cleanBetorTitle(releaseTitle)
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
	yearRe := reYearWord

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

// DedupByInfoHash removes duplicate torrents by info_hash (the magnet's when
// it is missing), merging metadata: the first copy is kept, with the highest
// seed count and empty fields filled from the others. Runs for every indexer
// (GlobalPostProcessors), since several sites list a torrent more than once.
// Follows the GuIndex approach of using info_hash as the canonical key.
func DedupByInfoHash(_ *Indexer, _ *http.Request, torrents []schema.IndexedTorrent) []schema.IndexedTorrent {
	type entry struct {
		torrent schema.IndexedTorrent
		idx     int
	}
	best := map[string]*entry{}
	var order []string

	for i, t := range torrents {
		key := torrentKey(t)
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
	if out.MagnetLink == "" && b.MagnetLink != "" {
		out.MagnetLink = b.MagnetLink
	}
	if out.InfoHash == "" && b.InfoHash != "" {
		out.InfoHash = b.InfoHash
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
	s = reWhitespace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)

	// Restore preserved tags
	for ph, tag := range placeholders {
		s = strings.ReplaceAll(s, ph, tag)
	}

	return strings.TrimSpace(s)
}

// normalizeForMatchBetor lowercases, removes accents roughly, and strips non-alphanumeric.
func normalizeForMatchBetor(s string) string {
	s = strings.ToLower(s)
	// basic accent strip (enough for PT-BR titles)
	replacer := strings.NewReplacer(
		"á", "a", "à", "a", "ã", "a", "â", "a",
		"é", "e", "ê", "e",
		"í", "i",
		"ó", "o", "ô", "o", "õ", "o",
		"ú", "u", "ü", "u",
		"ç", "c",
	)
	s = replacer.Replace(s)
	// keep only letters, numbers and spaces
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == ' ' {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// isRelevantToTitleBetor returns true if the torrent title shares significant keywords with the expected title.
// Requires at least 2 matching significant words (length >= 3), or 1 if the expected title is short.
func isRelevantToTitleBetor(torrentTitle, expectedNorm string) bool {
	if expectedNorm == "" || torrentTitle == "" {
		return true // don't filter when we have nothing to compare
	}
	torrentNorm := normalizeForMatchBetor(torrentTitle)
	expectedWords := significantWordsBetor(expectedNorm)
	if len(expectedWords) == 0 {
		return true
	}
	torrentWords := map[string]bool{}
	for _, w := range strings.Fields(torrentNorm) {
		if len(w) >= 3 {
			torrentWords[w] = true
		}
	}
	matches := 0
	for _, w := range expectedWords {
		if torrentWords[w] {
			matches++
		}
	}
	threshold := 2
	if len(expectedWords) < 2 {
		threshold = 1
	}
	return matches >= threshold
}

func significantWordsBetor(s string) []string {
	var out []string
	stop := map[string]bool{"the": true, "a": true, "an": true, "of": true, "and": true, "um": true, "uma": true, "de": true, "do": true, "da": true, "dos": true, "das": true, "o": true, "os": true, "as": true}
	for _, w := range strings.Fields(s) {
		if len(w) >= 3 && !stop[w] {
			out = append(out, w)
		}
	}
	return out
}
