package requester

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
)

// ErrBlockedDestination is returned for a page URL the requester refuses to
// fetch: a scheme other than http(s), or a host that is (or resolves to) a
// loopback, private, link-local or otherwise non-public address.
//
// Page URLs come from scraped HTML (a post link, a pagination link): the
// site, or whoever controls a link on it, chooses them. Without this check
// such a link could point the indexer, or FlareSolverr behind it, at Redis,
// Meilisearch, the cloud metadata service or anything else on the internal
// network.
var ErrBlockedDestination = errors.New("destination not allowed")

// maxRedirects matches net/http's default limit.
const maxRedirects = 10

// extraBlocked lists non-public ranges that netip's Is* helpers don't cover.
var extraBlocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network"
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved, includes broadcast
}

// isPublicAddr reports whether a page may be fetched from addr.
func isPublicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() {
		return false
	}
	for _, p := range extraBlocked {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// dialControl runs after DNS resolution, on the address actually dialed, so
// a hostname that resolves to a public address at check time and to an
// internal one at dial time (DNS rebinding) is still refused. It also covers
// every redirect hop, since each one dials through the same transport.
func (i *Requster) dialControl(_, address string, _ syscall.RawConn) error {
	if i.allowPrivateHosts.Load() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrBlockedDestination, address)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !isPublicAddr(addr) {
		return fmt.Errorf("%w: %s", ErrBlockedDestination, address)
	}
	return nil
}

// checkRedirect keeps net/http's redirect limit and refuses a hop to a
// scheme other than http(s). The host of each hop is checked by dialControl.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("%w: redirect to scheme %q", ErrBlockedDestination, req.URL.Scheme)
	}
	return nil
}

// checkDestination is run on a page URL before any fetch. dialControl alone
// protects the plain client, but FlareSolverr fetches the URL from its own
// network, where nothing on our side sees the dialed address: the host is
// resolved here and refused if any of its addresses is not public.
func (i *Requster) checkDestination(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBlockedDestination, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q", ErrBlockedDestination, u.Scheme)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return fmt.Errorf("%w: no host in %q", ErrBlockedDestination, rawURL)
	}
	if i.allowPrivateHosts.Load() {
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return fmt.Errorf("%w: %s", ErrBlockedDestination, host)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !isPublicAddr(addr) {
			return fmt.Errorf("%w: %s", ErrBlockedDestination, host)
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		// a name we can't resolve may still resolve inside FlareSolverr's
		// network (a compose service name, say), so it is not fetched
		return fmt.Errorf("%w: resolving %s: %v", ErrBlockedDestination, host, err)
	}
	for _, addr := range addrs {
		if !isPublicAddr(addr) {
			return fmt.Errorf("%w: %s resolves to %s", ErrBlockedDestination, host, addr)
		}
	}
	return nil
}

// AllowPrivateHosts turns the destination checks off, for a deployment that
// scrapes a site on its own network and for tests against a local server.
// REQUESTER_ALLOW_PRIVATE_HOSTS=true does the same from the environment.
func (i *Requster) AllowPrivateHosts() {
	i.allowPrivateHosts.Store(true)
}
