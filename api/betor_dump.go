package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/module195/torrent-indexer/cache"
	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/magnet"
	"github.com/module195/torrent-indexer/schema"
	meilisearch "github.com/module195/torrent-indexer/search"
	"github.com/module195/torrent-indexer/utils"
)

const (
	betorDumpLockKey   = "betor-dump:lock"
	betorDumpBatchSize = 2000
	// betorDumpMaxBody caps the downloaded dump. It is far above the
	// catalog's size, and only there so a wrong URL can't fill the memory.
	betorDumpMaxBody = 512 << 20
	// betorDumpMaxItems caps the decoded entries. The byte cap alone did not
	// bound memory: each tiny entry became a ~120-byte struct, so a 16 MiB
	// dump of "{}" killed the process with a fatal out-of-memory error
	// (security audit 18/11). Over the cap the sync fails, not the process.
	betorDumpMaxItems = 2_000_000
	// betorDumpCommonShare is the share of an IMDb ID's torrent names a word
	// must appear in to count as part of that title.
	betorDumpCommonShare = 0.2
)

// betorDumpItem is one entry of the catalog dump (items.json). Numbers are
// float64 so a float in the dump does not fail the whole decode.
type betorDumpItem struct {
	IMDbID      *string  `json:"imdb_id"`
	TorrentName string   `json:"torrent_name"`
	MagnetXT    string   `json:"magnet_xt"`
	MagnetURI   string   `json:"magnet_uri"`
	ProviderURL string   `json:"provider_url"`
	Languages   []string `json:"languages"`
	Size        *float64 `json:"torrent_size"`
	Seeds       *float64 `json:"torrent_num_seeds"`
	Peers       *float64 `json:"torrent_num_peers"`
}

func (it betorDumpItem) imdbID() string {
	if it.IMDbID == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(*it.IMDbID))
}

// BetorDumpSync loads the betor catalog dump into Meilisearch on an interval,
// replacing the external jq sync container.
type BetorDumpSync struct {
	URL      string
	Interval time.Duration
	Search   *meilisearch.SearchIndexer
	Redis    *cache.Redis
	Client   *http.Client
	// RetryDelays overrides betorDumpRetryDelays (tests).
	RetryDelays []time.Duration
}

// betorDumpRetryDelays are the waits before retrying a failed sync, one per
// attempt; once they run out, the next try is the regular interval.
var betorDumpRetryDelays = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute}

// Start runs one sync right away and then one per interval, until ctx ends.
// A failed sync is retried after betorDumpRetryDelays (at start Meilisearch
// may still be coming up) instead of waiting for the next interval.
func (s *BetorDumpSync) Start(ctx context.Context) {
	delays := s.RetryDelays
	if delays == nil {
		delays = betorDumpRetryDelays
	}
	go func() {
		ticker := time.NewTicker(s.Interval)
		defer ticker.Stop()
		var retry <-chan time.Time
		attempt := 0
		run := func() {
			if s.runLocked(ctx) {
				attempt, retry = 0, nil
				return
			}
			if attempt < len(delays) {
				d := delays[attempt]
				attempt++
				logging.Info().Dur("in", d).Int("attempt", attempt).Msg("Betor dump sync will retry")
				retry = time.After(d)
				return
			}
			attempt, retry = 0, nil
		}
		run()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			case <-retry:
				run()
			}
		}
	}()
}

// runLocked runs a sync unless another replica holds the lock. Without Redis
// (or with Redis down) it runs anyway: a duplicate upsert is harmless. It
// reports false only when this run failed; the lock is then released, so a
// retry (or a restart) is not skipped for the lock's whole TTL.
func (s *BetorDumpSync) runLocked(ctx context.Context) (ok bool) {
	// runs in the background sync goroutine: a panic on dump data must cost
	// this sync, not the process
	defer utils.RecoverAsError(nil, "betor dump sync")
	locked := false
	if s.Redis != nil {
		ttl := s.Interval / 2
		if ttl < 10*time.Minute {
			ttl = 10 * time.Minute
		}
		got, err := s.Redis.SetNX(ctx, betorDumpLockKey, []byte(time.Now().Format(time.RFC3339)), ttl)
		if err == nil && !got {
			logging.Info().Msg("Betor dump sync skipped: another replica ran it recently")
			return true
		}
		locked = err == nil
	}
	start := time.Now()
	n, err := s.Run(ctx)
	if err != nil {
		logging.Error().Err(err).Int("documents", n).Msg("Betor dump sync failed")
		if locked {
			if derr := s.Redis.Del(context.WithoutCancel(ctx), betorDumpLockKey); derr != nil {
				logging.Warn().Err(derr).Msg("Failed to release the betor dump lock")
			}
		}
		return false
	}
	logging.Info().Int("documents", n).Dur("took", time.Since(start)).Msg("Betor dump sync done")
	return true
}

