package requester

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/module195/torrent-indexer/logging"
	"github.com/module195/torrent-indexer/utils"
)

type FlareSolverr struct {
	url         string
	maxTimeout  int
	httpClient  *http.Client
	sessionPool chan string
	mu          sync.Mutex
	initiated   atomic.Bool
	initMu      sync.Mutex
	lastInit    time.Time
	// sessionless is set for solvers that only speak request.get (Byparr):
	// sessions.* are never sent and pool entries are just concurrency slots.
	sessionless atomic.Bool
	detected    bool // guarded by initMu
	// clearances keeps each host's cf_clearance for the plain client
	clearances *clearanceStore
}

// dumbSession is a pool slot that is not a real FlareSolverr session.
const dumbSession = "dumb-session"

// flaresolverrInitRetryInterval throttles re-initialization attempts when
// FlareSolverr was unreachable at startup.
const flaresolverrInitRetryInterval = time.Minute

var (
	ErrListSessions = fmt.Errorf("failed to list sessions")
)

// flaresolverrMaxBody bounds the page read back when FlareSolverr only
// returns cookies and the page is fetched again with them.
const flaresolverrMaxBody = 32 << 20

// sessionPrefix names the sessions this process creates. Only sessions with
// it are adopted at start: another indexer replica, or another app sharing
// the solver, keeps its own (a session is one browser tab; two processes
// using the same one at once step on each other).
var sessionPrefix = func() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "local"
	}
	return "torrent-indexer-" + host + "-"
}()

func newSessionID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return sessionPrefix + hex.EncodeToString(b)
}

// cookieURL is the URL a solver cookie is stored under in a cookie jar. The
// jar ignores URLs without an http(s) scheme, and the domain may come with a
// leading dot (".site.com").
func cookieURL(target *url.URL, domain string) *url.URL {
	host := strings.TrimPrefix(domain, ".")
	if host == "" {
		host = target.Hostname()
	}
	scheme := target.Scheme
	if scheme != "http" && scheme != "https" {
		scheme = "https"
	}
	return &url.URL{Scheme: scheme, Host: host, Path: "/"}
}

func NewFlareSolverr(url string, timeoutMilli int, poolSize int) *FlareSolverr {
	if poolSize <= 0 {
		poolSize = 5
	}
	// FlareSolverr itself enforces maxTimeout; give the HTTP client some
	// headroom so we receive its error response instead of cutting it off.
	httpClient := &http.Client{
		Timeout: time.Duration(timeoutMilli)*time.Millisecond + 5*time.Second,
	}
	sessionPool := make(chan string, poolSize)

	f := &FlareSolverr{
		url:         url,
		maxTimeout:  timeoutMilli,
		httpClient:  httpClient,
		sessionPool: sessionPool,
		clearances:  newClearanceStore(),
	}

	f.tryInit()

	return f
}

// tryInit fills the session pool and marks the instance as initiated.
// Attempts are throttled so an unavailable FlareSolverr is not hammered.
func (f *FlareSolverr) tryInit() bool {
	if f.initiated.Load() {
		return true
	}
	if f.url == "" {
		return false
	}
	f.initMu.Lock()
	defer f.initMu.Unlock()
	if f.initiated.Load() {
		return true
	}
	if !f.lastInit.IsZero() && time.Since(f.lastInit) < flaresolverrInitRetryInterval {
		return false
	}
	f.lastInit = time.Now()
	if err := f.FillSessionPool(); err != nil {
		return false
	}
	if len(f.sessionPool) == 0 {
		return false
	}
	f.initiated.Store(true)
	return true
}

