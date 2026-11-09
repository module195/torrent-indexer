package goscrape

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/module195/torrent-indexer/cache"
)

// allowInternalTrackers lets a test scrape its fake tracker on 127.0.0.1.
func allowInternalTrackers(t *testing.T) {
	t.Helper()
	old := allowPrivateTrackers.Swap(true)
	t.Cleanup(func() { allowPrivateTrackers.Store(old) })
}

// S-02 (auditoria de segurança 2026-11-09): trackers vêm de magnets enviados
// por terceiros (POST /indexers/manual) e de sites indexados; o scrape não pode
// virar um disparador de UDP para a rede interna do servidor.
func TestResolveTrackerRejectsInternalAddresses(t *testing.T) {
	old := allowPrivateTrackers.Swap(false)
	t.Cleanup(func() { allowPrivateTrackers.Store(old) })

	for _, hp := range []string{
		"127.0.0.1:6969",
		"10.0.0.1:1",
		"172.16.5.4:6379",
		"192.168.1.1:80",
		"169.254.169.254:80",
		"0.0.0.0:6969",
		"224.0.0.1:1",
		"[::1]:6969",
		"[fd00::1]:6969",
		"localhost:6969", // nome que resolve para loopback
	} {
		addr, err := resolveTracker(hp, time.Second)
		if !errors.Is(err, ErrInternalTracker) {
			t.Errorf("%s: got %q, %v; want ErrInternalTracker", hp, addr, err)
		}
	}
}

func TestResolveTrackerAllowsInternalWhenOptedIn(t *testing.T) {
	allowInternalTrackers(t)
	addr, err := resolveTracker("127.0.0.2:6969", time.Second)
	if err != nil || addr != "127.0.0.2:6969" {
		t.Fatalf("got %q, %v", addr, err)
	}
}

// A magnet with hundreds of tr= must not fan out to hundreds of scrapes.
func TestScrapePeersCapsMagnetTrackers(t *testing.T) {
	allowInternalTrackers(t)
	t.Setenv("REDIS_HOST", "127.0.0.1")
	resetTrackersMem(t)
	// no additional trackers: only the magnet's ones are scraped
	trackersMemMu.Lock()
	trackersMem = []string{"http://127.0.0.1:1/announce"}
	trackersMemExpires = time.Now().Add(time.Hour)
	trackersMemMu.Unlock()
	oldMax := maxAdditionalTrackers
	maxAdditionalTrackers = 1
	t.Cleanup(func() { maxAdditionalTrackers = oldMax })

	base, scrapes, _ := fakeTracker(t)
	trackers := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		trackers = append(trackers, fmt.Sprintf("%s%d", base, i))
	}
	hash := "89abcdef0123456789abcdef0123456789abcdef"
	_, _, _ = scrapePeers(context.Background(), cache.NewRedis(), hash, trackers)
	// the goroutines that lost the race still finish their scrape
	time.Sleep(batchWindow + scrapeTimeout)
	if got := int(scrapes.Load()); got > maxMagnetTrackers {
		t.Fatalf("%d scrapes for one magnet, want at most %d", got, maxMagnetTrackers)
	}
}