// Run downloads the dump, converts it and upserts it in batches. It returns
// how many documents were sent.
func (s *BetorDumpSync) Run(ctx context.Context) (int, error) {
	items, err := s.fetch(ctx)
	if err != nil {
		return 0, err
	}
	docs := betorDumpDocuments(items)
	sent := 0
	for start := 0; start < len(docs); start += betorDumpBatchSize {
		end := min(start+betorDumpBatchSize, len(docs))
		if err := s.Search.UpsertDocuments(docs[start:end]); err != nil {
			return sent, err
		}
		sent = end
	}
	return sent, nil
}

func (s *BetorDumpSync) fetch(ctx context.Context) ([]betorDumpItem, error) {
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download dump: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download dump: status %d", resp.StatusCode)
	}
	// external response: bounded like every other one (security audit 28/10)
	body := http.MaxBytesReader(nil, resp.Body, betorDumpMaxBody)
	return decodeBetorDump(body, betorDumpMaxItems)
}

// decodeBetorDump streams the dump's JSON array one entry at a time. Entries
// with no magnet are dropped (betorDumpTorrent would skip them anyway), and
// more than maxItems useful entries fails the decode instead of growing the
// slice without bound.
func decodeBetorDump(r io.Reader, maxItems int) ([]betorDumpItem, error) {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("decode dump: %w", err)
	}
	if tok != json.Delim('[') {
		return nil, fmt.Errorf("decode dump: expected a JSON array")
	}
	var items []betorDumpItem
	for dec.More() {
		var it betorDumpItem
		if err := dec.Decode(&it); err != nil {
			return nil, fmt.Errorf("decode dump: %w", err)
		}
		if it.MagnetURI == "" && it.MagnetXT == "" {
			continue
		}
		if len(items) >= maxItems {
			return nil, fmt.Errorf("decode dump: more than %d items", maxItems)
		}
		items = append(items, it)
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("decode dump: %w", err)
	}
	return items, nil
}

// betorDumpDocuments converts the dump into Meilisearch documents, one per
// info hash (the last entry wins).
func betorDumpDocuments(items []betorDumpItem) []map[string]any {
	common := betorDumpCommonWords(items)
	pos := make(map[string]int, len(items))
	docs := make([]map[string]any, 0, len(items))
	for _, it := range items {
		t, ok := betorDumpTorrent(it, common)
		if !ok {
			continue
		}
		doc := compactTorrentDocument(t)
		if i, seen := pos[t.InfoHash]; seen {
			docs[i] = doc
			continue
		}
		pos[t.InfoHash] = len(docs)
		docs = append(docs, doc)
	}
	return docs
}

// betorDumpTorrent converts one dump entry the way parseBetorItem converts a
// catalog page item. It drops the IMDb link (but keeps the torrent) when the
// name shares no word with the other names under that IMDb ID.
func betorDumpTorrent(it betorDumpItem, common map[string]map[string]bool) (schema.IndexedTorrent, bool) {
	var t schema.IndexedTorrent

	mag, err := magnet.ParseMagnetUri(it.MagnetURI)
	if err != nil {
		if it.MagnetXT == "" {
			return t, false
		}
		// the magnet link is broken but the xt alone still gives the hash
		mag, err = magnet.ParseMagnetUri("magnet:?xt=" + it.MagnetXT)
		if err != nil {
			return t, false
		}
	}

	audio := parseBetorLanguages(strings.Join(it.Languages, ","))
	audio = getAudioFromTitle(it.TorrentName, audio)
	name := cleanBetorTitle(it.TorrentName)
	if name == "" {
		name = cleanBetorTitle(mag.DisplayName)
	}

	year := reYearWord.FindString(name)
	if year == "" {
		year = reYearWord.FindString(it.ProviderURL)
	}

	imdbLink := ""
	if id := it.imdbID(); reIMDbID.MatchString(id) && !betorDumpSuspicious(it.TorrentName, common[id]) {
		imdbLink = "https://www.imdb.com/title/" + id + "/"
	}

	var size int64
	if it.Size != nil {
		size = int64(*it.Size)
	}

	t = schema.IndexedTorrent{
		Title:         name,
		OriginalTitle: appendAudioISO639_2Code(name, audio),
		Details:       it.ProviderURL,
		Year:          year,
		IMDB:          imdbLink,
		Audio:         audio,
		MagnetLink:    it.MagnetURI,
		InfoHash:      mag.InfoHash.String(),
		Trackers:      mag.Trackers,
		Size:          formatBetorSize(size),
		SeedCount:     floatToInt(it.Seeds),
		LeechCount:    floatToInt(it.Peers),
		Indexer:       betor.Label,
	}
	if t.MagnetLink == "" {
		t.MagnetLink = "magnet:?xt=" + it.MagnetXT
	}
	return t, true
}

