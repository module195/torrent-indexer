package handler

import (
	"context"
	"crypto/sha1" //nolint:gosec // BitTorrent v1 info hashes are SHA-1
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/module195/torrent-indexer/magnet"
	"github.com/module195/torrent-indexer/schema"
)

type memKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemKV() *memKV { return &memKV{m: map[string][]byte{}} }

func (c *memKV) Get(_ context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.m[key]; ok {
		return v, nil
	}
	return nil, errors.New("miss")
}

func (c *memKV) SetWithExpiration(_ context.Context, key string, value []byte, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = value
	return nil
}

func TestFullfilMissingMetadataFromTorrentCache(t *testing.T) {
	info := "d6:lengthi1610612736e4:name24:Lupin.S04.1080p.DUAL.mkv12:piece lengthi16384e6:pieces20:xxxxxxxxxxxxxxxxxxxxe"
	sum := sha1.Sum([]byte(info)) //nolint:gosec // BitTorrent v1 info hash
	hash := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+strings.ToUpper(hash)+".torrent" {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("d4:info" + info + "e"))
	}))
	defer srv.Close()

	i := &Indexer{magnetMetadataAPI: magnet.NewClient("", time.Second, nil).WithTorrentCache(srv.URL).WithCache(newMemKV())}
	r := httptest.NewRequest(http.MethodGet, "/indexers/x?q=lupin", nil)
	in := []schema.IndexedTorrent{
		{Title: "Lupin 4ª Temporada", OriginalTitle: "Lupin", MagnetLink: "magnet:?xt=urn:btih:" + hash},
		{Title: "Other", OriginalTitle: "Other", MagnetLink: "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20)},
	}
	run := func() []schema.IndexedTorrent {
		byHash := map[string]schema.IndexedTorrent{}
		for _, it := range FullfilMissingMetadata(i, r, in) {
			byHash[it.MagnetLink] = it
		}
		return []schema.IndexedTorrent{byHash[in[0].MagnetLink], byHash[in[1].MagnetLink]}
	}

	// the first search doesn't wait for the lookup
	if out := run(); out[0].Size != "" {
		t.Fatalf("first search waited for the lookup: size %q", out[0].Size)
	}
	// the next one finds it in the cache
	var out []schema.IndexedTorrent
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if out = run(); out[0].Size != "" || time.Now().After(deadline) {
			break
		}
	}
	if !strings.HasPrefix(out[0].Size, "1.5") {
		t.Errorf("size = %q", out[0].Size)
	}
	if out[0].SizeBytes != 1610612736 {
		t.Errorf("size_bytes = %d", out[0].SizeBytes)
	}
	if out[0].Title != "Lupin 4ª Temporada" {
		t.Errorf("title overwritten: %q", out[0].Title)
	}
	if len(out[0].Files) != 1 || out[0].Files[0].SizeBytes != 1610612736 {
		t.Errorf("files = %+v", out[0].Files)
	}
	if out[1].Size != "" || out[1].SizeBytes != 0 {
		t.Errorf("torrent missing from the cache got size %q", out[1].Size)
	}
}
