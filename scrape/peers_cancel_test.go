package goscrape

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/module195/torrent-indexer/cache"
)

// slowTracker answers BEP 15 connect and scrape requests after delay; every
// infohash gets 7 seeders.
func slowTracker(t *testing.T, delay time.Duration) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			action := binary.BigEndian.Uint32(buf[8:])
			tid := binary.BigEndian.Uint32(buf[12:])
			var out []byte
			switch action {
			case actionConnect:
				out = make([]byte, 16)
				binary.BigEndian.PutUint32(out[0:], actionConnect)
				binary.BigEndian.PutUint32(out[4:], tid)
				binary.BigEndian.PutUint64(out[8:], 42)
			case actionScrap:
				count := (n - 16) / 20
				out = make([]byte, 8+12*count)
				binary.BigEndian.PutUint32(out[0:], actionScrap)
				binary.BigEndian.PutUint32(out[4:], tid)
				for i := 0; i < count; i++ {
					binary.BigEndian.PutUint32(out[8+12*i:], 7)
				}
				time.Sleep(delay)
			}
			_, _ = conn.WriteTo(out, addr)
		}
	}()
	return "udp://" + conn.LocalAddr().String() + "/announce"
}

// A caller that gives up returns at once, while the scrape it shares with
// another caller still finishes and answers that one.
func TestGetLeechsAndSeedsCallerCancelDoesNotFailSharedScrape(t *testing.T) {
	allowInternalTrackers(t)
	t.Setenv("REDIS_HOST", "127.0.0.1")
	resetTrackersMem(t)
	// no additional trackers: only the local one is scraped
	trackersMemMu.Lock()
	trackersMem = []string{"http://127.0.0.1:1/announce"}
	trackersMemExpires = time.Now().Add(time.Hour)
	trackersMemMu.Unlock()
	oldMax := maxAdditionalTrackers
	maxAdditionalTrackers = 1
	t.Cleanup(func() { maxAdditionalTrackers = oldMax })

	r := cache.NewRedis()
	tracker := slowTracker(t, 250*time.Millisecond)
	hash := "0123456789abcdef0123456789abcdef01234567"

	ctxA, cancelA := context.WithCancel(context.Background())
	errA := make(chan error, 1)
	start := time.Now()
	go func() {
		_, _, err := GetLeechsAndSeeds(ctxA, r, nil, hash, []string{tracker})
		errA <- err
	}()
	type res struct {
		seed int
		err  error
	}
	resB := make(chan res, 1)
	time.Sleep(20 * time.Millisecond)
	go func() {
		_, seed, err := GetLeechsAndSeeds(context.Background(), r, nil, hash, []string{tracker})
		resB <- res{seed, err}
	}()

	time.Sleep(30 * time.Millisecond)
	cancelA()
	select {
	case err := <-errA:
		if err != context.Canceled {
			t.Fatalf("cancelled caller got %v, want context.Canceled", err)
		}
		if el := time.Since(start); el > 200*time.Millisecond {
			t.Fatalf("cancelled caller returned after %v, want right after cancel", el)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled caller did not return")
	}

	select {
	case r := <-resB:
		if r.err != nil || r.seed != 7 {
			t.Fatalf("other caller got seed=%d err=%v, want 7 seeders", r.seed, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("other caller did not get the shared scrape")
	}
}
