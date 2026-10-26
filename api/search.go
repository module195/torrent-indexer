package handler

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/schema"
	meilisearch "github.com/module195/torrent-indexer/search"
)

// MeilisearchHandler handles HTTP requests for Meilisearch integration.
type MeilisearchHandler struct {
	Module *meilisearch.SearchIndexer
}

// HealthResponse represents the health check response
type HealthResponse struct {
	Status    string                 `json:"status"`
	Service   string                 `json:"service"`
	Details   map[string]interface{} `json:"details,omitempty"`
	Timestamp string                 `json:"timestamp"`
}

// StatsResponse represents the stats endpoint response
type StatsResponse struct {
	Status            string           `json:"status"`
	NumberOfDocuments int64            `json:"numberOfDocuments"`
	IsIndexing        bool             `json:"isIndexing"`
	FieldDistribution map[string]int64 `json:"fieldDistribution"`
	Service           string           `json:"service"`
}

// NewMeilisearchHandler creates a new instance of MeilisearchHandler.
func NewMeilisearchHandler(module *meilisearch.SearchIndexer) *MeilisearchHandler {
	return &MeilisearchHandler{Module: module}
}

// SearchTorrentHandler handles the searching of torrent items.
func (h *MeilisearchHandler) SearchTorrentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		query = time.Now().Format("2006-01-02")
	}

	// Normalize query (decode URL, clean special chars)
	query = normalizeQuery(query)

	limitStr := r.URL.Query().Get("limit")
	limit := 200
	if limitStr != "" {
		var err error
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			http.Error(w, "Invalid limit parameter", http.StatusBadRequest)
			return
		}
	}

	ctx := r.Context()

	// IMDb ID (tt1234567): look it up in the imdb field instead of the titles.
	// Every hit ties, and Meilisearch returns them oldest first: fetch them
	// all, so the limit keeps the best seeded (and whatever season/episode
	// asks for) instead of the first ones indexed.
	if id := strings.ToLower(query); reIMDbID.MatchString(id) {
		results, err := h.Module.SearchTorrentByIMDB(ctx, id, meilisearch.MaxSearchHits)
		if err != nil {
			http.Error(w, "Failed to search torrents", http.StatusInternalServerError)
			return
		}
		results = filterAndSort(r, filterByIMDBID(results, id))
		writeSearchResults(w, results[:min(len(results), limit)])
		return
	}

	// Try original query
	results, err := h.Module.SearchTorrent(ctx, query, limit)
	if err != nil {
		http.Error(w, "Failed to search torrents", http.StatusInternalServerError)
		return
	}

	// Apply the similarity filter against the query Meilisearch got: the raw
	// one keeps punctuation ("Star Wars: Andor S02E03") that release names
	// lack, and the right release scored below the threshold
	results = AddSimilarityCheck(nil, withQuery(r, query), results)

	// If poor results and query looks English, try Portuguese translation
	if shouldTryTranslation(query, results) {
		ptQueryStr := translateToPT(query)
		if ptQueryStr != query {
			ptResults, err := h.Module.SearchTorrent(ctx, ptQueryStr, limit)
			if err == nil && len(ptResults) > 0 {
				ptResults = AddSimilarityCheck(nil, withQuery(r, ptQueryStr), ptResults)

				// Merge: keep items with good similarity against EN or PT query
				results = mergeByBestSimilarity(results, ptResults, translationMinSimilarity(r))
			}
		}
	}

	// the PT search adds up to limit more
	results = filterAndSort(r, results)
	writeSearchResults(w, results[:min(len(results), limit)])
}

// withQuery returns a copy of r whose q is query.
func withQuery(r *http.Request, query string) *http.Request {
	clone := r.Clone(r.Context())
	q := clone.URL.Query()
	q.Set("q", query)
	clone.URL.RawQuery = q.Encode()
	return clone
}

// filterAndSort applies the same audio/year/imdb/season/episode filters and
// sortBy/sortDirection ordering as /indexers/<name>.
func filterAndSort(r *http.Request, results []schema.IndexedTorrent) []schema.IndexedTorrent {
	results = FilterBy(nil, r, results)
	slices.SortStableFunc(results, torrentComparator(r.URL.Query()))
	return results
}

// translationMinSimilarity is the score a hit needs against the EN or the PT
// query to survive the merge, following filter_results and min_similarity.
func translationMinSimilarity(r *http.Request) float32 {
	if !filterResults(r) {
		return 0
	}
	if minSim, ok := minSimilarityParam(r); ok {
		return minSim
	}
	return 0.4
}