func floatToInt(f *float64) int {
	if f == nil {
		return 0
	}
	return int(*f)
}

// compactTorrentDocument turns a torrent into a Meilisearch document without
// empty fields, so an upsert does not wipe what a scraper stored for the same
// hash (date, files, year...).
func compactTorrentDocument(t schema.IndexedTorrent) map[string]any {
	doc := map[string]any{
		"id":          t.InfoHash,
		"info_hash":   t.InfoHash,
		"seed_count":  t.SeedCount,
		"leech_count": t.LeechCount,
	}
	set := func(key, value string) {
		if value != "" {
			doc[key] = value
		}
	}
	set("title", t.Title)
	set("original_title", t.OriginalTitle)
	set("details", t.Details)
	set("year", t.Year)
	set("imdb", t.IMDB)
	set("magnet_link", t.MagnetLink)
	set("size", t.Size)
	set("indexer", t.Indexer)
	if len(t.Audio) > 0 {
		doc["audio"] = t.Audio
	}
	if len(t.Trackers) > 0 {
		doc["trackers"] = t.Trackers
	}
	if len(t.Files) > 0 {
		doc["files"] = t.Files
	}
	if !t.Date.IsZero() {
		doc["date"] = t.Date
		// numeric, so Meilisearch can sort by it (the RFC 3339 string mixes
		// offsets and would sort as text)
		doc["date_ts"] = t.Date.Unix()
	}
	// the bare ID (tt1234567), filterable: /search by IMDb filters on it
	// instead of matching the link as text
	if id := reIMDbIDInLink.FindString(strings.ToLower(t.IMDB)); id != "" {
		doc["imdb_id"] = id
	}
	return doc
}

// betorDumpJunk are release words that say nothing about which title it is.
var betorDumpJunk = map[string]bool{
	"dual": true, "dublado": true, "legendado": true, "nacional": true, "fullhd": true,
	"bluray": true, "bdrip": true, "webdl": true, "webrip": true, "remux": true,
	"hdtv": true, "hdrip": true, "camrip": true, "complete": true, "completa": true,
	"temporada": true, "season": true, "starckfilmes": true, "torrent": true, "download": true,
}

// betorDumpWords returns the distinct title words of a torrent name: letters
// only, 4+ characters, release tags left out.
func betorDumpWords(name string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.Fields(normalizeForMatchBetor(strings.NewReplacer(".", " ", "-", " ", "_", " ").Replace(name))) {
		if len(w) < 4 || betorDumpJunk[w] || seen[w] || strings.ContainsAny(w, "0123456789") {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// betorDumpCommonWords maps each IMDb ID to the words found in at least
// betorDumpCommonShare of its torrent names.
func betorDumpCommonWords(items []betorDumpItem) map[string]map[string]bool {
	counts := map[string]map[string]int{}
	totals := map[string]int{}
	for _, it := range items {
		id := it.imdbID()
		if id == "" {
			continue
		}
		totals[id]++
		if counts[id] == nil {
			counts[id] = map[string]int{}
		}
		for _, w := range betorDumpWords(it.TorrentName) {
			counts[id][w]++
		}
	}
	common := make(map[string]map[string]bool, len(counts))
	for id, words := range counts {
		for w, n := range words {
			if float64(n)/float64(totals[id]) >= betorDumpCommonShare {
				if common[id] == nil {
					common[id] = map[string]bool{}
				}
				common[id][w] = true
			}
		}
	}
	return common
}

// betorDumpSuspicious reports whether a name shares no word with the common
// words of its IMDb ID, i.e. the catalog likely tagged it with the wrong ID.
func betorDumpSuspicious(name string, common map[string]bool) bool {
	if len(common) == 0 {
		return false
	}
	for _, w := range betorDumpWords(name) {
		if common[w] {
			return false
		}
	}
	return true
}
