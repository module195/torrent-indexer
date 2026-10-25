package magnet

import (
	"context"
	"crypto/sha1" //nolint:gosec // BitTorrent v1 info hashes are SHA-1
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// bencode encodes test values: string, int, []any and map[string]any.
func bencode(v any) string {
	switch v := v.(type) {
	case string:
		return fmt.Sprintf("%d:%s", len(v), v)
	case int:
		return fmt.Sprintf("i%de", v)
	case []any:
		var b strings.Builder
		b.WriteString("l")
		for _, e := range v {
			b.WriteString(bencode(e))
		}
		b.WriteString("e")
		return b.String()
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("d")
		for _, k := range keys {
			b.WriteString(bencode(k) + bencode(v[k]))
		}
		b.WriteString("e")
		return b.String()
	}
	panic(fmt.Sprintf("bencode: unsupported %T", v))
}

// makeTorrent returns a .torrent with the given info dict and its info hash.
func makeTorrent(info map[string]any) ([]byte, string) {
	sum := sha1.Sum([]byte(bencode(info))) //nolint:gosec // BitTorrent v1 info hash
	data := bencode(map[string]any{
		"announce":      "udp://tracker.example:1337/announce",
		"created by":    "test",
		"creation date": 1700000000,
		"info":          info,
	})
	return []byte(data), hex.EncodeToString(sum[:])
}

func multiFileInfo() map[string]any {
	return map[string]any{
		"name":         "The.Boys.S05.1080p",
		"piece length": 262144,
		"pieces":       strings.Repeat("x", 20),
		"files": []any{
			map[string]any{"length": 1500, "path": []any{"The.Boys.S05E01.mkv"}},
			map[string]any{"length": 12, "path": []any{".pad", "12"}, "attr": "p"},
			map[string]any{"length": 2500, "path": []any{"Subs", "pt.srt"}},
		},
	}
}

func TestParseTorrentFileSingleFile(t *testing.T) {
	data, hash := makeTorrent(map[string]any{
		"name":         "Lupin.2026.mkv",
		"length":       1234567,
		"piece length": 262144,
		"pieces":       strings.Repeat("x", 20),
	})
	m, err := parseTorrentFile(data, strings.ToUpper(hash))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "Lupin.2026.mkv" || m.Size != 1234567 || m.InfoHash != hash {
		t.Fatalf("got name=%q size=%d hash=%q", m.Name, m.Size, m.InfoHash)
	}
	if len(m.Files) != 1 || m.Files[0].Path != "Lupin.2026.mkv" {
		t.Fatalf("files = %+v", m.Files)
	}
	if m.CreatedBy != "test" || m.CreatedAt.Unix() != 1700000000 {
		t.Fatalf("created = %q %v", m.CreatedBy, m.CreatedAt)
	}
}

func TestParseTorrentFileMultiFileSkipsPadding(t *testing.T) {
	data, hash := makeTorrent(multiFileInfo())
	m, err := parseTorrentFile(data, hash)
	if err != nil {
		t.Fatal(err)
	}
	if m.Size != 4000 {
		t.Fatalf("size = %d, want 4000", m.Size)
	}
	want := []TorrentFile{
		{Path: "The.Boys.S05.1080p/The.Boys.S05E01.mkv", Size: 1500, Offset: 0},
		{Path: "The.Boys.S05.1080p/Subs/pt.srt", Size: 2500, Offset: 1512},
	}
	if fmt.Sprint(m.Files) != fmt.Sprint(want) {
		t.Fatalf("files = %+v, want %+v", m.Files, want)
	}
}

func TestParseTorrentFileRejects(t *testing.T) {
	data, _ := makeTorrent(multiFileInfo())
	other := strings.Repeat("ab", 20)
	for name, tc := range map[string]struct {
		data []byte
		hash string
	}{
		"wrong info hash": {data, other},
		"html page":       {[]byte("<!DOCTYPE html><html></html>"), other},
		"truncated":       {data[:len(data)/2], other},
		"no info":         {[]byte(bencode(map[string]any{"announce": "x"})), other},
		"deep nesting":    {[]byte(strings.Repeat("l", 100) + strings.Repeat("e", 100)), other},
	} {
		if _, err := parseTorrentFile(tc.data, tc.hash); !errors.Is(err, errNotTorrent) {
			t.Errorf("%s: err = %v, want errNotTorrent", name, err)
		}
	}
}

func newCacheServer(t *testing.T, torrents map[string][]byte, status int, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		hash := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/torrent/"), ".torrent")
		if hash != strings.ToUpper(hash) {
			t.Errorf("hash %q is not upper case", hash)
		}
		data, ok := torrents[strings.ToLower(hash)]
		if !ok {
			http.Redirect(w, r, "/search/"+hash, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/x-bittorrent")
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchMetadataFromTorrentCache(t *testing.T) {
	data, hash := makeTorrent(multiFileInfo())
	var hits atomic.Int32
	srv := newCacheServer(t, map[string][]byte{hash: data}, 0, &hits)
	c := NewClient("", time.Second, nil).WithTorrentCache(srv.URL + "/torrent")
	if !c.IsEnabled() {
		t.Fatal("client with a torrent cache should be enabled")
	}

	// concurrent lookups of the same hash share one request
	errs := make(chan error, 5)
	for range 5 {
		go func() {
			m, err := c.FetchMetadata(context.Background(), "magnet:?xt=urn:btih:"+hash+"&dn=x")
			if err == nil && m.Size != 4000 {
				err = fmt.Errorf("size = %d", m.Size)
			}
			errs <- err
		}()
	}
	for range 5 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("cache hit %d times, want 1", n)
	}

	_, err := c.FetchMetadata(context.Background(), "magnet:?xt=urn:btih:"+strings.Repeat("cd", 20))
	if !errors.Is(err, ErrMetadataNotFound) {
		t.Fatalf("missing torrent: err = %v, want ErrMetadataNotFound", err)
	}
}

func TestFetchMetadataTransientErrorIsNotAMiss(t *testing.T) {
	var hits atomic.Int32
	srv := newCacheServer(t, nil, http.StatusTooManyRequests, &hits)
	c := NewClient("", time.Second, nil).WithTorrentCache(srv.URL + "/torrent/")
	_, err := c.FetchMetadata(context.Background(), "magnet:?xt=urn:btih:"+strings.Repeat("ef", 20))
	if err == nil || errors.Is(err, ErrMetadataNotFound) {
		t.Fatalf("err = %v, want a non-miss error", err)
	}
}

func TestFetchMetadataReturnsAtDeadline(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)
	c := NewClient("", time.Second, nil).WithTorrentCache(srv.URL + "/")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.FetchMetadata(ctx, "magnet:?xt=urn:btih:"+strings.Repeat("01", 20))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("returned after %v", time.Since(start))
	}
}

func TestDisabledClient(t *testing.T) {
	var c *MetadataClient
	if c.IsEnabled() {
		t.Fatal("nil client is enabled")
	}
	if NewClient("", time.Second, nil).IsEnabled() {
		t.Fatal("client without sources is enabled")
	}
	if NewClient("", time.Second, nil).WithTorrentCache("").IsEnabled() {
		t.Fatal("empty torrent cache URL enabled the client")
	}
}
