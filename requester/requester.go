package requester

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/fereidani/httpdecompressor"
	"github.com/module195/torrent-indexer/cache"
	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/utils"
)

const (
	cacheKey = "shortLivedCache"
)

// challangeMarkers are the (lowercase) texts of a Cloudflare challenge page.
var challangeMarkers = [][]byte{[]byte("just a moment"), []byte("cf-chl-bypass"), []byte("under attack")}

// coalesceTimeout bounds a shared (coalesced) fetch, which runs detached from
// any single caller's context.
const coalesceTimeout = 2 * time.Minute

// maxDocumentBody bounds a fetched page, after decompression. A scraped
// site, or a host one of its links points to, controls both the declared
// Content-Length and the body: neither may size an allocation unchecked.
const maxDocumentBody = 16 << 20

type Requster struct {
	coalesce                  bool
	sf                        singleflight.Group
	fs                        *FlareSolverr
	c                         *cache.Redis
	httpClient                *http.Client
	shortLivedCacheExpiration time.Duration
	// allowPrivateHosts turns off the destination checks of netguard.go
	allowPrivateHosts atomic.Bool
}

func NewRequester(fs *FlareSolverr, c *cache.Redis, timeout time.Duration) *Requster {
	r := &Requster{
		fs:                        fs,
		c:                         c,
		shortLivedCacheExpiration: 30 * time.Minute,
		// Phase 2 (opt-in): concurrent requests for the same URL share one fetch
		coalesce: os.Getenv("REQUEST_COALESCE") == "true",
	}
	if os.Getenv("REQUESTER_ALLOW_PRIVATE_HOSTS") == "true" {
		r.AllowPrivateHosts()
	}
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   r.dialControl,
	}
	r.httpClient = &http.Client{
		Timeout:       timeout,
		CheckRedirect: checkRedirect,
		Transport: &http.Transport{
			// no Proxy: dialControl must see the address of the site itself
			DialContext:         dialer.DialContext,
			DisableCompression:  false,
			MaxIdleConns:        100,              // Increase connection pool
			MaxIdleConnsPerHost: 10,               // More connections per host
			IdleConnTimeout:     90 * time.Second, // Keep connections alive longer
			DisableKeepAlives:   false,            // Enable keep-alive
			ForceAttemptHTTP2:   true,             // Use HTTP/2 when possible
		},
	}
	return r
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

	ch := i.sf.DoChan(key, func() (_ interface{}, err error) {
		// the shared fetch runs outside the handler goroutine, where a
		// panic would end the whole process instead of one request
		defer utils.RecoverAsError(&err, "coalesced fetch")
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

	// the URL may come from a scraped link: refuse internal destinations
	// before either the plain client or FlareSolverr goes there
	if err := i.checkDestination(ctx, url); err != nil {
		return nil, err
	}

	// try request with plain client
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for url %s: %w", url, err)
	}

	// Add browser-like headers to spoof a real browser
	spoofBrowserHeaders(req, ref)
	// a clearance the solver got for this host lets the plain client past
	// the challenge, instead of a full solve for each page of the site
	cl, withClearance := i.fs.clearanceFor(url)
	if withClearance {
		cl.apply(req)
	}

	resp, err := i.httpClient.Do(req)
	if err != nil && errors.Is(err, ErrBlockedDestination) {
		// a redirect into the internal network: FlareSolverr would follow
		// the same redirect, so there is no fallback
		return nil, fmt.Errorf("failed to do request for url %s: %w", url, err)
	}
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

	// Pre-allocate buffer based on Content-Length if available. The value
	// comes from the remote server, so it is checked against the cap first.
	var buf bytes.Buffer
	if resp != nil && resp.ContentLength > maxDocumentBody {
		return nil, fmt.Errorf("response too large for url %s: %d bytes", url, resp.ContentLength)
	}
	if resp != nil && resp.ContentLength > 0 {
		buf.Grow(int(resp.ContentLength))
	} else {
		buf.Grow(32 * 1024) // Default 32KB pre-allocation
	}

	// Use io.Copy instead of io.ReadAll for better performance; the limit
	// also covers gzip/br bodies that expand well past their wire size
	n, err := io.Copy(&buf, io.LimitReader(body, maxDocumentBody+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read response body for url %s: %w", url, err)
	}
	if n > maxDocumentBody {
		return nil, fmt.Errorf("response too large for url %s: more than %d bytes", url, maxDocumentBody)
	}
	bodyByte = buf.Bytes()
	servedByPlainClient := resp != nil
	challanged := hasChallange(bodyByte)
	if challanged {
		if withClearance && resp != nil {
			logging.Debug().Str("url", url).Msg("Solver clearance rejected by the site")
			i.fs.clearanceRejected(url)
		}
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
		challanged = hasChallange(bodyByte)
	} else if withClearance && resp != nil {
		logging.Debug().Str("url", url).Msg("Request served from plain client with solver clearance")
	} else {
		logging.Debug().Str("url", url).Msg("Request served from plain client")
	}

	// a 5xx, 403 or 429 page is the site failing or blocking us, not a page
	// without results: report it, so the handler answers with an error and
	// the source health in /sources?v=2 sees the failure
	if servedByPlainClient && isUpstreamFailure(resp.StatusCode) {
		return nil, &UpstreamStatusError{URL: url, StatusCode: resp.StatusCode}
	}

	// don't cache other error pages (404) served by the plain client; they
	// are still returned, since some sites answer an empty search with 404
	cacheable := !servedByPlainClient || (resp.StatusCode >= 200 && resp.StatusCode < 300)

	// save response to cache if it's not a challange, body is not empty and is valid HTML
	if !challanged && len(bodyByte) > 0 && utils.IsValidHTML(string(bodyByte)) {
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

// UpstreamStatusError is returned when the site answered with a status that
// means it failed or blocked the request (5xx, 403, 429).
type UpstreamStatusError struct {
	URL        string
	StatusCode int
}

func (e *UpstreamStatusError) Error() string {
	return fmt.Sprintf("upstream returned status %d for url %s", e.StatusCode, e.URL)
}

func isUpstreamFailure(code int) bool {
	return code >= 500 || code == http.StatusForbidden || code == http.StatusTooManyRequests
}

func (i *Requster) ExpireDocument(ctx context.Context, url string) error {
	key := cache.Key(cache.KindDocShort, url, fmt.Sprintf("%s:%s", cacheKey, url))
	return i.c.Del(ctx, key)
}

// hasChallange checks if the body contains a challange marker, ignoring case.
// A plain search on a lowercased copy is ~17x faster than a (?i) regex on
// pages of 50-170 KB.
func hasChallange(body []byte) bool {
	lower := bytes.ToLower(body)
	for _, m := range challangeMarkers {
		if bytes.Contains(lower, m) {
			return true
		}
	}
	return false
}
