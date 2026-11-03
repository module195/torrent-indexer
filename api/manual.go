package handler

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/magnet"
	"github.com/module195/torrent-indexer/schema"
	goscrape "github.com/module195/torrent-indexer/scrape"
)

// manualTorrentsRedisKey is a hash: info hash -> manualEntry. One field per
// torrent, so concurrent POSTs don't overwrite each other (the old list was
// read, appended to and written back, and lost entries under concurrency).
const manualTorrentsRedisKey = "manual:torrents:v2"

// each entry is kept this long after it was added
var manualTorrentExpiration = 8 * time.Hour

const (
	manualMaxEntries  = 200
	manualMaxBodySize = 8 << 10
)

type ManualIndexerRequest struct {
	MagnetLink string `json:"magnetLink"`
}

type manualEntry struct {
	AddedAt time.Time             `json:"added_at"`
	Torrent schema.IndexedTorrent `json:"torrent"`
}

func writeManualError(w http.ResponseWriter, i *Indexer, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
	i.metrics.IndexerErrors.WithLabelValues("manual").Inc()
}

// manualPostAllowed checks MANUAL_API_TOKEN when it is set: the POST needs
// "Authorization: Bearer <token>". Unset keeps the endpoint open, as before.
func manualPostAllowed(r *http.Request) bool {
	token := os.Getenv("MANUAL_API_TOKEN")
	if token == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func (i *Indexer) HandlerManualIndexer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Method == http.MethodPost {
		if !manualPostAllowed(r) {
			writeManualError(w, i, http.StatusUnauthorized, errors.New("missing or wrong MANUAL_API_TOKEN"))
			return
		}
		var req ManualIndexerRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, manualMaxBodySize)).Decode(&req); err != nil {
			writeManualError(w, i, http.StatusBadRequest, err)
			return
		}
		m, err := magnet.ParseMagnetUri(req.MagnetLink)
		if err != nil {
			writeManualError(w, i, http.StatusBadRequest, err)
			return
		}
		infoHash := m.InfoHash.String()
		if strings.Trim(infoHash, "0") == "" {
			writeManualError(w, i, http.StatusBadRequest, errors.New("magnet link without info hash"))
			return
		}

		if n, err := i.redis.HLen(ctx, manualTorrentsRedisKey); err == nil && n >= manualMaxEntries {
			writeManualError(w, i, http.StatusInsufficientStorage, errors.New("too many manual torrents"))
			return
		}

		var audio []schema.Audio
		releaseTitle := m.DisplayName
		magnetAudio := getAudioFromTitle(releaseTitle, audio)
		peer, seed, err := goscrape.GetLeechsAndSeeds(ctx, i.redis, i.metrics, infoHash, m.Trackers)
		if err != nil {
			logging.ErrorWithRequest(r).Err(err).Str("info_hash", infoHash).Msg("Failed to get peers and seeds")
		}
		entry := manualEntry{
			AddedAt: time.Now(),
			Torrent: schema.IndexedTorrent{
				Title:         appendAudioISO639_2Code(releaseTitle, magnetAudio),
				OriginalTitle: processTitle(releaseTitle, magnetAudio),
				Audio:         magnetAudio,
				MagnetLink:    req.MagnetLink,
				InfoHash:      infoHash,
				Trackers:      m.Trackers,
				LeechCount:    peer,
				SeedCount:     seed,
			},
		}
		value, err := json.Marshal(entry)
		if err != nil {
			writeManualError(w, i, http.StatusInternalServerError, err)
			return
		}
		// the same magnet again replaces its entry (and renews it)
		if err := i.redis.HSet(ctx, manualTorrentsRedisKey, infoHash, value, manualTorrentExpiration); err != nil {
			logging.ErrorWithRequest(r).Err(err).Msg("Failed to save manual torrent to Redis")
			writeManualError(w, i, http.StatusInternalServerError, err)
			return
		}
	}

	stored, err := i.redis.HGetAll(ctx, manualTorrentsRedisKey)
	if err != nil {
		logging.ErrorWithRequest(r).Err(err).Msg("Failed to fetch manual torrents from Redis")
		writeManualError(w, i, http.StatusInternalServerError, err)
		return
	}

	entries := make([]manualEntry, 0, len(stored))
	var expired []string
	for hash, raw := range stored {
		var e manualEntry
		if json.Unmarshal([]byte(raw), &e) != nil || time.Since(e.AddedAt) > manualTorrentExpiration {
			expired = append(expired, hash)
			continue
		}
		entries = append(entries, e)
	}
	if len(expired) > 0 {
		_ = i.redis.HDel(ctx, manualTorrentsRedisKey, expired...)
	}
	// oldest first, as the list used to be
	slices.SortFunc(entries, func(a, b manualEntry) int { return a.AddedAt.Compare(b.AddedAt) })
	torrents := make([]schema.IndexedTorrent, 0, len(entries))
	for _, e := range entries {
		torrents = append(torrents, e.Torrent)
	}
	indexed := len(torrents)

	// q, filters, sorting and limit work as on the other indexers
	for _, p := range []PostProcessorFunc{AddSimilarityCheck, FilterBy, ApplySorting, ApplyLimit} {
		torrents = p(i, r, torrents)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(Response{
		Results:      torrents,
		Count:        len(torrents),
		IndexedCount: indexed,
	}); err != nil {
		logging.Error().Err(err).Msg("Failed to encode response")
	}
}
