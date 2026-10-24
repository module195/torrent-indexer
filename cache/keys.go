package cache

import "os"

// Key namespaces (Phase 2). Only applied when REDIS_NAMESPACED_KEYS=true so the
// default keyspace stays exactly as before; when enabled, the old un-prefixed
// keys simply expire on their own TTL.
const (
	KindDoc      = "doc"
	KindDocShort = "doc:short"
	KindPeers    = "peers"
	KindMeta     = "meta"
	KindResp     = "resp"
)

var namespacedKeys = os.Getenv("REDIS_NAMESPACED_KEYS") == "true"

// Key returns the redis key for the given kind. With namespacing disabled it
// returns legacy unless legacy is empty (new kinds always get a prefix).
func Key(kind, key, legacy string) string {
	if namespacedKeys || legacy == "" {
		return kind + ":" + key
	}
	return legacy
}
