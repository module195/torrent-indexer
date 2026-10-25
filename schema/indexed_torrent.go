package schema

import "time"

type IndexedTorrent struct {
	Title         string    `json:"title"`
	OriginalTitle string    `json:"original_title"`
	Details       string    `json:"details"`
	Year          string    `json:"year"`
	IMDB          string    `json:"imdb"`
	Audio         []Audio   `json:"audio"`
	MagnetLink    string    `json:"magnet_link"`
	Date          time.Time `json:"date,omitzero"`
	InfoHash      string    `json:"info_hash"`
	Trackers      []string  `json:"trackers"`
	Size          string    `json:"size"`
	// SizeBytes is Size as a number, when known from the torrent's metadata
	SizeBytes  int64   `json:"size_bytes,omitempty"`
	Files      []File  `json:"files,omitempty"`
	LeechCount int     `json:"leech_count"`
	SeedCount  int     `json:"seed_count"`
	Similarity float32 `json:"similarity"`
	Indexer    string  `json:"indexer,omitempty"`
}

type File struct {
	Path string `json:"path"`
	Size string `json:"size"`
	// SizeBytes is Size as a number, so clients need not parse "1.2 GB" back
	SizeBytes int64 `json:"size_bytes,omitempty"`
}
