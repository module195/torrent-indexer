package requester

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A Cloudflare clearance (cf_clearance + the solver's user agent) is valid
// for the whole host, not just the page the solver opened. Without reuse,
// every page of a challenged site (the listing and each title page) costs a
// full solve and holds a pool session for seconds. With it, the plain client
// sends the clearance and the solver runs about once per host per TTL.

// clearanceMaxTTL caps how long a clearance is reused, below Cloudflare's
// usual 30 min and never past the cookie's own expiry.
const clearanceMaxTTL = 25 * time.Minute

// clearanceBackoff stops storing clearances for a host after one did not
// work for the plain client (for example the solver leaves through another
// IP, and cf_clearance is bound to it), so we don't pay a useless plain
// request before every solve.
const clearanceBackoff = 30 * time.Minute

// maxClearanceHosts bounds the store; hosts are the indexers' sites, so this
// is far above what is used.
const maxClearanceHosts = 256

type clearance struct {
	userAgent string
	cookies   []*http.Cookie
	expires   time.Time
}

type clearanceStore struct {
	mu      sync.Mutex
	now     func() time.Time
	byHost  map[string]clearance
	noReuse map[string]time.Time
}

func newClearanceStore() *clearanceStore {
	return &clearanceStore{
		now:     time.Now,
		byHost:  map[string]clearance{},
		noReuse: map[string]time.Time{},
	}
}

func clearanceHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// remember stores the solver's clearance for the host of rawURL. Only a
// solution with cf_clearance and a user agent is kept: other cookies alone
// don't get past the challenge.
func (s *clearanceStore) remember(rawURL string, sol solution) {
	host := clearanceHost(rawURL)
	if s == nil || host == "" || sol.UserAgent == "" {
		return
	}
	now := s.now()
	expires := now.Add(clearanceMaxTTL)
	var cookies []*http.Cookie
	found := false
	for _, c := range sol.Cookies {
		if c.Name == "" {
			continue
		}
		if c.Name == "cf_clearance" {
			found = true
			if c.Expiry > 0 {
				if exp := time.Unix(int64(c.Expiry), 0); exp.Before(expires) {
					expires = exp
				}
			}
		}
		cookies = append(cookies, &http.Cookie{Name: c.Name, Value: c.Value})
	}
	if !found || !expires.After(now) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if until, ok := s.noReuse[host]; ok {
		if now.Before(until) {
			return
		}
		delete(s.noReuse, host)
	}
	if _, ok := s.byHost[host]; !ok && len(s.byHost) >= maxClearanceHosts {
		s.pruneLocked(now)
		if len(s.byHost) >= maxClearanceHosts {
			return
		}
	}
	s.byHost[host] = clearance{userAgent: sol.UserAgent, cookies: cookies, expires: expires}
}

// lookup returns the live clearance for the host of rawURL.
func (s *clearanceStore) lookup(rawURL string) (clearance, bool) {
	host := clearanceHost(rawURL)
	if s == nil || host == "" {
		return clearance{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byHost[host]
	if !ok {
		return clearance{}, false
	}
	if !s.now().Before(c.expires) {
		delete(s.byHost, host)
		return clearance{}, false
	}
	return c, true
}

// rejected drops the clearance of the host of rawURL after the site still
// answered with a challenge, and stops storing new ones for a while.
func (s *clearanceStore) rejected(rawURL string) {
	host := clearanceHost(rawURL)
	if s == nil || host == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byHost, host)
	if len(s.noReuse) >= maxClearanceHosts {
		now := s.now()
		for h, until := range s.noReuse {
			if !now.Before(until) {
				delete(s.noReuse, h)
			}
		}
	}
	if len(s.noReuse) < maxClearanceHosts {
		s.noReuse[host] = s.now().Add(clearanceBackoff)
	}
}

func (s *clearanceStore) pruneLocked(now time.Time) {
	for h, c := range s.byHost {
		if !now.Before(c.expires) {
			delete(s.byHost, h)
		}
	}
}

// apply sends the clearance on req: the solver's user agent (cf_clearance is
// bound to it) and its cookies.
func (c clearance) apply(req *http.Request) {
	req.Header.Set("User-Agent", c.userAgent)
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}
}

// clearanceFor returns the solver clearance stored for the host of rawURL. It is
// safe on a nil FlareSolverr.
func (f *FlareSolverr) clearanceFor(rawURL string) (clearance, bool) {
	if f == nil {
		return clearance{}, false
	}
	return f.clearances.lookup(rawURL)
}

func (f *FlareSolverr) clearanceRejected(rawURL string) {
	if f == nil {
		return
	}
	f.clearances.rejected(rawURL)
}
