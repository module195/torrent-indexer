package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/module195/torrent-indexer/schema"
)

// Contract tests: the JSON shape guindex reads from /sources?v=2 and
// /indexers/multi. They decode into generic maps on purpose, so renaming a
// field or changing its JSON type fails here even if the Go structs still
// round-trip. Adding fields is fine; removing or retyping one is a breaking
// change for clients.

type jsonKind int

const (
	kString jsonKind = iota
	kNumber
	kBool
	kArray
	kObject
)

func (k jsonKind) String() string {
	return [...]string{"string", "number", "bool", "array", "object"}[k]
}

func kindOf(v any) (jsonKind, bool) {
	switch v.(type) {
	case string:
		return kString, true
	case float64:
		return kNumber, true
	case bool:
		return kBool, true
	case []any:
		return kArray, true
	case map[string]any:
		return kObject, true
	}
	return 0, false
}

// requireFields fails unless obj has every key with the given JSON kind.
func requireFields(t *testing.T, where string, obj map[string]any, fields map[string]jsonKind) {
	t.Helper()
	for key, want := range fields {
		v, ok := obj[key]
		if !ok {
			t.Errorf("%s: missing %q", where, key)
			continue
		}
		if got, ok := kindOf(v); !ok || got != want {
			t.Errorf("%s: %q should be %s, got %T", where, key, want, v)
		}
	}
}

func decodeObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("invalid JSON %q: %v", body, err)
	}
	return m
}

func Test_contractSourcesV2(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	stats := NewIndexerStats()
	stats.now = func() time.Time { return now }
	stats.started = now.Add(-startupGrace)
	stats.record("betor", http.StatusOK, 300*time.Millisecond)
	for range unhealthyAfter {
		stats.record("rede_torrent", http.StatusBadGateway, time.Second)
	}

	rec := httptest.NewRecorder()
	NewSourcesHandler(stats, nil, Features{Multi: true, XCache: true}).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sources?v=2", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, content-type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := decodeObject(t, rec.Body.Bytes())
	requireFields(t, "/sources?v=2", body, map[string]jsonKind{
		"api_version":   kNumber,
		"indexer_names": kArray,
		"indexers":      kArray,
		"search":        kObject,
		"features":      kObject,
	})
	if body["api_version"] != float64(2) {
		t.Errorf("api_version = %v, want 2", body["api_version"])
	}
	for _, n := range body["indexer_names"].([]any) {
		if _, ok := n.(string); !ok {
			t.Errorf("indexer_names entries should be strings: %v", n)
		}
	}
	requireFields(t, "features", body["features"].(map[string]any), map[string]jsonKind{
		"multi": kBool, "x_cache": kBool, "response_cache": kBool,
	})
	requireFields(t, "search", body["search"].(map[string]any), map[string]jsonKind{"meilisearch": kBool})

	byName := map[string]map[string]any{}
	for _, it := range body["indexers"].([]any) {
		entry, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("indexers entries should be objects: %v", it)
		}
		requireFields(t, "indexers[]", entry, map[string]jsonKind{
			"name":                 kString,
			"supports_text":        kBool,
			"supports_imdb":        kBool,
			"healthy":              kBool,
			"consecutive_failures": kNumber,
			"samples":              kNumber,
		})
		byName[entry["name"].(string)] = entry
	}
	if len(byName) != len(ActiveIndexers) {
		t.Errorf("indexers has %d entries, want %d", len(byName), len(ActiveIndexers))
	}
	requireFields(t, "indexers[betor]", byName["betor"], map[string]jsonKind{"p50_ms": kNumber})
	down := byName["rede_torrent"]
	requireFields(t, "indexers[rede_torrent]", down, map[string]jsonKind{
		"last_failure_at": kString, "retry_after_s": kNumber,
	})
	if down["healthy"] != false {
		t.Errorf("rede_torrent should be unhealthy: %v", down)
	}
	if _, err := time.Parse(time.RFC3339, down["last_failure_at"].(string)); err != nil {
		t.Errorf("last_failure_at is not RFC 3339: %v", err)
	}
	if _, ok := byName["comando_torrents"]["retry_after_s"]; ok {
		t.Error("retry_after_s must be absent while healthy")
	}
}

func Test_contractIndexersMulti(t *testing.T) {
	h := NewMultiHandler(map[string]http.HandlerFunc{
		"rede_torrent": fakeIndexer(schema.IndexedTorrent{Title: "A", InfoHash: "aaaa", SeedCount: 3}),
		"bludv_ru": func(w http.ResponseWriter, r *http.Request) {
			writeJSONError(w, http.StatusInternalServerError, "boom")
		},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/indexers/multi?q=x&q=tt0111161&indexers=rede_torrent,bludv_ru", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, content-type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := decodeObject(t, rec.Body.Bytes())
	requireFields(t, "/indexers/multi", body, map[string]jsonKind{
		"results": kArray, "count": kNumber, "partial": kBool, "calls": kArray,
	})

	results := body["results"].([]any)
	if len(results) == 0 {
		t.Fatal("expected merged results")
	}
	for _, it := range results {
		requireFields(t, "results[]", it.(map[string]any), map[string]jsonKind{
			"title": kString, "info_hash": kString, "seed_count": kNumber, "found_in": kArray,
		})
	}

	states := map[string]bool{}
	for _, it := range body["calls"].([]any) {
		call := it.(map[string]any)
		requireFields(t, "calls[]", call, map[string]jsonKind{
			"indexer": kString, "q": kString, "state": kString, "count": kNumber, "duration_ms": kNumber,
		})
		state := call["state"].(string)
		switch state {
		case "ok", "error", "skipped", "timeout":
		default:
			t.Errorf("unknown call state %q", state)
		}
		states[state] = true
		if state == "skipped" {
			requireFields(t, "calls[skipped]", call, map[string]jsonKind{"reason": kString})
		}
		if state == "error" {
			requireFields(t, "calls[error]", call, map[string]jsonKind{"status": kNumber})
		}
	}
	for _, s := range []string{"ok", "error", "skipped"} {
		if !states[s] {
			t.Errorf("expected a %q call in %s", s, rec.Body.String())
		}
	}

	bad := httptest.NewRecorder()
	h.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/indexers/multi", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("missing q: status %d", bad.Code)
	}
	requireFields(t, "error body", decodeObject(t, bad.Body.Bytes()), map[string]jsonKind{"error": kString})
}
