package handler

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

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
	limit := 100
	if limitStr != "" {
		var err error
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			http.Error(w, "Invalid limit parameter", http.StatusBadRequest)
			return
		}
	}

	// Try original query
	results, err := h.Module.SearchTorrent(query, limit)
	if err != nil {
		http.Error(w, "Failed to search torrents", http.StatusInternalServerError)
		return
	}

	// Apply similarity filter
	results = AddSimilarityCheck(nil, r, results)

	// If poor results and query looks English, try Portuguese translation
	if shouldTryTranslation(query, results) {
		ptQueryStr := translateToPT(query)
		if ptQueryStr != query {
			ptResults, err := h.Module.SearchTorrent(ptQueryStr, limit)
			if err == nil && len(ptResults) > 0 {
				// Create a new request with translated query for similarity check
				ptRequest := r.Clone(r.Context())
				q := ptRequest.URL.Query()
				q.Set("q", ptQueryStr)
				ptRequest.URL.RawQuery = q.Encode()
				
				ptResults = AddSimilarityCheck(nil, ptRequest, ptResults)
				if len(ptResults) > len(results) {
					results = ptResults
				}
			}
		}
	}

	response := map[string]interface{}{
		"results": results,
		"count":   len(results),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// normalizeQuery cleans the query string
func normalizeQuery(q string) string {
	// URL decode
	decoded, err := url.QueryUnescape(q)
	if err == nil {
		q = decoded
	}
	// Replace + with space
	q = strings.ReplaceAll(q, "+", " ")
	// Remove special chars (including colon) except alphanumeric, spaces, hyphens
	q = regexp.MustCompile(`[^\w\s\-]`).ReplaceAllString(q, " ")
	// Collapse multiple spaces
	q = regexp.MustCompile(`\s+`).ReplaceAllString(q, " ")
	return strings.TrimSpace(q)
}

// shouldTryTranslation checks if we should try a Portuguese translation
func shouldTryTranslation(query string, results []schema.IndexedTorrent) bool {
	// If no results or all results have low similarity
	if len(results) == 0 {
		return true
	}
	// Check if best result has low similarity
	if len(results) > 0 && results[0].Similarity < 0.3 {
		return true
	}
	// Check if query looks English (no Portuguese chars)
	hasPT := regexp.MustCompile(`[áéíóúãõç]`).MatchString(strings.ToLower(query))
	return !hasPT
}

// translateToPT attempts to translate common English movie titles to Portuguese
func translateToPT(query string) string {
	// Common translations (normalized, no special chars)
	translations := map[string]string{
		"spider man brand new day":    "homem aranha um novo dia",
		"spider man":                  "homem aranha",
		"spiderman":                   "homem aranha",
		"the batman":                  "batman",
		"avengers":                    "vingadores",
		"iron man":                    "homem de ferro",
		"captain america":             "capitao america",
		"black widow":                 "viuva negra",
		"doctor strange":              "doutor estranho",
		"guardians of the galaxy":     "guardioes da galaxia",
		"ant man":                     "homem formiga",
		"black panther":               "pantera negra",
		"wonder woman":                "mulher maravilha",
		"joker":                       "coringa",
		"fantastic four":              "quarteto fantastico",
	}

	qLower := strings.ToLower(query)
	
	// Direct match
	if pt, ok := translations[qLower]; ok {
		return pt
	}
	
	// Partial match (longest first)
	var matches []string
	for en := range translations {
		if strings.Contains(qLower, en) {
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
		return strings.Replace(qLower, en, translations[en], 1)
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
