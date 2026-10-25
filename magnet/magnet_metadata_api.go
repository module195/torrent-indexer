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
	c          Cache

	torrentCacheURL  string
	torrentCacheHTTP *http.Client
	torrentCacheSem  chan struct{}

	breakerMu       sync.Mutex
	cacheFailures   int
	cachePauseUntil time.Time

	mu       sync.Mutex
	inflight map[string]*lookup
}

// Cache is the part of cache.Redis the client uses.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	SetWithExpiration(ctx context.Context, key string, value []byte, expiration time.Duration) error
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
	// After torrentCacheMaxFailures failures in a row (timeouts, rate
	// limits, network errors) the torrent cache is skipped for
	// torrentCachePause, so an unreachable cache doesn't pile up lookups.
	torrentCacheMaxFailures = 5
	torrentCachePause       = 10 * time.Minute
)

// ErrMetadataNotFound means no source had the torrent, now or recently.
var ErrMetadataNotFound = errors.New("torrent metadata not found")

// NewClient returns a client for the magnet-metadata-api at baseURL (empty
// disables it). The public torrent cache is off until WithTorrentCache.
func NewClient(baseURL string, timeout time.Duration, c *cache.Redis) *MetadataClient {
	mc := &MetadataClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:      100,
				IdleConnTimeout:   30 * time.Second,
				ForceAttemptHTTP2: true,
			},
		},
		inflight: map[string]*lookup{},
	}
	if c != nil {
		mc.c = c
	}
	return mc
}

// WithCache replaces the Redis cache, for tests.
func (c *MetadataClient) WithCache(kv Cache) *MetadataClient {
	c.c = kv
	return c
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
	k, err := c.keys(magnetURI)
	if err != nil {
		return nil, err
	}
	if m, known, err := c.cached(ctx, k); known {
		return m, err
	}
	l := c.start(ctx, magnetURI, k)
	select {
	case <-l.done:
		return l.m, l.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// CachedMetadata returns the metadata for magnetURI only if it is already in
// Redis. ok is false when nothing is known yet; Prefetch then looks it up.
func (c *MetadataClient) CachedMetadata(ctx context.Context, magnetURI string) (m *MetadataResponse, ok bool) {
	k, err := c.keys(magnetURI)
	if err != nil {
		return nil, false
	}
	m, known, err := c.cached(ctx, k)
	return m, known && err == nil
}

// Prefetch looks magnetURI up in the background, unless it is cached or
// already being looked up, so a later search finds it in Redis. It never
// blocks the caller.
func (c *MetadataClient) Prefetch(ctx context.Context, magnetURI string) {
	k, err := c.keys(magnetURI)
	if err != nil {
		return
	}
	if _, known, _ := c.cached(ctx, k); known {
		return
	}
	c.start(ctx, magnetURI, k)
}

// HasAPI reports whether the magnet-metadata-api is configured.
func (c *MetadataClient) HasAPI() bool {
	return c != nil && c.baseURL != ""
}

type lookupKeys struct {
	infoHash, cacheKey, missKey string
}

func (c *MetadataClient) keys(magnetURI string) (lookupKeys, error) {
	if !c.IsEnabled() {
		return lookupKeys{}, fmt.Errorf("magnet metadata API is not enabled")
	}
	m, err := ParseMagnetUri(magnetURI)
	if err != nil {
		return lookupKeys{}, fmt.Errorf("failed to parse magnet URI: %w", err)
	}
	h := m.InfoHash.HexString()
	return lookupKeys{infoHash: h, cacheKey: "metadata:" + h, missKey: "metadata:miss:" + h}, nil
}

// cached reports what Redis knows: the metadata, or ErrMetadataNotFound for a
// recent miss. known is false when it knows nothing.
func (c *MetadataClient) cached(ctx context.Context, k lookupKeys) (m *MetadataResponse, known bool, err error) {
	if c.c == nil {
		return nil, false, nil
	}
	if cachedData, err := c.c.Get(ctx, k.cacheKey); err == nil && cachedData != nil {
		var cachedMetadata MetadataResponse
		if err := json.Unmarshal(cachedData, &cachedMetadata); err == nil {
			return &cachedMetadata, true, nil
		}
	}
	if miss, err := c.c.Get(ctx, k.missKey); err == nil && miss != nil {
		return nil, true, ErrMetadataNotFound
	}
	return nil, false, nil
}

// start joins the running lookup for the info hash or starts one, detached
// from ctx's cancellation.
func (c *MetadataClient) start(ctx context.Context, magnetURI string, k lookupKeys) *lookup {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.inflight[k.infoHash]
	if !ok {
		l = &lookup{done: make(chan struct{})}
		c.inflight[k.infoHash] = l
		go c.run(context.WithoutCancel(ctx), l, magnetURI, k.infoHash, k.cacheKey, k.missKey)
	}
	return l
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
		m, err := c.fetchFromTorrentCacheGuarded(ctx, infoHash)
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

var errTorrentCachePaused = errors.New("torrent cache paused after repeated failures")

// fetchFromTorrentCacheGuarded skips the torrent cache while it is paused and
// pauses it after torrentCacheMaxFailures failures in a row. A "not there"
// answer counts as the cache working.
func (c *MetadataClient) fetchFromTorrentCacheGuarded(ctx context.Context, infoHash string) (*MetadataResponse, error) {
	c.breakerMu.Lock()
	paused := time.Now().Before(c.cachePauseUntil)
	c.breakerMu.Unlock()
	if paused {
		return nil, errTorrentCachePaused
	}

	m, err := c.fetchFromTorrentCache(ctx, infoHash)
	failed := err != nil && !errors.Is(err, errNotTorrent) && ctx.Err() == nil

	c.breakerMu.Lock()
	defer c.breakerMu.Unlock()
	if !failed {
		c.cacheFailures = 0
		return m, err
	}
	c.cacheFailures++
	if c.cacheFailures >= torrentCacheMaxFailures && !time.Now().Before(c.cachePauseUntil) {
		c.cachePauseUntil = time.Now().Add(torrentCachePause)
		c.cacheFailures = 0
		logging.Warn().Err(err).Str("url", c.torrentCacheURL).Dur("pause", torrentCachePause).
			Msg("Torrent cache keeps failing; pausing it")
	}
	return m, err
}
