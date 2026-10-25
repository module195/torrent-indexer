package magnet

import (
	"context"
	"crypto/sha1" //nolint:gosec // BitTorrent v1 info hashes are SHA-1
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultTorrentCacheURL is a public cache that serves .torrent files by info
// hash (upper case hex). A miss redirects to an HTML page.
const DefaultTorrentCacheURL = "https://itorrents.net/torrent/"

// maxTorrentFileSize caps what is read from the cache: a .torrent with
// thousands of files is a few hundred KB.
const maxTorrentFileSize = 8 << 20

var errNotTorrent = errors.New("torrent cache did not return a .torrent")

// fetchFromTorrentCache downloads the .torrent for infoHash from the public
// cache and reads name, size and files from its info dictionary. The info
// dictionary must hash to infoHash, so an unrelated or broken file is
// rejected. The error wraps errNotTorrent when the cache answered that it
// doesn't have a valid .torrent for infoHash.
func (c *MetadataClient) fetchFromTorrentCache(ctx context.Context, infoHash string) (*MetadataResponse, error) {
	select {
	case c.torrentCacheSem <- struct{}{}:
		defer func() { <-c.torrentCacheSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	u := c.torrentCacheURL + strings.ToUpper(infoHash) + ".torrent"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.torrentCacheHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound || (resp.StatusCode >= 300 && resp.StatusCode < 400):
		// the cache doesn't have it
		return nil, fmt.Errorf("%w: status %s", errNotTorrent, resp.Status)
	default:
		// rate limited or down: not a verdict on this torrent
		return nil, fmt.Errorf("torrent cache responded with status: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTorrentFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxTorrentFileSize {
		return nil, fmt.Errorf("%w: larger than %d bytes", errNotTorrent, maxTorrentFileSize)
	}
	return parseTorrentFile(body, infoHash)
}

// parseTorrentFile reads a .torrent (bencode) and checks that its info
// dictionary hashes to infoHash.
func parseTorrentFile(data []byte, infoHash string) (*MetadataResponse, error) {
	d := &bdecoder{data: data}
	root, err := d.value()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNotTorrent, err)
	}
	top, ok := root.(map[string]any)
	if !ok {
		return nil, errNotTorrent
	}
	info, ok := top["info"].(map[string]any)
	if !ok || d.infoEnd <= d.infoStart {
		return nil, fmt.Errorf("%w: no info dictionary", errNotTorrent)
	}
	sum := sha1.Sum(data[d.infoStart:d.infoEnd]) //nolint:gosec // BitTorrent v1 info hash
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, infoHash) {
		return nil, fmt.Errorf("%w: info hash %s, want %s", errNotTorrent, got, infoHash)
	}

	m := &MetadataResponse{InfoHash: strings.ToLower(infoHash)}
	m.Name = bstring(info, "name.utf-8")
	if m.Name == "" {
		m.Name = bstring(info, "name")
	}
	if n, ok := info["length"].(int64); ok {
		// single-file torrent
		m.Size = n
		m.Files = []TorrentFile{{Path: m.Name, Size: n}}
	} else if files, ok := info["files"].([]any); ok {
		var offset int64
		for _, f := range files {
			fd, ok := f.(map[string]any)
			if !ok {
				continue
			}
			n, _ := fd["length"].(int64)
			path := bpath(fd, "path.utf-8")
			if path == "" {
				path = bpath(fd, "path")
			}
			// BEP 47 padding files are not part of the content
			if attr := bstring(fd, "attr"); strings.Contains(attr, "p") || strings.HasPrefix(path, ".pad/") {
				offset += n
				continue
			}
			if m.Name != "" {
				path = m.Name + "/" + path
			}
			m.Files = append(m.Files, TorrentFile{Path: path, Size: n, Offset: offset})
			m.Size += n
			offset += n
		}
	}
	if m.Size <= 0 {
		return nil, fmt.Errorf("%w: no length", errNotTorrent)
	}
	if t, ok := top["creation date"].(int64); ok && t > 0 {
		m.CreatedAt = time.Unix(t, 0).UTC()
	}
	m.CreatedBy = bstring(top, "created by")
	m.Comment = bstring(top, "comment")
	return m, nil
}

func bstring(d map[string]any, key string) string {
	s, _ := d[key].(string)
	return s
}

func bpath(d map[string]any, key string) string {
	parts, _ := d[key].([]any)
	var out []string
	for _, p := range parts {
		if s, ok := p.(string); ok {
			out = append(out, s)
		}
	}
	return strings.Join(out, "/")
}

// bdecoder is a minimal bencode decoder. Dictionaries become map[string]any,
// lists []any, integers int64 and byte strings string. It records where the
// top-level "info" value starts and ends, to hash it.
type bdecoder struct {
	data               []byte
	pos                int
	depth              int
	infoStart, infoEnd int
}

const maxBencodeDepth = 64

func (d *bdecoder) value() (any, error) {
	if d.pos >= len(d.data) {
		return nil, io.ErrUnexpectedEOF
	}
	switch c := d.data[d.pos]; {
	case c == 'i':
		end := d.index('e', d.pos+1)
		if end < 0 {
			return nil, io.ErrUnexpectedEOF
		}
		n, err := strconv.ParseInt(string(d.data[d.pos+1:end]), 10, 64)
		if err != nil {
			return nil, err
		}
		d.pos = end + 1
		return n, nil
	case c >= '0' && c <= '9':
		return d.str()
	case c == 'l' || c == 'd':
		d.depth++
		if d.depth > maxBencodeDepth {
			return nil, errors.New("bencode nested too deep")
		}
		defer func() { d.depth-- }()
		d.pos++
		if c == 'l' {
			var list []any
			for d.pos < len(d.data) && d.data[d.pos] != 'e' {
				v, err := d.value()
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			if d.pos >= len(d.data) {
				return nil, io.ErrUnexpectedEOF
			}
			d.pos++
			return list, nil
		}
		dict := map[string]any{}
		for d.pos < len(d.data) && d.data[d.pos] != 'e' {
			k, err := d.str()
			if err != nil {
				return nil, err
			}
			start := d.pos
			v, err := d.value()
			if err != nil {
				return nil, err
			}
			if k == "info" && d.depth == 1 {
				d.infoStart, d.infoEnd = start, d.pos
			}
			dict[k] = v
		}
		if d.pos >= len(d.data) {
			return nil, io.ErrUnexpectedEOF
		}
		d.pos++
		return dict, nil
	default:
		return nil, fmt.Errorf("bencode: unexpected %q at %d", c, d.pos)
	}
}

func (d *bdecoder) str() (string, error) {
	colon := d.index(':', d.pos)
	if colon < 0 {
		return "", io.ErrUnexpectedEOF
	}
	n, err := strconv.Atoi(string(d.data[d.pos:colon]))
	if err != nil || n < 0 {
		return "", fmt.Errorf("bencode: bad string length at %d", d.pos)
	}
	end := colon + 1 + n
	if end > len(d.data) || end < colon {
		return "", io.ErrUnexpectedEOF
	}
	d.pos = end
	return string(d.data[colon+1 : end]), nil
}

func (d *bdecoder) index(b byte, from int) int {
	for i := from; i < len(d.data); i++ {
		if d.data[i] == b {
			return i
		}
	}
	return -1
}
