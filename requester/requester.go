package requester

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/module195/torrent-indexer/cache"
	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/utils"
	"github.com/fereidani/httpdecompressor"
)

const (
	cacheKey = "shortLivedCache"
)

var challangeRegex = regexp.MustCompile(`(?i)(just a moment|cf-chl-bypass|under attack)`)

// coalesceTimeout bounds a shared (coalesced) fetch, which runs detached from
// any single caller's context.
const coalesceTimeout = 2 * time.Minute

type Requster struct {
	coalesce                  bool
	sf                        singleflight.Group
	fs                        *FlareSolverr
	c                         *cache.Redis
	httpClient                *http.Client
	shortLivedCacheExpiration time.Duration
}

func NewRequester(fs *FlareSolverr, c *cache.Redis, timeout time.Duration) *Requster {
	httpClient := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableCompression:  false,
			MaxIdleConns:        100,              // Increase connection pool
			MaxIdleConnsPerHost: 10,               // More connections per host
			IdleConnTimeout:     90 * time.Second, // Keep connections alive longer
			DisableKeepAlives:   false,            // Enable keep-alive
			ForceAttemptHTTP2:   true,             // Use HTTP/2 when possible
		},
	}

	return &Requster{
		fs:                        fs,
		httpClient:                httpClient,
		c:                         c,
		shortLivedCacheExpiration: 30 * time.Minute,
		// Phase 2 (opt-in): concurrent requests for the same URL share one fetch
		coalesce: os.Getenv("REQUEST_COALESCE") == "true",
	}
}

func (i *Requster) SetShortLivedCacheExpiration(expiration time.Duration) {
	i.shortLivedCacheExpiration = expiration
}

func (i *Requster) GetDocument(ctx context.Context, url string, referer ...string) (io.ReadCloser, error) {
	var body io.ReadCloser

	// Extract referer if provided
	ref := ""
	if len(referer) > 0 {
		ref = referer[0]
	}

	// try request from short-lived cache
	key := cache.Key(cache.KindDocShort, url, fmt.Sprintf("%s:%s", cacheKey, url))
	bodyByte, err := i.c.Get(ctx, key)
	if err == nil {
		logging.Debug().Str("url", url).Msg("Returning from short-lived cache")
		body = io.NopCloser(bytes.NewReader(bodyByte))
		return body, nil
	}

	if !i.coalesce {
		return i.fetchDocument(ctx, url, ref, key)
	}

	ch := i.sf.DoChan(key, func() (interface{}, error) {
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coalesceTimeout)
		defer cancel()
		rc, err := i.fetchDocument(fetchCtx, url, ref, key)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		if res.Shared {
			logging.Debug().Str("url", url).Msg("Request coalesced with an in-flight fetch")
		}
		// each caller gets its own reader over the shared bytes
		return io.NopCloser(bytes.NewReader(res.Val.([]byte))), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fetchDocument fetches url with the plain client, falling back to
// FlareSolverr, and stores valid responses in the short-lived cache.
func (i *Requster) fetchDocument(ctx context.Context, url, ref, key string) (io.ReadCloser, error) {
	var body io.ReadCloser
	var bodyByte []byte

	// try request with plain client
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for url %s: %w", url, err)
	}

	// Add browser-like headers to spoof a real browser
	spoofBrowserHeaders(req, ref)

	resp, err := i.httpClient.Do(req)
	if err != nil {
		// try request with flare solverr
		body, err = i.fs.Get(ctx, url, 3)
		if err != nil {
			return nil, fmt.Errorf("failed to do request for url %s: %w", url, err)
		}
	} else {
		defer resp.Body.Close()

		// Decompress response using httpdecompressor
		body, err = httpdecompressor.Reader(resp)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress response: %w", err)
		}
		defer body.Close()
	}

	// Pre-allocate buffer based on Content-Length if available
	var buf bytes.Buffer
	if resp != nil && resp.ContentLength > 0 {
		buf.Grow(int(resp.ContentLength))
	} else {
		buf.Grow(32 * 1024) // Default 32KB pre-allocation
	}

	// Use io.Copy instead of io.ReadAll for better performance
	_, err = io.Copy(&buf, body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body for url %s: %w", url, err)
	}
	bodyByte = buf.Bytes()
	servedByPlainClient := resp != nil
	if hasChallange(bodyByte) {
		servedByPlainClient = false
		// try request with flare solverr
		body, err = i.fs.Get(ctx, url, 3)
		if err != nil {
			return nil, fmt.Errorf("failed to do request for url %s: %w", url, err)
		}
		bodyByte, err = io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response body: %w", err)
		}
		logging.Debug().Str("url", url).Msg("Request served from flaresolverr")
	} else {
		logging.Debug().Str("url", url).Msg("Request served from plain client")
	}

	// don't cache error pages (404/5xx) served by the plain client; they are
	// still returned so callers behave exactly as before
	cacheable := !servedByPlainClient || (resp.StatusCode >= 200 && resp.StatusCode < 300)

	// save response to cache if it's not a challange, body is not empty and is valid HTML
	if !hasChallange(bodyByte) && len(bodyByte) > 0 && utils.IsValidHTML(string(bodyByte)) {
		if cacheable {
			err = i.c.SetWithExpiration(ctx, key, bodyByte, i.shortLivedCacheExpiration)
			if err != nil {
				logging.Error().Err(err).Str("url", url).Msg("Failed to save response to cache")
			} else {
				logging.Debug().Str("url", url).Msg("Saved to cache")
			}
		} else {
			logging.Debug().Str("url", url).Int("status", resp.StatusCode).Msg("Not caching non-2xx response")
		}
	} else {
		return nil, fmt.Errorf("response is a challange")
	}

	return io.NopCloser(bytes.NewReader(bodyByte)), nil
}

func (i *Requster) ExpireDocument(ctx context.Context, url string) error {
	key := cache.Key(cache.KindDocShort, url, fmt.Sprintf("%s:%s", cacheKey, url))
	return i.c.Del(ctx, key)
}

// hasChallange checks if the body contains a challange by regex matching
func hasChallange(body []byte) bool {
	return challangeRegex.Match(body)
}
