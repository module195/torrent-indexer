package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/module195/torrent-indexer/cache"
	"github.com/module195/torrent-indexer/logging"
	"github.com/xhit/go-str2duration/v2"
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

// ttlEnvName is the per-indexer override of RESPONSE_CACHE_TTL, named like
// INDEXER_<NAME>_URL: "betor" -> RESPONSE_CACHE_TTL_BETOR,
// "torrent-dos-filmes" -> RESPONSE_CACHE_TTL_TORRENT_DOS_FILMES.
func ttlEnvName(label string) string {
	return "RESPONSE_CACHE_TTL_" + strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, label)
}

// ttlFor returns the cache TTL for one indexer: its RESPONSE_CACHE_TTL_<NAME>
// when set (0 turns the cache off for it), otherwise the global TTL.
func (rc *ResponseCache) ttlFor(label string) time.Duration {
	v := os.Getenv(ttlEnvName(label))
	if v == "" {
		return rc.ttl
	}
	if v == "0" {
		return 0
	}
	ttl, err := str2duration.ParseDuration(v)
	if err != nil || ttl < 0 {
		logging.Error().Err(err).Str("indexer", label).Str("value", v).Msg("Invalid per-indexer response cache TTL; using RESPONSE_CACHE_TTL")
		return rc.ttl
	}
	return ttl
}

// noCacheParam asks for a fresh run that skips the cached response (the new
// result still replaces it). It is not part of the cache key and is removed
// before the handler runs. "Cache-Control: no-cache" does the same.
const noCacheParam = "nocache"

// wantsFresh reports whether r asked to skip the cached response, and strips
// the nocache parameter from r so the key and the handler never see it.
func wantsFresh(r *http.Request) bool {
	q := r.URL.Query()
	fresh := strings.Contains(strings.ToLower(r.Header.Get("Cache-Control")), "no-cache")
	if vals, ok := q[noCacheParam]; ok {
		v := strings.ToLower(strings.TrimSpace(vals[0]))
		fresh = fresh || v == "" || v == "1" || v == "true"
		q.Del(noCacheParam)
		r.URL.RawQuery = q.Encode()
		r.RequestURI = r.URL.RequestURI()
	}
	return fresh
}

// Wrap returns next with response caching/coalescing for the given indexer.
// A nil receiver (feature disabled), or a per-indexer TTL of 0, only adds
// "X-Cache: BYPASS"; the body is whatever next writes.
func (rc *ResponseCache) Wrap(label string, next http.HandlerFunc) http.HandlerFunc {
	if rc == nil {
		return WithXCacheBypass(next)
	}
	ttl := rc.ttlFor(label)
	if ttl == 0 {
		logging.Info().Str("indexer", label).Msg("Response cache disabled for indexer")
		return WithXCacheBypass(next)
	}
	if ttl != rc.ttl {
		logging.Info().Str("indexer", label).Dur("ttl", ttl).Msg("Per-indexer response cache TTL")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			WithXCacheBypass(next)(w, r)
			return
		}
		fresh := wantsFresh(r)
		key := cache.Key(cache.KindResp, label+":"+r.URL.Query().Encode(), "")

		if fresh {
			rc.indexer.metrics.CacheMisses.WithLabelValues("indexer_response_bypass").Inc()
		} else if body, err := rc.redis.Get(r.Context(), key); err == nil && len(body) > 0 {
			rc.indexer.metrics.CacheHits.WithLabelValues("indexer_response").Inc()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			_, _ = w.Write(body)
			return
		}
		if !fresh {
			rc.indexer.metrics.CacheMisses.WithLabelValues("indexer_response").Inc()
		}

		ch := rc.sf.DoChan(key, func() (interface{}, error) {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), responseCacheFetchTimeout)
			defer cancel()
			bw := &bufferedWriter{header: http.Header{}}
			next(bw, r.Clone(ctx))
			if bw.status == 0 {
				bw.status = http.StatusOK
			}
			res := capturedResponse{status: bw.status, contentType: bw.header.Get("Content-Type"), body: bw.buf.Bytes()}
			if ttl, ok := responseCacheTTL(res, ttl); ok {
				if err := rc.redis.SetWithExpiration(ctx, key, res.body, ttl); err != nil {
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

// emptyResultTTL caps how long a response with no results is cached.
const emptyResultTTL = 2 * time.Minute

// responseCacheTTL says whether res may be cached and for how long. Responses
// with results get the full TTL. With no results, a page that listed torrents
// (indexed_count > 0) was real and everything was filtered out, so it gets a
// short TTL; a page that listed nothing may be a challenge or a transient
// site failure and is not cached at all.
func responseCacheTTL(res capturedResponse, full time.Duration) (time.Duration, bool) {
	if res.status != http.StatusOK {
		return 0, false
	}
	var probe struct {
		Count        int `json:"count"`
		IndexedCount int `json:"indexed_count"`
	}
	if json.Unmarshal(res.body, &probe) != nil {
		return 0, false
	}
	if probe.Count > 0 {
		return full, true
	}
	if probe.IndexedCount > 0 {
		if full < emptyResultTTL {
			return full, true
		}
		return emptyResultTTL, true
	}
	return 0, false
}
