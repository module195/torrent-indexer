package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/module195/torrent-indexer/cache"
	"github.com/module195/torrent-indexer/consts"
	"github.com/module195/torrent-indexer/magnet"
	"github.com/module195/torrent-indexer/monitoring"
	"github.com/module195/torrent-indexer/requester"
	"github.com/module195/torrent-indexer/schema"
	meilisearch "github.com/module195/torrent-indexer/search"
)

type Indexer struct {
	config            IndexersConfig
	redis             *cache.Redis
	metrics           *monitoring.Metrics
	requester         *requester.Requster
	search            *meilisearch.SearchIndexer
	magnetMetadataAPI *magnet.MetadataClient
	postProcessors    []PostProcessorFunc
}

type IndexerMeta struct {
	Label       string // Label is used for Prometheus metrics and logging. Must be alphanumeric optionally with underscores.
	URL         string // URL is the base URL of the indexer, e.g. "https://example.com/"
	SearchURL   string // SearchURL is the base URL for search queries, e.g. "?s="
	PagePattern string // PagePattern for pagination, e.g. "page/%s"
}

type Response struct {
	Results      []schema.IndexedTorrent `json:"results"`
	Count        int                     `json:"count"`
	IndexedCount int                     `json:"indexed_count,omitempty"`
}

type PostProcessorFunc func(*Indexer, *http.Request, []schema.IndexedTorrent) []schema.IndexedTorrent

var GlobalPostProcessors = []PostProcessorFunc{
	AddSimilarityCheck,     // Jaccard similarity
	DedupByInfoHash,        // Drop repeated torrents (same info hash)
	PreferReleaseSize,      // Size stated by the magnet itself (xl=, release name)
	FullfilMissingMetadata, // Fill missing size or title metadata
	CleanupTitleWebsites,   // Remove website names from titles
	FallbackPostTitle,      // Fallback to original title if empty
	AppendAudioTags,        // Add (brazilian, eng, etc.) audio tags to titles
	ApplySorting,           // Sort results based on sortBy and sortDirection params
	SendToSearchIndexer,    // Send indexed torrents to Meilisearch
	FilterBy,               // Filter results based on query params (audio, etc.)
	ApplyLimit,             // Limit number of results based on query param
}

type IndexersConfig struct {
	FallbackTitleEnabled bool
}

func NewIndexers(
	config IndexersConfig,
	redis *cache.Redis,
	metrics *monitoring.Metrics,
	req *requester.Requster,
	si *meilisearch.SearchIndexer,
	mc *magnet.MetadataClient,
) *Indexer {
	return &Indexer{
		config:            config,
		redis:             redis,
		metrics:           metrics,
		requester:         req,
		search:            si,
		magnetMetadataAPI: mc,
		postProcessors:    GlobalPostProcessors,
	}
}

func HandlerIndex(w http.ResponseWriter, r *http.Request) {
	currentTime := time.Now().Format(time.RFC850)

	commonQueryParams := map[string]string{
		"q":              "search query",
		"page":           "page number",
		"filter_results": "drop results whose title is not similar to q (true/false, default: true)",
		"min_similarity": "similarity (0 to 1) a result needs to be kept when filter_results is on (default: 0.35)",
		"limit":          "maximum number of results to return",
		"sortBy":         "sort by field (title, original_title, year, date, seed_count, leech_count, size, similarity)",
		"sortDirection":  "sort direction (asc or desc, default: desc)",
		"audio":          "filter by audio languages (comma separated, e.g. brazilian,eng; por, pt-br and other ISO codes work too)",
		"year":           "filter by year (e.g. 2020)",
		"imdb":           "filter by imdb ID (e.g. tt1234567) - this ONLY FILTERTS results, for searching by IMDB ID use the \"q\" parameter",
		"season":         "filter by season (e.g. 4): keeps S04, S04E10, 4ª Temporada, 1ª a 5ª Temporada, Todas as Temporadas",
		"episode":        "filter by episode (e.g. 10): with season, keeps S04E10, S04E08-E10 and season 4 packs; alone, only explicit episodes",
	}

	// Define structs for ordered JSON output
	type EndpointDetail struct {
		Method      string                 `json:"method"`
		Description string                 `json:"description"`
		QueryParams map[string]string      `json:"query_params,omitempty"`
		Body        map[string]interface{} `json:"body,omitempty"`
	}

	type Endpoints struct {
		IndexerGeneric []EndpointDetail `json:"/indexers/{indexer_name}"`
		Manual         []EndpointDetail `json:"/indexers/manual"`
		Search         []EndpointDetail `json:"/search"`
		UI             []EndpointDetail `json:"/ui/"`
	}

	type RootResponse struct {
		Time         string      `json:"time"`
		Build        interface{} `json:"build"`
		IndexerNames []string    `json:"indexer_names"`
		Endpoints    Endpoints   `json:"endpoints"`
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	err := json.NewEncoder(w).Encode(RootResponse{
		Time:  currentTime,
		Build: consts.GetBuildInfo(),
		IndexerNames: []string{
			"torrent-dos-filmes",
			"comando_torrents",
			//"via_filmes",
			//"starck-filmes",
			//"bludv",
			//"filme_torrent",
			"rede_torrent",
			//"vaca_torrent",
			"torrentfilmeshds",
			"bludv_ru",
			"baixarapido",
			"betor",
		},
		Endpoints: Endpoints{
			IndexerGeneric: []EndpointDetail{
				{
					Method:      "GET",
					Description: "Indexer expoint for the specified indexer",
					QueryParams: commonQueryParams,
				},
			},
			Manual: []EndpointDetail{
				{
					Method:      "POST",
					Description: "Add a manual torrent entry to the indexer for 12 hours",
					Body: map[string]interface{}{
						"magnetLink": "magnet link",
					},
				},
				{
					Method:      "GET",
					Description: "Get all manual torrents",
				},
			},
			Search: []EndpointDetail{
				{
					Method:      "GET",
					Description: "Search for cached torrents across all indexers",
					QueryParams: map[string]string{
						"q":              "search query",
						"limit":          "maximum number of results to return (default: 10)",
						"filter_results": commonQueryParams["filter_results"],
						"min_similarity": commonQueryParams["min_similarity"],
						"sortBy":         commonQueryParams["sortBy"],
						"sortDirection":  commonQueryParams["sortDirection"],
						"audio":          commonQueryParams["audio"],
						"year":           commonQueryParams["year"],
						"imdb":           commonQueryParams["imdb"],
						"season":         commonQueryParams["season"],
						"episode":        commonQueryParams["episode"],
					},
				},
			},
			UI: []EndpointDetail{
				{
					Method:      "GET",
					Description: "Show the unified search UI (only work if Meilisearch is enabled)",
				},
			},
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
