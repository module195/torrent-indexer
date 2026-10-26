package meilisearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/module195/torrent-indexer/schema"
)

// SearchIndexer integrates with Meilisearch to index and search torrent items.
type SearchIndexer struct {
	Client    *http.Client
	BaseURL   string
	APIKey    string
	IndexName string
}

// IndexStats represents statistics about the Meilisearch index
type IndexStats struct {
	NumberOfDocuments int64            `json:"numberOfDocuments"`
	IsIndexing        bool             `json:"isIndexing"`
	FieldDistribution map[string]int64 `json:"fieldDistribution"`
}

// HealthStatus represents the health status of Meilisearch
type HealthStatus struct {
	Status string `json:"status"`
}

// MaxSearchHits is the most hits one search returns (Meilisearch's default
// pagination.maxTotalHits).
const MaxSearchHits = 1000

// NewSearchIndexer creates a new instance of SearchIndexer.
func NewSearchIndexer(baseURL, apiKey, indexName string) *SearchIndexer {
	// guindex sends several /search at once, and the indexers upsert in the
	// background: the default 2 idle connections per host would make most of
	// these requests open a new one.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 32
	return &SearchIndexer{
		Client:    &http.Client{Timeout: 10 * time.Second, Transport: transport},
		BaseURL:   baseURL,
		APIKey:    apiKey,
		IndexName: indexName,
	}
}

// IndexTorrent indexes a single torrent item in Meilisearch.
func (t *SearchIndexer) IndexTorrent(torrent schema.IndexedTorrent) error {
	url := fmt.Sprintf("%s/indexes/%s/documents", t.BaseURL, t.IndexName)

	torrentWithKey := struct {
		Hash string `json:"id"`
		schema.IndexedTorrent
	}{
		Hash:           torrent.InfoHash,
		IndexedTorrent: torrent,
	}

	jsonData, err := json.Marshal(torrentWithKey)
	if err != nil {
		return fmt.Errorf("failed to marshal torrent data: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if t.APIKey != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", t.APIKey))
	}

	resp, err := t.Client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	return nil
}

func (t *SearchIndexer) IndexTorrents(torrents []schema.IndexedTorrent) error {
	url := fmt.Sprintf("%s/indexes/%s/documents", t.BaseURL, t.IndexName)

	torrentsWithKey := make([]struct {
		Hash string `json:"id"`
		schema.IndexedTorrent
	}, 0, len(torrents))
	for _, torrent := range torrents {
		torrentWithKey := struct {
			Hash string `json:"id"`
			schema.IndexedTorrent
		}{
			Hash:           torrent.InfoHash,
			IndexedTorrent: torrent,
		}
		torrentsWithKey = append(torrentsWithKey, torrentWithKey)
	}

	jsonData, err := json.Marshal(torrentsWithKey)
	if err != nil {
		return fmt.Errorf("failed to marshal torrent data: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if t.APIKey != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", t.APIKey))
	}

	resp, err := t.Client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	return nil
}

// UpsertDocuments adds or updates documents (PUT): fields a document does not
// carry keep the value already stored, unlike IndexTorrents (POST), which
// replaces the whole document. Each document needs an "id". Fails on any
// status other than 202, so callers can log it.
func (t *SearchIndexer) UpsertDocuments(docs []map[string]any) error {
	url := fmt.Sprintf("%s/indexes/%s/documents?primaryKey=id", t.BaseURL, t.IndexName)

	jsonData, err := json.Marshal(docs)
	if err != nil {
		return fmt.Errorf("failed to marshal documents: %w", err)
	}

	req, err := http.NewRequest(http.MethodPut, url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if t.APIKey != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", t.APIKey))
	}

	resp, err := t.Client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upsert failed: status %d, body: %s", resp.StatusCode, body)
	}
	return nil
}

// SearchTorrent searches indexed torrents in Meilisearch based on the query.
// Every word must match first; when nothing does ("the boys 4 temporada",
// "duna parte dois" against "Duna Parte 2"), it retries dropping words from
// the end, and the caller's similarity check discards what is too far off.
// At most 200 hits per search.
func (t *SearchIndexer) SearchTorrent(ctx context.Context, query string, limit int) ([]schema.IndexedTorrent, error) {
	attributes := []string{"title", "original_title"}
	limit = min(limit, 200)
	hits, err := t.search(ctx, query, limit, attributes, "all")
	if err != nil || len(hits) > 0 || len(strings.Fields(query)) < 2 {
		return hits, err
	}
	return t.search(ctx, query, limit, attributes, "last")
}

// SearchTorrentByIMDB searches the imdb field (an IMDb URL such as
// https://www.imdb.com/title/tt3581920/) for an ID like tt3581920. The ID is
// sent as a phrase so Meilisearch applies neither typo tolerance nor prefix
// matching; callers should still check the ID exactly. Every hit ties on
// relevance, so Meilisearch returns them in insertion order, oldest first:
// callers wanting the newest releases of a long series must ask for up to
// MaxSearchHits and sort them themselves.
func (t *SearchIndexer) SearchTorrentByIMDB(ctx context.Context, imdbID string, limit int) ([]schema.IndexedTorrent, error) {
	return t.search(ctx, `"`+imdbID+`"`, min(limit, MaxSearchHits), []string{"imdb"}, "all")
}

func (t *SearchIndexer) search(ctx context.Context, query string, limit int, attributes []string, matchingStrategy string) ([]schema.IndexedTorrent, error) {
	url := fmt.Sprintf("%s/indexes/%s/search", t.BaseURL, t.IndexName)
	requestBody := map[string]interface{}{
		"q":                    query,
		"limit":                limit,
		"matchingStrategy":     matchingStrategy,
		"attributesToSearchOn": attributes,
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal search query: %w", err)
	}

	// the client's context: a guindex that gave up stops the search too
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if t.APIKey != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", t.APIKey))
	}

	resp, err := t.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("search failed: %s", body)
	}

	var result struct {
		Hits []schema.IndexedTorrent `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse search response: %w", err)
	}

	return result.Hits, nil
}

// GetStats retrieves statistics about the Meilisearch index including document count.
// This method can be used for health checks and monitoring.
func (t *SearchIndexer) GetStats() (*IndexStats, error) {
	url := fmt.Sprintf("%s/indexes/%s/stats", t.BaseURL, t.IndexName)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	if t.APIKey != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", t.APIKey))
	}

	resp, err := t.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get stats: status %d, body: %s", resp.StatusCode, body)
	}

	var stats IndexStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil, fmt.Errorf("failed to parse stats response: %w", err)
	}

	return &stats, nil
}

// IsHealthy checks if Meilisearch is available and responsive.
// Returns true if the service is healthy, false otherwise.
func (t *SearchIndexer) IsHealthy() bool {
	url := fmt.Sprintf("%s/health", t.BaseURL)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false
	}

	// Use a shorter timeout for health checks
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// GetDocumentCount returns the number of indexed documents.
// This is a convenience method that extracts just the document count from stats.
func (t *SearchIndexer) GetDocumentCount() (int64, error) {
	stats, err := t.GetStats()
	if err != nil {
		return 0, err
	}
	return stats.NumberOfDocuments, nil
}
