package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	meilisearch "github.com/module195/torrent-indexer/search"
)

func strPtr(s string) *string     { return &s }
func floatPtr(f float64) *float64 { return &f }

func TestBetorDumpTorrentBase32Hash(t *testing.T) {
	// base32 and hex forms of the same info hash
	it := betorDumpItem{
		TorrentName: "Filme Exemplo 2020 1080p",
		MagnetXT:    "urn:btih:RLUZYFPVNDF3LBYU7FAAR44XW6FCDDS2",
		MagnetURI:   "magnet:?xt=urn:btih:RLUZYFPVNDF3LBYU7FAAR44XW6FCDDS2&tr=udp://tracker.example:80",
		Size:        floatPtr(1500000000),
	}
	got, ok := betorDumpTorrent(it, nil)
	if !ok {
		t.Fatal("item dropped")
	}
	if got.InfoHash != "8ae99c15f568cbb58714f94008f397b78a218e5a" {
		t.Errorf("InfoHash = %q", got.InfoHash)
	}
	if got.Year != "2020" || got.Size != "1.40 GB" || len(got.Trackers) != 1 || got.Indexer != "betor" {
		t.Errorf("unexpected torrent: %+v", got)
	}
}

func TestBetorDumpTorrentYearFromProviderURLAndNoSize(t *testing.T) {
	it := betorDumpItem{
		TorrentName: "Sem Ano 720p",
		MagnetURI:   "magnet:?xt=urn:btih:8AE99C15F568CBB58714F94008F397B78A218E5A",
		ProviderURL: "https://site.example/sem-ano-2019-dublado/",
	}
	got, ok := betorDumpTorrent(it, nil)
	if !ok {
		t.Fatal("item without year or size must be kept")
	}
	if got.Year != "2019" {
		t.Errorf("Year = %q, want 2019", got.Year)
	}
	if got.InfoHash != "8ae99c15f568cbb58714f94008f397b78a218e5a" {
		t.Errorf("InfoHash = %q, want lowercase hex", got.InfoHash)
	}
	doc := compactTorrentDocument(got)
	for _, k := range []string{"size", "date", "files", "imdb"} {
		if _, ok := doc[k]; ok {
			t.Errorf("empty field %q must be left out of the document", k)
		}
	}
}

func TestBetorDumpSuspiciousIMDb(t *testing.T) {
	id := strPtr("tt15239678")
	names := []string{
		"Duna Parte 2 2024 1080p WEB-DL DUAL", "Duna Parte 2 2024 1080p WEB-DL DUAL",
		"Duna Parte Dois 720p", "Duna Parte Dois 720p",
		"Dune Part Two 2024 2160p HDR", "Dune Part Two 2024 2160p HDR",
		"Duna Parte 2 4K", "Duna Parte 2 4K",
		"Supergirl 2026 1080p",
	}
	var items []betorDumpItem
	for i, n := range names {
		items = append(items, betorDumpItem{
			IMDbID:      id,
			TorrentName: n,
			MagnetURI:   fmt.Sprintf("magnet:?xt=urn:btih:%040x", i+1),
		})
	}
	docs := betorDumpDocuments(items)
	if len(docs) != len(names) {
		t.Fatalf("got %d documents, want %d (nothing is dropped)", len(docs), len(names))
	}
	for _, d := range docs {
		_, hasIMDb := d["imdb"]
		isSupergirl := strings.HasPrefix(d["title"].(string), "Supergirl")
		if hasIMDb == isSupergirl {
			t.Errorf("%q: imdb present = %v", d["title"], hasIMDb)
		}
	}
}

func TestBetorDumpRunUpsertsWithPut(t *testing.T) {
	dump := `[{"imdb_id":null,"torrent_name":"Filme 2021","magnet_xt":"urn:btih:8AE99C15F568CBB58714F94008F397B78A218E5A",
		"magnet_uri":"magnet:?xt=urn:btih:8AE99C15F568CBB58714F94008F397B78A218E5A","provider_url":"p",
		"languages":["pt","en"],"torrent_size":null,"torrent_num_seeds":3,"torrent_num_peers":1}]`
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, dump)
	}))
	defer src.Close()

	var method string
	var docs []map[string]any
	meili := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		_ = json.NewDecoder(r.Body).Decode(&docs)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer meili.Close()

	s := &BetorDumpSync{URL: src.URL, Search: meilisearch.NewSearchIndexer(meili.URL, "", "torrents")}
	n, err := s.Run(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("Run = %d, %v", n, err)
	}
	if method != http.MethodPut {
		t.Errorf("method = %s, want PUT", method)
	}
	if docs[0]["id"] != "8ae99c15f568cbb58714f94008f397b78a218e5a" || docs[0]["seed_count"] != float64(3) {
		t.Errorf("unexpected document: %v", docs[0])
	}
}
