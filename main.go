package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	handler "github.com/module195/torrent-indexer/api"
	"github.com/module195/torrent-indexer/cache"
	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/magnet"
	"github.com/module195/torrent-indexer/monitoring"
	"github.com/module195/torrent-indexer/public"
	"github.com/module195/torrent-indexer/requester"
	meilisearch "github.com/module195/torrent-indexer/search"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	str2duration "github.com/xhit/go-str2duration/v2"
)

// imdbQueryRE matches IMDB IDs (tt followed by 7-8 digits)
var imdbQueryRE = regexp.MustCompile(`^tt\d{7,8}$`)

// defaultBetorDumpURL is used when BETOR_DUMP=true and BETOR_DUMP_URL is empty.
const defaultBetorDumpURL = "https://catalogo-data.betor.top/items.json"

// indexerSupportsIMDb reports whether path is /indexers/<name> of an indexer
// whose capability accepts IMDb IDs.
func indexerSupportsIMDb(path string) bool {
	name, ok := strings.CutPrefix(path, "/indexers/")
	if !ok {
		return false
	}
	name = strings.TrimSuffix(name, "/")
	for _, c := range handler.ActiveIndexers {
		if c.Name == name {
			return c.SupportsIMDb
		}
	}
	return false
}

// skipIMDBQueryMiddleware returns empty results for IMDB ID queries.
// Brazilian torrent sites don't support IMDB search, so we skip these
// queries to avoid unnecessary FlareSolverr requests.
// Exceptions: the indexers whose capability says they support IMDb (betor,
// bludv_ru), and /search, which looks the ID up in the imdb field of the
// Meilisearch index, so we let those requests through.
func skipIMDBQueryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Libera os indexers que aceitam IMDb (veja handler.ActiveIndexers), o
		// /indexers/multi, que aplica as mesmas regras por indexer, e o
		// /search, que busca o ID no campo imdb do Meilisearch
		if indexerSupportsIMDb(r.URL.Path) || r.URL.Path == "/indexers/multi" || r.URL.Path == "/search" {
			next.ServeHTTP(w, r)
			return
		}

		q := r.URL.Query().Get("q")
		if imdbQueryRE.MatchString(q) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasPrefix(r.URL.Path, "/indexers/") {
				w.Header().Set("X-Cache", "BYPASS")
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"results": []interface{}{},
				"count":   0,
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// newHTTPServer builds a server with header/idle timeouts so slow or idle
// clients can't hold connections forever. No WriteTimeout: indexer requests
// may legitimately take long (FlareSolverr).
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func main() {
	// Initialize logging first
	logging.InitLogger()

	redis := cache.NewRedis()
	searchIndex := meilisearch.NewSearchIndexer(os.Getenv("MEILISEARCH_ADDRESS"), os.Getenv("MEILISEARCH_KEY"), "torrents")
	var magnetMetadataAPI *magnet.MetadataClient
	if os.Getenv("MAGNET_METADATA_API_ENABLED") == "true" {
		timeout := 10 * time.Second
		if v := os.Getenv("MAGNET_METADATA_API_TIMEOUT_SECONDS"); v != "" {
			if t, err := strconv.Atoi(v); err == nil {
				timeout = time.Duration(t) * time.Second
			}
		}
		magnetMetadataAPI = magnet.NewClient(os.Getenv("MAGNET_METADATA_API_ADDRESS"), timeout, redis)
	}
	metrics := monitoring.NewMetrics()
	metrics.Register()

	timeoutFlaresolverrMilli := 30000
	if v := os.Getenv("FLARESOLVERR_TIMEOUT_SECONDS"); v != "" {
		if t, err := strconv.Atoi(v); err == nil {
			timeoutFlaresolverrMilli = t * 1000
		}
	}

	flaresolverrPoolSize := 5
	if v := os.Getenv("FLARESOLVERR_POOL_SIZE"); v != "" {
		if t, err := strconv.Atoi(v); err == nil {
			flaresolverrPoolSize = t
		}
	}

	flaresolverr := requester.NewFlareSolverr(os.Getenv("FLARESOLVERR_ADDRESS"), timeoutFlaresolverrMilli, flaresolverrPoolSize)
	prometheus.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "flaresolverr_sessions_available",
		Help: "Idle FlareSolverr sessions in the pool",
	}, func() float64 { return float64(flaresolverr.SessionsAvailable()) }))
	if v := os.Getenv("FLARESOLVERR_HEALTHCHECK_SECONDS"); v != "" {
		if t, err := strconv.Atoi(v); err == nil && t > 0 {
			logging.Info().Int("interval_seconds", t).Msg("Enabling FlareSolverr health check")
			flaresolverr.StartHealthCheck(time.Duration(t) * time.Second)
		}
	}

	timeoutRequester := 5000 * time.Millisecond
	if v := os.Getenv("REQUEST_TIMEOUT_MILLISECONDS"); v != "" {
		if t, err := strconv.Atoi(v); err == nil {
			timeoutRequester = time.Duration(t) * time.Millisecond
		}
	}
	req := requester.NewRequester(flaresolverr, redis, timeoutRequester)

	// get shot-lived and long-lived cache expiration from env
	shortLivedCacheExpiration, err := str2duration.ParseDuration(os.Getenv("SHORT_LIVED_CACHE_EXPIRATION"))
	if err == nil {
		logging.Info().Dur("expiration", shortLivedCacheExpiration).Msg("Setting short-lived cache expiration")
		req.SetShortLivedCacheExpiration(shortLivedCacheExpiration)
	}
	if v := os.Getenv("LONG_LIVED_CACHE_EXPIRATION"); v != "" {
		longLivedCacheExpiration, err := str2duration.ParseDuration(v)
		if err == nil {
			logging.Info().Dur("expiration", longLivedCacheExpiration).Msg("Setting long-lived cache expiration")
			redis.SetDefaultExpiration(longLivedCacheExpiration)
		} else {
			logging.Error().Err(err).Msg("Failed to parse long-lived cache expiration")
		}
	}

	icfg := handler.IndexersConfig{
		FallbackTitleEnabled: os.Getenv("FALLBACK_TITLE_ENABLED") == "true",
	}

	// Optional: load the betor catalog dump into Meilisearch on an interval.
	// BETOR_DUMP=false turns it off even with BETOR_DUMP_URL set; BETOR_DUMP=true
	// turns it on, using the default dump URL when BETOR_DUMP_URL is empty.
	dumpURL := os.Getenv("BETOR_DUMP_URL")
	dumpEnabled := dumpURL != ""
	if v := os.Getenv("BETOR_DUMP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			dumpEnabled = b
		} else {
			logging.Error().Str("value", v).Msg("Invalid BETOR_DUMP; expected true or false")
		}
	}
	if dumpEnabled && dumpURL == "" {
		dumpURL = defaultBetorDumpURL
	}
	if dumpEnabled && searchIndex.BaseURL == "" {
		logging.Warn().Msg("Betor dump sync needs MEILISEARCH_ADDRESS; not starting it")
	} else if !dumpEnabled && dumpURL != "" {
		logging.Info().Msg("Betor dump sync disabled by BETOR_DUMP=false")
	}
	if dumpEnabled && searchIndex.BaseURL != "" {
		interval := 8 * time.Hour
		if v := os.Getenv("BETOR_DUMP_INTERVAL"); v != "" {
			if d, err := str2duration.ParseDuration(v); err == nil && d >= time.Minute {
				interval = d
			} else {
				logging.Error().Str("value", v).Msg("Invalid BETOR_DUMP_INTERVAL; using 8h")
			}
		}
		logging.Info().Str("url", dumpURL).Dur("interval", interval).Msg("Enabling betor dump sync")
		(&handler.BetorDumpSync{URL: dumpURL, Interval: interval, Search: searchIndex, Redis: redis}).Start(context.Background())
	}

	indexers := handler.NewIndexers(icfg, redis, metrics, req, searchIndex, magnetMetadataAPI)
	search := handler.NewMeilisearchHandler(searchIndex)

	// Phase 2 (opt-in): short-lived response cache + coalescing per indexer
	var respCache *handler.ResponseCache
	if v := os.Getenv("RESPONSE_CACHE_TTL"); v != "" {
		ttl, err := str2duration.ParseDuration(v)
		if err == nil && ttl > 0 {
			logging.Info().Dur("ttl", ttl).Msg("Enabling indexer response cache")
			respCache = handler.NewResponseCache(indexers, ttl)
		} else {
			logging.Error().Err(err).Str("value", v).Msg("Invalid RESPONSE_CACHE_TTL; response cache disabled")
		}
	}

	indexerMux := http.NewServeMux()
	metricsMux := http.NewServeMux()

	// Phase 3: per-indexer health/latency for /sources?v=2 (in-memory only)
	stats := handler.NewIndexerStats()
	indexerRoutes := map[string]http.HandlerFunc{
		"baixarapido": respCache.Wrap("baixarapido", stats.Track("baixarapido", indexers.HandlerBaixarApidoIndexer)),
		//"via_filmes": indexers.HandlerViaFilmesIndexer,
		"comando_torrents":   respCache.Wrap("comando_torrents", stats.Track("comando_torrents", indexers.HandlerComandoIndexer)),
		"rede_torrent":       respCache.Wrap("rede_torrent", stats.Track("rede_torrent", indexers.HandlerRedeTorrentIndexer)),
		"starck-filmes":      respCache.Wrap("starck-filmes", stats.Track("starck-filmes", indexers.HandlerStarckFilmesIndexer)),
		"torrent-dos-filmes": respCache.Wrap("torrent-dos-filmes", stats.Track("torrent-dos-filmes", indexers.HandlerTorrentDosFilmesIndexer)),
		//"bludv": indexers.HandlerBluDVIndexer,
		//"vaca_torrent": indexers.HandlerVacaTorrentIndexer,
		// torrentfilmeshds has its own cache and sets X-Cache itself (BYPASS stays on errors)
		"torrentfilmeshds": handler.WithXCacheBypass(stats.Track("torrentfilmeshds", indexers.HandlerTorrentFilmesHDsIndexer)),
		"bludv_ru":         respCache.Wrap("bludv_ru", stats.Track("bludv_ru", indexers.HandlerBludvRuIndexer)),
		"betor":            respCache.Wrap("betor", stats.Track("betor", indexers.HandlerBetorIndexer)),
	}
	for _, c := range handler.ActiveIndexers {
		if indexerRoutes[c.Name] == nil {
			logging.Fatal().Str("indexer", c.Name).Msg("Active indexer has no route")
		}
	}

	indexerMux.HandleFunc("/", handler.HandlerIndex)
	for name, h := range indexerRoutes {
		indexerMux.HandleFunc("/indexers/"+name, h)
	}
	indexerMux.HandleFunc("/indexers/manual", handler.WithXCacheBypass(indexers.HandlerManualIndexer))
	multi := handler.NewMultiHandler(indexerRoutes)
	multi.WarmCache = respCache != nil
	multi.Stats = stats
	indexerMux.Handle("/indexers/multi", multi)
	indexerMux.Handle("/sources", handler.NewSourcesHandler(stats, searchIndex, handler.Features{
		Multi:         true,
		XCache:        true,
		ResponseCache: respCache != nil,
	}))
	indexerMux.HandleFunc("/search", search.SearchTorrentHandler)
	indexerMux.HandleFunc("/search/health", search.HealthHandler)
	indexerMux.HandleFunc("/search/stats", search.StatsHandler)
	indexerMux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.FS(public.UIFiles))))
	loggedIndexerMux := logging.HTTPLoggingMiddleware(indexerMux)
	loggedIndexerMux = skipIMDBQueryMiddleware(loggedIndexerMux)

	metricsMux.Handle("/metrics", promhttp.Handler())

	metricsPort := os.Getenv("METRICS_PORT")
	if metricsPort == "" {
		metricsPort = "8081"
	}

	go func() {
		err := newHTTPServer(":"+metricsPort, metricsMux).ListenAndServe()
		if err != nil {
			panic(err)
		}
	}()

	port := os.Getenv("PORT")
	if port == "" {
		port = "7006"
	}

	logging.Info().Str("port", port).Msg("Server listening")
	err = newHTTPServer(":"+port, loggedIndexerMux).ListenAndServe()
	if err != nil {
		logging.Fatal().Err(err).Msg("Server failed to start")
	}
}
