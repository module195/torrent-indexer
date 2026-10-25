package magnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/module195/torrent-indexer/cache"
	"github.com/module195/torrent-indexer/logging"
)

type MetadataRequest struct {
	MagnetURI string `json:"magnet_uri"`
}

type TorrentFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Offset int64  `json:"offset"`
}

type MetadataResponse struct {
	InfoHash    string        `json:"info_hash"`
	Name        string        `json:"name"`
	Size        int64         `json:"size"`
	Files       []TorrentFile `json:"files"`
	CreatedBy   string        `json:"created_by"`
	CreatedAt   time.Time     `json:"created_at"`
	Comment     string        `json:"comment"`
	Trackers    []string      `json:"trackers"`
	DownloadURL string        `json:"download_url"`
}

// MetadataClient finds a torrent's name, size and files by info hash. It asks
// a public .torrent cache first (one fast HTTP request) and then, when
// configured, the magnet-metadata-api, which fetches the metadata from peers
// over the DHT. Results are cached in Redis, and so are misses, for a while.
type MetadataClient struct {
	baseURL    string
	httpClient *http.Client
	c          *cache.Redis

	torrentCacheURL  string
	torrentCacheHTTP *http.Client
	torrentCacheSem  chan struct{}

	mu       sync.Mutex
	inflight map[string]*lookup
}

// lookup is one metadata fetch shared by every caller asking for the same
// info hash while it runs.
type lookup struct {
	done chan struct{}
	m    *MetadataResponse
	err  error
}

const (
	metadataTTL     = 7 * 24 * time.Hour
	metadataMissTTL = 6 * time.Hour
	// lookupTimeout bounds a fetch that outlives its caller: it keeps running
	// so the result still reaches the cache for the next search.
	lookupTimeout           = 30 * time.Second
	torrentCacheTimeout     = 5 * time.Second
	torrentCacheConcurrency = 8
)

// ErrMetadataNotFound means no source had the torrent, now or recently.
var ErrMetadataNotFound = errors.New("torrent metadata not found")

// NewClient returns a client for the magnet-metadata-api at baseURL (empty
// disables it). The public torrent cache is off until WithTorrentCache.
func NewClient(baseURL string, timeout time.Duration, c *cache.Redis) *MetadataClient {
	return &MetadataClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:      100,
				IdleConnTimeout:   30 * time.Second,
				ForceAttemptHTTP2: true,
			},
		},
		c:        c,
		inflight: map[string]*lookup{},
	}
}

// WithTorrentCache enables the public .torrent cache at url (the info hash
// and ".torrent" are appended). An empty url leaves it off.
func (c *MetadataClient) WithTorrentCache(url string) *MetadataClient {
	if url == "" {
		return c
	}
	if !strings.HasSuffix(url, "/") {
		url += "/"
	}
	c.torrentCacheURL = url
	c.torrentCacheHTTP = &http.Client{
		Timeout: torrentCacheTimeout,
		// a miss redirects to an HTML page: don't follow it
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	c.torrentCacheSem = make(chan struct{}, torrentCacheConcurrency)
	return c
}

func (c *MetadataClient) IsEnabled() bool {
	return c != nil && (c.baseURL != "" || c.torrentCacheURL != "")
}

// FetchMetadata returns the metadata for magnetURI. It returns when the
// metadata arrives or ctx is done, whichever comes first; in the second case
// the fetch goes on in the background and caches its result.
func (c *MetadataClient) FetchMetadata(ctx context.Context, magnetURI string) (*MetadataResponse, error) {
	if !c.IsEnabled() {
		return nil, fmt.Errorf("magnet metadata API is not enabled")
	}
	m, err := ParseMagnetUri(magnetURI)
	if err != nil {
		return nil, fmt.Errorf("failed to parse magnet URI: %w", err)
	}
	infoHash := m.InfoHash.HexString()
	cacheKey := "metadata:" + infoHash
	missKey := "metadata:miss:" + infoHash

	// Check cache first
	if c.c != nil {
		if cachedData, err := c.c.Get(ctx, cacheKey); err == nil && cachedData != nil {
			var cachedMetadata MetadataResponse
			if err := json.Unmarshal(cachedData, &cachedMetadata); err == nil {
				return &cachedMetadata, nil
			}
		}
		if miss, err := c.c.Get(ctx, missKey); err == nil && miss != nil {
			return nil, ErrMetadataNotFound
		}
	}

	c.mu.Lock()
	l, ok := c.inflight[infoHash]
	if !ok {
		l = &lookup{done: make(chan struct{})}
		c.inflight[infoHash] = l
		go c.run(context.WithoutCancel(ctx), l, magnetURI, infoHash, cacheKey, missKey)
	}
	c.mu.Unlock()

	select {
	case <-l.done:
		return l.m, l.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *MetadataClient) run(ctx context.Context, l *lookup, magnetURI, infoHash, cacheKey, missKey string) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	defer func() {
		c.mu.Lock()
		delete(c.inflight, infoHash)
		c.mu.Unlock()
		close(l.done)
	}()

	l.m, l.err = c.fetch(ctx, magnetURI, infoHash)
	if c.c == nil {
		return
	}
	if l.err == nil {
		if data, err := json.Marshal(l.m); err == nil {
			_ = c.c.SetWithExpiration(ctx, cacheKey, data, metadataTTL)
		}
	} else if errors.Is(l.err, ErrMetadataNotFound) {
		_ = c.c.SetWithExpiration(ctx, missKey, []byte("1"), metadataMissTTL)
	}
}

// fetch tries the torrent cache, then the magnet-metadata-api. The error wraps
// ErrMetadataNotFound only when every source said it doesn't have the torrent
// (or gave up on it), so a network hiccup isn't remembered as a miss.
func (c *MetadataClient) fetch(ctx context.Context, magnetURI, infoHash string) (*MetadataResponse, error) {
	notFound := true
	var errs []error
	if c.torrentCacheURL != "" {
		m, err := c.fetchFromTorrentCache(ctx, infoHash)
		if err == nil {
			logging.Debug().Str("info_hash", infoHash).Msg("Retrieved metadata from torrent cache")
			return m, nil
		}
		if !errors.Is(err, errNotTorrent) {
			notFound = false
		}
		errs = append(errs, fmt.Errorf("torrent cache: %w", err))
	}
	if c.baseURL != "" {
		m, err := c.fetchFromAPI(ctx, magnetURI, infoHash)
		if err == nil {
			return m, nil
		}
		errs = append(errs, fmt.Errorf("magnet metadata API: %w", err))
	}
	err := errors.Join(errs...)
	if notFound {
		return nil, fmt.Errorf("%w: %w", ErrMetadataNotFound, err)
	}
	return nil, err
}

func (c *MetadataClient) fetchFromAPI(ctx context.Context, magnetURI, infoHash string) (*MetadataResponse, error) {
	reqBody := MetadataRequest{MagnetURI: magnetURI}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/metadata", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	logging.Debug().Str("info_hash", infoHash).Msg("Fetching metadata from MAGNET_METADATA_API")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send POST request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API responded with status: %s", resp.Status)
	}

	var metadata MetadataResponse
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return &metadata, nil
}
