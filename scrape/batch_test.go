package goscrape

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeTracker answers BEP 15 connect and scrape requests; every infohash gets
// seeders = its position in the request + 1. It counts scrape packets.
func fakeTracker(t *testing.T) (string, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var scrapes, maxHashes atomic.Int32
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			action := binary.BigEndian.Uint32(buf[8:])
			tid := binary.BigEndian.Uint32(buf[12:])
			switch action {
			case actionConnect:
				out := make([]byte, 16)
				binary.BigEndian.PutUint32(out[0:], actionConnect)
				binary.BigEndian.PutUint32(out[4:], tid)
				binary.BigEndian.PutUint64(out[8:], 42)
				_, _ = conn.WriteTo(out, addr)
			case actionScrap:
				count := (n - 16) / 20
				scrapes.Add(1)
				if int32(count) > maxHashes.Load() {
					maxHashes.Store(int32(count))
				}
				out := make([]byte, 8+12*count)
				binary.BigEndian.PutUint32(out[0:], actionScrap)
				binary.BigEndian.PutUint32(out[4:], tid)
				for i := 0; i < count; i++ {
					binary.BigEndian.PutUint32(out[8+12*i:], uint32(i+1))
				}
				_, _ = conn.WriteTo(out, addr)
			}
		}
	}()
	return "udp://" + conn.LocalAddr().String() + "/announce", &scrapes, &maxHashes
}

func TestScrapeBatchedSharesOneRequest(t *testing.T) {
	tracker, scrapes, maxHashes := fakeTracker(t)

	const n = 10
	var wg sync.WaitGroup
	got := make([]peers, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = scrapeBatched(tracker, fmt.Sprintf("%040x", i+1))
		}(i)
	}
	// the same hash twice in the batch is sent once
	wg.Add(1)
	var dup peers
	var dupErr error
	go func() {
		defer wg.Done()
		dup, dupErr = scrapeBatched(tracker, fmt.Sprintf("%040x", 1))
	}()
	wg.Wait()

	if s := scrapes.Load(); s != 1 {
		t.Fatalf("scrape requests = %d, want 1", s)
	}
	if m := maxHashes.Load(); m != n {
		t.Fatalf("hashes in the request = %d, want %d", m, n)
	}
	seen := map[int]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("hash %d: %v", i, errs[i])
		}
		seen[got[i].Seeders] = true
	}
	if len(seen) != n {
		t.Fatalf("each hash should get its own answer, got %v", got)
	}
	if dupErr != nil || dup != got[0] {
		t.Fatalf("duplicate hash got %v, %v; want %v", dup, dupErr, got[0])
	}
}

func TestScrapeBatchedRejectsBadInput(t *testing.T) {
	if _, err := scrapeBatched("http://tracker.example/announce", fmt.Sprintf("%040x", 1)); err != ErrUnsupportedScheme {
		t.Fatalf("http tracker: %v", err)
	}
	if _, err := scrapeBatched("udp://127.0.0.1:1/announce", "not-a-hash"); err != errInvalidInfohash {
		t.Fatalf("bad hash: %v", err)
	}
}