// detectSessionSupport finds out whether the solver understands sessions.*.
// FLARESOLVERR_SESSIONS=true|false forces the answer. Otherwise GET / is
// probed: FlareSolverr answers {"msg": "FlareSolverr is ready!", ...}; Byparr
// redirects to /docs and ignores "cmd", so a sessions.list/create would make
// it open the browser on an empty URL. Unknown solvers keep the old behavior
// (try sessions, fall back to slots if sessions.list is not understood).
func (f *FlareSolverr) detectSessionSupport() (bool, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FLARESOLVERR_SESSIONS"))) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(strings.TrimRight(f.url, "/") + "/")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return false, nil // Byparr: / -> /docs
	}
	var info struct {
		Msg string `json:"msg"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info)
	msg := strings.ToLower(info.Msg)
	if strings.Contains(msg, "byparr") {
		return false, nil
	}
	return true, nil
}

// SupportsSessions reports whether sessions.* are used with this solver.
func (f *FlareSolverr) SupportsSessions() bool {
	return !f.sessionless.Load()
}

func (f *FlareSolverr) fillDumbSessions() {
	for len(f.sessionPool) < cap(f.sessionPool) {
		f.sessionPool <- dumbSession
	}
}

func (f *FlareSolverr) FillSessionPool() error {
	// Check if the pool is already filled
	if len(f.sessionPool) == cap(f.sessionPool) {
		return nil
	}

	if !f.detected {
		supported, err := f.detectSessionSupport()
		if err != nil {
			logging.Error().Err(err).Msg("Failed to reach FlareSolverr")
			return err
		}
		f.detected = true
		f.sessionless.Store(!supported)
		if !supported {
			logging.Info().Int("slots", cap(f.sessionPool)).Msg("Solver without session support (e.g. Byparr); using concurrency slots instead of sessions")
		}
	}
	if f.sessionless.Load() {
		f.fillDumbSessions()
		return nil
	}

	// Pre-initialize the pool with existing sessions
	sessions, err := f.ListSessions()
	if err != nil {
		// if fail to list sessions, it may not support the sessions.list command
		// create new dumb sessions to fill the pool
		if err == ErrListSessions {
			f.fillDumbSessions()
			return nil
		}
		logging.Error().Err(err).Msg("Failed to list existing FlareSolverr sessions")
		return err
	} else {
		for _, session := range sessions {
			if !strings.HasPrefix(session, sessionPrefix) {
				continue // someone else's session
			}
			// Add available sessions to the pool
			if len(f.sessionPool) < cap(f.sessionPool) {
				f.sessionPool <- session
			}
		}
		if len(f.sessionPool) > 0 {
			logging.Info().Int("sessions", len(f.sessionPool)).Msg("Added FlareSolverr sessions to the pool")
		}
	}

	// If fewer than poolSize sessions were found, create new ones to fill the pool
	for len(f.sessionPool) < cap(f.sessionPool) {
		if f.CreateSession() == "" {
			// avoid spinning forever when session creation keeps failing
			break
		}
	}

	if len(f.sessionPool) == 0 {
		return fmt.Errorf("failed to create FlareSolverr sessions")
	}
	return nil
}

func (f *FlareSolverr) CreateSession() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	body := map[string]string{"cmd": "sessions.create", "session": newSessionID()}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return ""
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/v1", f.url), bytes.NewBuffer(jsonBody))
	if err != nil {
		return ""
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return ""
	}

	defer resp.Body.Close()

	var sessionResponse map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&sessionResponse)
	if err != nil {
		return ""
	}

	session, ok := sessionResponse["session"].(string)
	if !ok {
		logging.Error().Msg("Failed to create FlareSolverr session")
		return ""
	}
	// Add session to the pool
	f.sessionPool <- session

	logging.Info().Str("session", session).Msg("Created new FlareSolverr session")
	return session
}

func (f *FlareSolverr) ListSessions() ([]string, error) {
	body := map[string]string{"cmd": "sessions.list"}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/v1", f.url), bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	var sessionsResponse map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&sessionsResponse)
	if err != nil {
		return nil, err
	}
	if sessionsResponse["sessions"] == nil {
		return nil, ErrListSessions
	}

	// unchecked type assertions here panicked on solvers that answer
	// "sessions" in another shape
	sessions, ok := sessionsResponse["sessions"].([]interface{})
	if !ok {
		return nil, ErrListSessions
	}
	var sessionIDs []string
	for _, session := range sessions {
		if id, ok := session.(string); ok {
			sessionIDs = append(sessionIDs, id)
		}
	}

	return sessionIDs, nil
}

func (f *FlareSolverr) RetrieveSession(ctx context.Context) (string, error) {
	// Blocking receive from the session pool.
	select {
	case session := <-f.sessionPool:
		return session, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type Response struct {
	Status   string   `json:"status"`
	Message  string   `json:"message"`
	Solution solution `json:"solution"`
}

type solution struct {
	Url       string            `json:"url"`
	Status    int               `json:"status"`
	Cookies   []solverCookie    `json:"cookies"`
	UserAgent string            `json:"userAgent"`
	Headers   map[string]string `json:"headers"`
	Response  string            `json:"response"`
}

type solverCookie struct {
	Domain   string `json:"domain"`
	Expiry   int    `json:"expiry"`
	HttpOnly bool   `json:"httpOnly"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	SameSite string `json:"sameSite"`
	Secure   bool   `json:"secure"`
	Value    string `json:"value"`
}

