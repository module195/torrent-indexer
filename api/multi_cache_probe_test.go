package handler

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/module195/torrent-indexer/cache"
)

// The multi's cache probe must find torrentfilmeshds' own result cache: its
// route has no respCache.Wrap, so ResponseCache.Cached alone never did, and
// the cached secondary stayed in the background. Skipped where no Redis runs.
func TestMultiCacheProbeFindsTorrentFilmesHDsCache(t *testing.T) {
	if !redisReachable() {
		t.Skip("no Redis")
	}
	r := cache.NewRedis()
	i := &Indexer{redis: r, metrics: testMetricsOnce()}
	ctx := context.Background()
	q := "probe-" + time.Now().Format("150405.000000")
	if err := r.SetWithExpiration(ctx, tfhdsResultKey(q, "1"), []byte(`{"results":[]}`), time.Minute); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Del(context.Background(), tfhdsResultKey(q, "1")) })

	probe := MultiCacheProbe(nil, i)
	cases := []struct {
		indexer string
		query   url.Values
		want    bool
	}{
		{"torrentfilmeshds", url.Values{"q": {q}, "sortBy": {"seeders"}}, true},
		{"torrentfilmeshds", url.Values{"q": {q}, "page": {"1"}}, true},
		{"torrentfilmeshds", url.Values{"q": {q}, "page": {"2"}}, false},
		{"torrentfilmeshds", url.Values{"q": {q + "-outro"}}, false},
		// response cache off (nil): the Wrap'd indexers are never cached
		{"rede_torrent", url.Values{"q": {q}}, false},
	}
	for _, c := range cases {
		if got := probe(ctx, c.indexer, c.query); got != c.want {
			t.Errorf("probe(%s, %s) = %v, want %v", c.indexer, c.query.Encode(), got, c.want)
		}
	}
}