func writeSearchResults(w http.ResponseWriter, results []schema.IndexedTorrent) {
	results = AddPublicTrackers(nil, nil, results)
	response := map[string]interface{}{
		"results": results,
		"count":   len(results),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// reIMDbIDInLink finds the ID in an imdb field such as
// "https://www.imdb.com/title/tt3581920/".
var reIMDbIDInLink = regexp.MustCompile(`\btt\d{7,8}\b`)

// filterByIMDBID keeps only the torrents whose imdb link has exactly this ID
// (tt1234567 must not match tt12345678).
func filterByIMDBID(torrents []schema.IndexedTorrent, id string) []schema.IndexedTorrent {
	out := make([]schema.IndexedTorrent, 0, len(torrents))
	for _, it := range torrents {
		if reIMDbIDInLink.FindString(strings.ToLower(it.IMDB)) == id {
			it.Similarity = 1 // exact ID match; the stored score was computed for another query
			out = append(out, it)
		}
	}
	return out
}

// mergeByBestSimilarity keeps items that score >= minSim against either query.
// Dedupes by info hash, keeping the higher similarity, and returns them by
// similarity (then seeds), so the order is the same on every request.
func mergeByBestSimilarity(a, b []schema.IndexedTorrent, minSim float32) []schema.IndexedTorrent {
	out := make([]schema.IndexedTorrent, 0, len(a)+len(b))
	pos := make(map[string]int)
	for _, it := range slices.Concat(a, b) {
		if it.Similarity < minSim {
			continue
		}
		key := torrentKey(it)
		if key == "" {
			out = append(out, it)
			continue
		}
		if i, ok := pos[key]; ok {
			if it.Similarity > out[i].Similarity {
				out[i] = it
			}
			continue
		}
		pos[key] = len(out)
		out = append(out, it)
	}
	slices.SortStableFunc(out, torrentComparator(url.Values{"sortBy": {"similarity"}}))
	return out
}

// reQueryNoise matches everything but letters (any script, accented
// included), digits, whitespace and hyphens.
var reQueryNoise = regexp.MustCompile(`[^\p{L}\p{N}\s\-]`)

// normalizeQuery cleans the query string
func normalizeQuery(q string) string {
	// URL decode
	decoded, err := url.QueryUnescape(q)
	if err == nil {
		q = decoded
	}
	// Replace + with space
	q = strings.ReplaceAll(q, "+", " ")
	// Remove special chars (including colon) except letters, digits, spaces, hyphens
	q = reQueryNoise.ReplaceAllString(q, " ")
	// Collapse multiple spaces
	q = reWhitespace.ReplaceAllString(q, " ")
	return strings.TrimSpace(q)
}

// shouldTryTranslation checks if we should try a Portuguese translation
func shouldTryTranslation(query string, results []schema.IndexedTorrent) bool {
	// If no results or all results have low similarity
	if len(results) == 0 {
		return true
	}
	// Check if best result has low similarity
	if len(results) > 0 && results[0].Similarity < 0.5 {
		return true
	}
	// Check if query looks English (no Portuguese chars)
	hasPT := rePortugueseChars.MatchString(strings.ToLower(query))
	return !hasPT
}

// translateToPT attempts to translate common English movie titles to Portuguese
func translateToPT(query string) string {
	// Common translations (normalized, no special chars)
	translations := map[string]string{
		"spider man brand new day": "homem aranha um novo dia",
		"spider man":               "homem aranha",
		"spiderman":                "homem aranha",
		"the batman":               "batman",
		"avengers":                 "vingadores",
		"iron man":                 "homem de ferro",
		"captain america":          "capitao america",
		"black widow":              "viuva negra",
		"doctor strange":           "doutor estranho",
		"guardians of the galaxy":  "guardioes da galaxia",
		"ant man":                  "homem formiga",
		"black panther":            "pantera negra",
		"wonder woman":             "mulher maravilha",
		"joker":                    "coringa",
		"fantastic four":           "quarteto fantastico",
	}

	qLower := strings.ToLower(query)

	// Direct match
	if pt, ok := translations[qLower]; ok {
		return pt
	}

	// Partial match on whole words (longest first): "ant man" is not in
	// "the elephant man", nor "iron man" in "iron mantis"
	padded := " " + qLower + " "
	var matches []string
	for en := range translations {
		if strings.Contains(padded, " "+en+" ") {
			matches = append(matches, en)
		}
	}
	// Sort by length descending to match longest first
	for i := 0; i < len(matches); i++ {
		for j := i + 1; j < len(matches); j++ {
			if len(matches[j]) > len(matches[i]) {
				matches[i], matches[j] = matches[j], matches[i]
			}
		}
	}
	if len(matches) > 0 {
		en := matches[0]
		return strings.TrimSpace(strings.Replace(padded, " "+en+" ", " "+translations[en]+" ", 1))
	}

	return query
}

// HealthHandler provides a health check endpoint for Meilisearch.
func (h *MeilisearchHandler) HealthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	isHealthy := h.Module.IsHealthy()

	response := HealthResponse{
		Service:   "meilisearch",
		Timestamp: getCurrentTimestamp(),
	}

	if isHealthy {
		stats, err := h.Module.GetStats()
		if err == nil {
			response.Status = "healthy"
			response.Details = map[string]interface{}{
				"documents": stats.NumberOfDocuments,
				"indexing":  stats.IsIndexing,
			}
			w.WriteHeader(http.StatusOK)
		} else {
			response.Status = "degraded"
			response.Details = map[string]interface{}{
				"error": "Could not retrieve stats",
			}
			w.WriteHeader(http.StatusOK)
		}
	} else {
		response.Status = "unhealthy"
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// StatsHandler provides detailed statistics about the Meilisearch index.
func (h *MeilisearchHandler) StatsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	stats, err := h.Module.GetStats()
	if err != nil {
		logging.Error().Err(err).Msg("Failed to retrieve Meilisearch stats")
		if !h.Module.IsHealthy() {
			http.Error(w, "Meilisearch service is unavailable", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "Failed to retrieve statistics", http.StatusInternalServerError)
		return
	}

	response := StatsResponse{
		Status:            "healthy",
		Service:           "meilisearch",
		NumberOfDocuments: stats.NumberOfDocuments,
		IsIndexing:        stats.IsIndexing,
		FieldDistribution: stats.FieldDistribution,
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

func getCurrentTimestamp() string {
	return time.Now().Format(time.RFC3339)
}
