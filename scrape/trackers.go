package goscrape

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/module195/torrent-indexer/cache"
	"github.com/module195/torrent-indexer/logging"
)

const (
	trackersListCacheKey        = "dynamic_trackers_list"
	trackersListCacheExpiration = 24 * time.Hour
)

var trackersListURLs = []string{
	"https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_best_ip.txt",
	"https://cdn.jsdelivr.net/gh/ngosang/trackerslist@master/trackers_best_ip.txt",
	"https://ngosang.github.io/trackerslist/trackers_best_ip.txt",
}

// fetchDynamicTrackers fetches the latest tracker list from GitHub and caches it
func fetchDynamicTrackers(ctx context.Context, r *cache.Redis) ([]string, error) {
	// Try to get from cache first
	cachedTrackers, err := r.Get(ctx, cache.Key(cache.KindMeta, trackersListCacheKey, trackersListCacheKey))
	if err == nil {
		var trackers []string
		err = json.Unmarshal(cachedTrackers, &trackers)
		if err == nil && len(trackers) > 0 {
			logging.Debug().Int("count", len(trackers)).Msg("Retrieved dynamic trackers from cache")
			return trackers, nil
		}
	}

	// Fetch from GitHub and mirrors
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	var lastErr error
	for _, url := range trackersListURLs {
		resp, err := client.Get(url)
		if err != nil {
			logging.Warn().Err(err).Str("url", url).Msg("Failed to fetch from tracker URL, trying next")
			lastErr = err
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
			logging.Warn().Err(err).Str("url", url).Msg("HTTP error from tracker URL, trying next")
			lastErr = err
			continue
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			logging.Warn().Err(err).Str("url", url).Msg("Failed to read response, trying next")
			lastErr = err
			continue
		}

		// Parse the tracker list
		lines := strings.Split(string(body), "\n")
		var trackers []string
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" {
				trackers = append(trackers, line)
			}
		}

		if len(trackers) == 0 {
			err = fmt.Errorf("no valid trackers found in response")
			logging.Warn().Err(err).Str("url", url).Msg("Empty tracker list, trying next")
			lastErr = err
			continue
		}

		// Cache the result
		trackersJSON, err := json.Marshal(trackers)
		if err == nil {
			err = r.SetWithExpiration(ctx, cache.Key(cache.KindMeta, trackersListCacheKey, trackersListCacheKey), trackersJSON, trackersListCacheExpiration)
			if err != nil {
				logging.Error().Err(err).Msg("Failed to cache dynamic trackers")
			} else {
				logging.Info().Int("count", len(trackers)).Str("url", url).Msg("Successfully cached dynamic trackers")
			}
		}

		return trackers, nil
	}

	// If we get here, all URLs failed
	logging.Error().Err(lastErr).Msg("Failed to fetch dynamic trackers from all mirror URLs")
	return nil, lastErr
}

// in-memory copy of the dynamic list, so each infohash doesn't hit Redis and
// re-decode the JSON list
const trackersMemoryTTL = 10 * time.Minute

var (
	trackersMemMu      sync.Mutex
	trackersMem        []string
	trackersMemExpires time.Time
)

// getAdditionalTrackers returns dynamic trackers with fallback to static ones
func getAdditionalTrackers(ctx context.Context, r *cache.Redis) []string {
	trackersMemMu.Lock()
	if len(trackersMem) > 0 && time.Now().Before(trackersMemExpires) {
		list := trackersMem
		trackersMemMu.Unlock()
		return list
	}
	trackersMemMu.Unlock()

	// Try to get dynamic trackers first
	dynamicTrackers, err := fetchDynamicTrackers(ctx, r)
	if err == nil && len(dynamicTrackers) > 0 {
		logging.Debug().Int("count", len(dynamicTrackers)).Msg("Using dynamic trackers")
		trackersMemMu.Lock()
		trackersMem = dynamicTrackers
		trackersMemExpires = time.Now().Add(trackersMemoryTTL)
		trackersMemMu.Unlock()
		return dynamicTrackers
	}

	// Fallback to static trackers
	logging.Warn().Err(err).Msg("Falling back to static trackers")
	return staticAdditionalTrackers
}

var staticAdditionalTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://p4p.arenabg.com:1337/announce",
	"udp://retracker.hotplug.ru:2710/announce",
	"http://tracker.bt4g.com:2095/announce",
	"http://bt.okmp3.ru:2710/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"http://tracker.mywaifu.best:6969/announce",
	"udp://ttk2.nbaonlineservice.com:6969/announce",
	"http://tracker.privateseedbox.xyz:2710/announce",
	"udp://evan.im:6969/announce",
	"https://tracker.yemekyedim.com:443/announce",
	"udp://retracker.lanta.me:2710/announce",
	"udp://martin-gebhardt.eu:25/announce",
	"http://tracker.beeimg.com:6969/announce",
	"udp://udp.tracker.projectk.org:23333/announce",
	"http://tracker.renfei.net:8080/announce",
	"https://tracker.expli.top:443/announce",
	"https://tr.nyacat.pw:443/announce",
	"udp://tracker.ducks.party:1984/announce",
	"udp://extracker.dahrkael.net:6969/announce",
	"http://ipv4.rer.lol:2710/announce",
	"udp://tracker.plx.im:6969/announce",
	"udp://tracker.tvunderground.org.ru:3218/announce",
	"http://tracker.tricitytorrents.com:2710/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.dler.com:6969/announce",
	"https://tracker.moeblog.cn:443/announce",
	"udp://d40969.acod.regrucolo.ru:6969/announce",
	"https://tracker.jdx3.org:443/announce",
	"http://ipv6.rer.lol:6969/announce",
	"udp://bandito.byterunner.io:6969/announce",
	"udp://tracker.gigantino.net:6969/announce",
	"http://tracker.netmap.top:6969/announce",
	"udp://tracker.yume-hatsuyuki.moe:6969/announce",
	"https://tracker.aburaya.live:443/announce",
	"udp://tracker.srv00.com:6969/announce",
	"udp://open.demonii.com:1337/announce",
	"udp://1c.premierzal.ru:6969/announce",
	"udp://tracker.fnix.net:6969/announce",
	"udp://tracker.kmzs123.cn:17272/announce",
	"https://tracker.home.kmzs123.cn:4443/announce",
	"udp://tracker-udp.gbitt.info:80/announce",
	"udp://tracker.torrust-demo.com:6969/announce",
	"udp://tracker.hifimarket.in:2710/announce",
	"udp://retracker01-msk-virt.corbina.net:80/announce",
	"https://tracker.ghostchu-services.top:443/announce",
	"udp://open.dstud.io:6969/announce",
	"udp://tracker.therarbg.to:6969/announce",
	"udp://tracker.bitcoinindia.space:6969/announce",
	"udp://www.torrent.eu.org:451/announce",
	"udp://tracker.hifitechindia.com:6969/announce",
	"udp://tracker.gmi.gd:6969/announce",
	"udp://tracker.skillindia.site:6969/announce",
	"http://tracker.ipv6tracker.ru:80/announce",
	"udp://tracker.tryhackx.org:6969/announce",
	"http://torrent.hificode.in:6969/announce",
	"http://open.trackerlist.xyz:80/announce",
	"http://taciturn-shadow.spb.ru:6969/announce",
	"http://0123456789nonexistent.com:80/announce",
	"http://shubt.net:2710/announce",
	"udp://tracker.valete.tf:9999/announce",
	"https://tracker.zhuqiy.top:443/announce",
	"https://tracker.leechshield.link:443/announce",
	"http://tracker.tritan.gg:8080/announce",
	"udp://t.overflow.biz:6969/announce",
	"udp://open.tracker.cl:1337/announce",
	"udp://explodie.org:6969/announce",
	"udp://exodus.desync.com:6969/announce",
	"udp://bt.ktrackers.com:6666/announce",
	"udp://wepzone.net:6969/announce",
	"udp://tracker2.dler.org:80/announce",
	"udp://tracker.theoks.net:6969/announce",
	"udp://tracker.ololosh.space:6969/announce",
	"udp://tracker.filemail.com:6969/announce",
	"udp://tracker.dump.cl:6969/announce",
	"udp://tracker.dler.org:6969/announce",
	"udp://tracker.bittor.pw:1337/announce",
}

// best-list refresh started by BestTrackers, so a burst of cold requests
// starts a single fetch
var trackersRefreshing atomic.Bool

// BestTrackers returns up to n trackers from the ranked public list, for the
// magnets the API hands out. It never blocks on the network: while the
// in-memory copy is cold it answers with the static list and, when Redis is
// available, refreshes the dynamic one in the background.
func BestTrackers(r *cache.Redis, n int) []string {
	trackersMemMu.Lock()
	list := trackersMem
	warm := len(list) > 0 && time.Now().Before(trackersMemExpires)
	trackersMemMu.Unlock()

	if !warm {
		if r != nil && trackersRefreshing.CompareAndSwap(false, true) {
			go func() {
				defer trackersRefreshing.Store(false)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				getAdditionalTrackers(ctx, r)
			}()
		}
		if len(list) == 0 {
			list = staticAdditionalTrackers
		}
	}

	out := make([]string, 0, n)
	for _, t := range list {
		if len(out) == n {
			break
		}
		lower := strings.ToLower(t)
		if strings.HasPrefix(lower, "udp://") || strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			out = append(out, t)
		}
	}
	return out
}
