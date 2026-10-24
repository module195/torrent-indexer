package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/module195/torrent-indexer/cache"
	"github.com/module195/torrent-indexer/logging"
)

// responseCacheFetchTimeout bounds a shared handler run, which is detached
// from the context of the request that started it.
const responseCacheFetchTimeout = 2 * time.Minute

// ResponseCache coalesces identical concurrent indexer requests and caches
// their final JSON for a short TTL (Phase 2, opt-in via RESPONSE_CACHE_TTL).
//
// The key is the indexer label plus the full, canonicalized query string, so a
// cached body is only ever replayed for a request with exactly the same
// parameters: the output is byte-for-byte what the handler would produce.
type ResponseCache struct {
	redis   *cache.Redis
	indexer *Indexer
	ttl     time.Duration
	sf      singleflight.Group
}

func NewResponseCache(i *Indexer, ttl time.Duration) *ResponseCache {
	return &ResponseCache{redis: i.redis, indexer: i, ttl: ttl}
}

type capturedResponse struct {
	status      int
	contentType string
	body        []byte
}

// bufferedWriter is a minimal http.ResponseWriter that records the response.
type bufferedWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (b *bufferedWriter) Header() http.Header { return b.header }
func (b *bufferedWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}
func (b *bufferedWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.buf.Write(p)
}

// WithXCacheBypass marks responses that were not served through the response
// cache with "X-Cache: BYPASS", so every indexer route sends X-Cache.
func WithXCacheBypass(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Cache", "BYPASS")
		next(w, r)
	}
}

// Wrap returns next with response caching/coalescing for the given indexer.
// A nil receiver (feature disabled) only adds "X-Cache: BYPASS"; the body is
// whatever next writes.
func (rc *ResponseCache) Wrap(label string, next http.HandlerFunc) http.HandlerFunc {
	if rc == nil {
		return WithXCacheBypass(next)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			WithXCacheBypass(next)(w, r)
			return
		}
		key := cache.Key(cache.KindResp, label+":"+r.URL.Query().Encode(), "")

		if body, err := rc.redis.Get(r.Context(), key); err == nil && len(body) > 0 {
			rc.indexer.metrics.CacheHits.WithLabelValues("indexer_response").Inc()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			_, _ = w.Write(body)
			return
		}
		rc.indexer.metrics.CacheMisses.WithLabelValues("indexer_response").Inc()

		ch := rc.sf.DoChan(key, func() (interface{}, error) {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), responseCacheFetchTimeout)
			defer cancel()
			bw := &bufferedWriter{header: http.Header{}}
			next(bw, r.Clone(ctx))
			if bw.status == 0 {
				bw.status = http.StatusOK
			}
			res := capturedResponse{status: bw.status, contentType: bw.header.Get("Content-Type"), body: bw.buf.Bytes()}
			if res.status == http.StatusOK && isNonEmptyResult(res.body) {
				if err := rc.redis.SetWithExpiration(ctx, key, res.body, rc.ttl); err != nil {
					logging.Error().Err(err).Str("indexer", label).Msg("Failed to cache indexer response")
				}
			}
			return res, nil
		})

		select {
		case out := <-ch:
			res := out.Val.(capturedResponse)
			if res.contentType != "" {
				w.Header().Set("Content-Type", res.contentType)
			}
			if out.Shared {
				w.Header().Set("X-Cache", "SHARED")
			} else {
				w.Header().Set("X-Cache", "MISS")
			}
			w.WriteHeader(res.status)
			_, _ = w.Write(res.body)
		case <-r.Context().Done():
			// client went away; the shared run keeps going for other waiters
		}
	}
}

// isNonEmptyResult reports whether body is an indexer JSON response with at
// least one result. Empty results are not cached so a transient site failure
// doesn't stick for the whole TTL.
func isNonEmptyResult(body []byte) bool {
	var probe struct {
		Count int `json:"count"`
	}
	return json.Unmarshal(body, &probe) == nil && probe.Count > 0
}
