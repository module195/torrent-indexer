package goscrape

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/module195/torrent-indexer/cache"
	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/monitoring"
	"github.com/module195/torrent-indexer/utils"
)

type peers struct {
	Seeders  int `json:"seed"`
	Leechers int `json:"leech"`
}

func getPeersFromCache(ctx context.Context, r *cache.Redis, infoHash string) (int, int, error) {
	// get peers and seeds from redis first
	peersCache, err := r.Get(ctx, cache.Key(cache.KindPeers, infoHash, infoHash))
	if err == nil {
		var peers peers
		err = json.Unmarshal(peersCache, &peers)
		if err != nil {
			return 0, 0, err
		}
		return peers.Leechers, peers.Seeders, nil
	}
	return 0, 0, err
}

func setPeersToCache(ctx context.Context, r *cache.Redis, infoHash string, peer, seed int) error {
	peers := peers{
		Seeders:  seed,
		Leechers: peer,
	}
	peersJSON, err := json.Marshal(peers)
	if err != nil {
		return err
	}
	err = r.SetWithExpiration(ctx, cache.Key(cache.KindPeers, infoHash, infoHash), peersJSON, 24*time.Hour)
	if err != nil {
		return err
	}
	return nil
}

// Phase 2 (opt-in) scrape limits. Zero/unset keeps the previous behaviour
// (every tracker, unbounded concurrency).
var (
	// maxAdditionalTrackers caps how many trackers from the dynamic/static
	// list are scraped (the list is ordered best-first). The magnet's own
	// trackers are always scraped.
	maxAdditionalTrackers = envInt("TRACKER_SCRAPE_MAX_ADDITIONAL")
	// scrapeSem bounds concurrent UDP scrapes across all requests.
	scrapeSem = newSemaphore(envInt("TRACKER_SCRAPE_CONCURRENCY"))
)

func envInt(name string) int {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func newSemaphore(n int) chan struct{} {
	if n <= 0 {
		return nil
	}
	return make(chan struct{}, n)
}

// limitTrackers keeps the first n UDP trackers (only UDP can be scraped;
// others fail immediately anyway). n <= 0 returns the list unchanged.
func limitTrackers(list []string, n int) []string {
	if n <= 0 {
		return list
	}
	out := make([]string, 0, n)
	for _, t := range list {
		if strings.HasPrefix(t, "udp://") {
			out = append(out, t)
			if len(out) == n {
				break
			}
		}
	}
	return out
}

func GetLeechsAndSeeds(ctx context.Context, r *cache.Redis, m *monitoring.Metrics, infoHash string, trackers []string) (int, int, error) {
	leech, seed, err := getPeersFromCache(ctx, r, infoHash)
	if err != nil {
		m.CacheMisses.WithLabelValues("peers").Inc()
		logging.Debug().Str("info_hash", infoHash).Msg("Unable to get peers from cache")
	} else {
		m.CacheHits.WithLabelValues("peers").Inc()
		logging.Debug().Str("info_hash", infoHash).Int("leech", leech).Int("seed", seed).Msg("Retrieved peers from cache")
		return leech, seed, nil
	}

	additionalTrackers := limitTrackers(getAdditionalTrackers(ctx, r), maxAdditionalTrackers)
	allTrackers := make([]string, 0, len(trackers)+len(additionalTrackers))
	allTrackers = append(allTrackers, trackers...)
	allTrackers = append(allTrackers, additionalTrackers...)
	allTrackers = utils.StableUniq(allTrackers)

	// buffered so tracker goroutines never block after we stop listening
	var peerChan = make(chan peers, len(allTrackers))
	var errChan = make(chan error, len(allTrackers))

	for _, tracker := range allTrackers {
		go func(tracker string) {
			if scrapeSem != nil {
				// results arriving after the 500ms window are discarded anyway,
				// so don't queue longer than that
				select {
				case scrapeSem <- struct{}{}:
					defer func() { <-scrapeSem }()
				case <-time.After(500 * time.Millisecond):
					errChan <- fmt.Errorf("scrape concurrency limit reached")
					return
				}
			}
			// get peers and seeds from redis first
			scraper, err := New(tracker)
			if err != nil {
				errChan <- err
				return
			}

			defer scraper.Close()

			scraper.SetTimeout(500 * time.Millisecond)

			res, err := scraper.Scrape([]byte(infoHash))
			if err != nil {
				errChan <- err
				return
			}
			if len(res) == 0 {
				errChan <- ErrResponse
				return
			}

			peerChan <- peers{
				Seeders:  int(res[0].Seeders),
				Leechers: int(res[0].Leechers),
			}
		}(tracker)
	}

	// prefer the first non-zero response
	timeout := time.After(500 * time.Millisecond)
	var fallbackPeer peers
	hasFallback := false
	responsesReceived := 0

	for responsesReceived < len(allTrackers) {
		select {
		case <-timeout:
			// Timeout reached, use fallback if available
			if hasFallback {
				err = setPeersToCache(ctx, r, infoHash, fallbackPeer.Leechers, fallbackPeer.Seeders)
				if err != nil {
					logging.Error().Err(err).Str("info_hash", infoHash).Msg("Failed to cache peer data")
				} else {
					logging.Debug().Str("info_hash", infoHash).Int("leech", fallbackPeer.Leechers).Int("seed", fallbackPeer.Seeders).Msg("Retrieved peers from tracker (fallback)")
				}
				return fallbackPeer.Leechers, fallbackPeer.Seeders, nil
			}
			return 0, 0, fmt.Errorf("unable to get peers from trackers for infohash: %s (timeout)", infoHash)
		case <-errChan:
			responsesReceived++
		case peer := <-peerChan:
			responsesReceived++
			// Store the first response as fallback
			if !hasFallback {
				fallbackPeer = peer
				hasFallback = true
			}
			// If we get a non-zero response, return immediately
			if peer.Seeders > 0 {
				err = setPeersToCache(ctx, r, infoHash, peer.Leechers, peer.Seeders)
				if err != nil {
					logging.Error().Err(err).Str("info_hash", infoHash).Msg("Failed to cache peer data")
				} else {
					logging.Debug().Str("info_hash", infoHash).Int("leech", peer.Leechers).Int("seed", peer.Seeders).Msg("Retrieved peers from tracker")
				}
				return peer.Leechers, peer.Seeders, nil
			} else if peer.Leechers > 0 {
				fallbackPeer = peer
			}
		}
	}

	// All trackers responded but none had non-zero peers, use fallback if available
	if hasFallback {
		err = setPeersToCache(ctx, r, infoHash, fallbackPeer.Leechers, fallbackPeer.Seeders)
		if err != nil {
			logging.Error().Err(err).Str("info_hash", infoHash).Msg("Failed to cache peer data")
		}
		return fallbackPeer.Leechers, fallbackPeer.Seeders, nil
	}

	return 0, 0, fmt.Errorf("unable to get peers from trackers for infohash: %s", infoHash)
}