func (f *FlareSolverr) Get(ctx context.Context, _url string, attempts int) (io.ReadCloser, error) {
	// Check if the FlareSolverr instance was initiated
	if !f.tryInit() {
		return io.NopCloser(bytes.NewReader([]byte(""))), nil
	}

	for {
		body, retry, err := f.get(ctx, _url)
		if retry && attempts != 0 {
			attempts--
			logging.Warn().Str("url", _url).Int("attempts_left", attempts).Msg("FlareSolverr Internal Server Error, retrying")
			continue
		}
		return body, err
	}
}

// get performs a single FlareSolverr request. retry reports whether the
// failure was a FlareSolverr 500 that is worth retrying. The session is
// always returned to the pool before get returns.
func (f *FlareSolverr) get(ctx context.Context, _url string) (io.ReadCloser, bool, error) {
	// Retrieve session from the pool (blocking if no sessions available)
	session, err := f.RetrieveSession(ctx)
	if err != nil {
		return nil, false, err
	}

	// Ensure the session is returned to the pool after the request is done
	defer func() {
		f.sessionPool <- session
	}()

	body := map[string]interface{}{
		"cmd":        "request.get",
		"url":        _url,
		"maxTimeout": f.maxTimeout,
	}
	if session != dumbSession {
		body["session"] = session
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, false, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/v1", f.url), bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, false, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	// Parse the response
	var response Response
	err = json.NewDecoder(resp.Body).Decode(&response)
	if err != nil {
		return nil, false, err
	}

	// Check if the response was successful
	if response.Status != "ok" {
		err := fmt.Errorf("failed to get response: %s, statusCode: %s", response.Message, resp.Status)
		return nil, resp.StatusCode == http.StatusInternalServerError, err
	}

	// the site's own status, as the solver saw it: a 5xx/403/429 page is a
	// failure, the same rule the plain client follows
	if isUpstreamFailure(response.Solution.Status) {
		return nil, false, &UpstreamStatusError{URL: _url, StatusCode: response.Solution.Status}
	}

	// Check if "Under attack" is in the response
	if strings.Contains(response.Solution.Response, "Under attack") {
		return nil, false, fmt.Errorf("under attack")
	}

	// check if the response is valid HTML
	if !utils.IsValidHTML(response.Solution.Response) {
		logging.Warn().Str("url", _url).Msg("FlareSolverr returned invalid HTML response")
		logging.Debug().Str("url", _url).Str("response", response.Solution.Response).Msg("FlareSolverr response content")
		response.Solution.Response = ""
	}

	// If the response body is empty but cookies are present, make a new request
	if response.Solution.Response == "" && len(response.Solution.Cookies) > 0 {
		logging.Debug().Str("url", _url).Msg("FlareSolverr making new request with cookies")
		// Create a new request with cookies
		client := &http.Client{
			Timeout: time.Duration(f.maxTimeout) * time.Millisecond,
		}
		cookieJar, err := cookiejar.New(&cookiejar.Options{})
		if err != nil {
			return nil, false, err
		}
		target, err := url.Parse(_url)
		if err != nil {
			return nil, false, err
		}
		for _, cookie := range response.Solution.Cookies {
			cookieJar.SetCookies(cookieURL(target, cookie.Domain), []*http.Cookie{
				{
					Name:   cookie.Name,
					Value:  cookie.Value,
					Domain: cookie.Domain,
					Path:   cookie.Path,
				},
			})
		}
		client.Jar = cookieJar

		secondReq, err := http.NewRequestWithContext(ctx, "GET", _url, nil)
		if err != nil {
			return nil, false, err
		}

		// use the same user returned by the FlareSolverr
		secondReq.Header.Set("User-Agent", response.Solution.UserAgent)

		secondResp, err := client.Do(secondReq)
		if err != nil {
			return nil, false, err
		}
		defer secondResp.Body.Close()
		if isUpstreamFailure(secondResp.StatusCode) {
			return nil, false, &UpstreamStatusError{URL: _url, StatusCode: secondResp.StatusCode}
		}

		respByte := new(bytes.Buffer)
		n, err := respByte.ReadFrom(io.LimitReader(secondResp.Body, flaresolverrMaxBody+1))
		if err != nil {
			return nil, false, err
		}
		if n > flaresolverrMaxBody {
			return nil, false, fmt.Errorf("page larger than %d bytes: %s", flaresolverrMaxBody, _url)
		}

		f.clearances.remember(_url, response.Solution)
		// Return the body of the second request
		return io.NopCloser(bytes.NewReader(respByte.Bytes())), false, nil
	}

	if response.Solution.Response != "" {
		f.clearances.remember(_url, response.Solution)
	}
	// Return the original response body
	return io.NopCloser(bytes.NewReader([]byte(response.Solution.Response))), false, nil
}

// SessionsAvailable returns how many sessions are idle in the pool.
func (f *FlareSolverr) SessionsAvailable() int {
	return len(f.sessionPool)
}

// StartHealthCheck periodically verifies the pooled sessions against
// FlareSolverr and recreates the ones it no longer knows (e.g. after a
// FlareSolverr restart). It also retries initialization if FlareSolverr was
// down at startup. Phase 2, opt-in.
func (f *FlareSolverr) StartHealthCheck(interval time.Duration) {
	if interval <= 0 || f.url == "" {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			f.healthCheck()
		}
	}()
}

func (f *FlareSolverr) healthCheck() {
	if !f.initiated.Load() {
		f.tryInit()
		return
	}
	if f.sessionless.Load() {
		return // nothing to verify: slots, not sessions
	}

	sessions, err := f.ListSessions()
	if err != nil {
		// ErrListSessions: FlareSolverr without sessions.list (dumb sessions)
		if err != ErrListSessions {
			logging.Warn().Err(err).Msg("FlareSolverr health check failed")
		}
		return
	}
	valid := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		valid[s] = true
	}

	// only idle sessions are checked; in-use ones are checked on a later tick
	idle := len(f.sessionPool)
	for k := 0; k < idle; k++ {
		var session string
		select {
		case session = <-f.sessionPool:
		default:
			return
		}
		if valid[session] {
			f.sessionPool <- session
			continue
		}
		logging.Warn().Str("session", session).Msg("FlareSolverr session no longer exists, recreating")
		if f.CreateSession() == "" {
			// keep pool capacity even if recreation failed; retry next tick
			f.sessionPool <- session
		}
	}
}
