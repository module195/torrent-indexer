package goscrape

import (
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

// Scrapes of the same tracker that start within batchWindow go out as one UDP
// request: BEP 15 takes up to 74 infohashes per scrape. A search with 100
// magnets used to send one request per magnet per tracker (thousands of
// packets, most of them timing out); now it is about two per tracker.
const (
	batchWindow  = 25 * time.Millisecond
	maxBatchSize = 74
	// same window GetLeechsAndSeeds waits for answers
	scrapeTimeout = 500 * time.Millisecond
)

var errInvalidInfohash = errors.New("invalid infohash")

type batchResult struct {
	peers peers
	err   error
}

type batchRequest struct {
	infoHash string
	result   chan batchResult
}

var (
	batchMu sync.Mutex
	pending = map[string][]batchRequest{} // tracker -> requests waiting for the next send
)

// scrapeBatched scrapes infoHash on tracker, sharing the UDP request with the
// other scrapes of the same tracker that arrive within batchWindow.
func scrapeBatched(tracker, infoHash string) (peers, error) {
	if !strings.HasPrefix(strings.ToLower(tracker), "udp://") {
		return peers{}, ErrUnsupportedScheme
	}
	infoHash = strings.ToLower(infoHash)
	if _, err := hex.DecodeString(infoHash); err != nil || len(infoHash) != 40 {
		return peers{}, errInvalidInfohash
	}

	req := batchRequest{infoHash: infoHash, result: make(chan batchResult, 1)}
	batchMu.Lock()
	queue := append(pending[tracker], req)
	if len(queue) >= maxBatchSize {
		delete(pending, tracker)
		batchMu.Unlock()
		go sendBatch(tracker, queue)
	} else {
		pending[tracker] = queue
		batchMu.Unlock()
		if len(queue) == 1 {
			time.AfterFunc(batchWindow, func() { flushBatch(tracker) })
		}
	}

	res := <-req.result
	return res.peers, res.err
}

func flushBatch(tracker string) {
	batchMu.Lock()
	queue := pending[tracker]
	delete(pending, tracker)
	batchMu.Unlock()
	if len(queue) > 0 {
		sendBatch(tracker, queue)
	}
}

// sendBatch scrapes every infohash in queue with one request and answers each
// waiting caller. Every request always gets exactly one result.
func sendBatch(tracker string, queue []batchRequest) {
	reply := func(err error, byHash map[string]peers) {
		for _, req := range queue {
			if err != nil {
				req.result <- batchResult{err: err}
				continue
			}
			req.result <- batchResult{peers: byHash[req.infoHash]}
		}
	}

	if scrapeSem != nil {
		// answers after the 500ms window are discarded anyway, so don't
		// queue longer than that
		select {
		case scrapeSem <- struct{}{}:
			defer func() { <-scrapeSem }()
		case <-time.After(scrapeTimeout):
			reply(errors.New("scrape concurrency limit reached"), nil)
			return
		}
	}

	scraper, err := New(tracker)
	if err != nil {
		reply(err, nil)
		return
	}
	defer scraper.Close()
	scraper.SetTimeout(scrapeTimeout)
	// answers after the 500ms window are discarded, so a retry only keeps
	// the socket open for nothing
	scraper.SetRetryLimit(0)

	// the same infohash can be asked twice in a batch (two indexers with the
	// same magnet); send it once
	hashes := make([][]byte, 0, len(queue))
	seen := make(map[string]bool, len(queue))
	for _, req := range queue {
		if !seen[req.infoHash] {
			seen[req.infoHash] = true
			hashes = append(hashes, []byte(req.infoHash))
		}
	}

	res, err := scraper.Scrape(hashes...)
	if err != nil {
		reply(err, nil)
		return
	}
	if len(res) != len(hashes) {
		reply(ErrResponse, nil)
		return
	}
	byHash := make(map[string]peers, len(res))
	for i, r := range res {
		byHash[string(hashes[i])] = peers{Seeders: int(r.Seeders), Leechers: int(r.Leechers)}
	}
	reply(nil, byHash)
}
