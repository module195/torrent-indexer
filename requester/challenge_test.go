package requester

import (
	"os"
	"strings"
	"testing"
)

// The challenge check looks only at the page's first <title>, ignoring case,
// and needs the whole title to be a known challenge title.
func TestHasChallange(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"<title>Just a moment...</title>", true},
		{"<TITLE>JUST A MOMENT</TITLE>", true},
		{"<html><head><title>Just a moment&hellip;</title></head></html>", true},
		{"<head><title>Attention Required! | Cloudflare</title>", true},
		{"<title>DDoS-Guard</title>", true},
		{"<html><body>Lupin S01E01 1080p</body></html>", false},
		{"just a mo ment", false},
		{"", false},
		// a search page echoing q must not look like a challenge
		{`<title>Busca por &quot;just a moment&quot; – Catálogo</title><meta name="description" content="just a moment">`, false},
		{`<title>Resultados</title><body>under attack cf-chl-bypass /cdn-cgi/challenge-platform/</body>`, false},
		{`<title>Busca</title><script type="application/ld+json">{"name":"<title>Just a moment...</title>"}</script>`, false},
	}
	for _, c := range cases {
		if got := hasChallange([]byte(c.body)); got != c.want {
			t.Errorf("hasChallange(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}

// The saved rede_torrent search page, with the query swapped for each old
// marker, must not read as a challenge: it used to fail the indexer and count
// against its health in /sources?v=2.
func TestHasChallangeIgnoresEchoedQuery(t *testing.T) {
	page, err := os.ReadFile("../api/testdata/rede_torrent/busca-homem.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"just a moment", "Under Attack", "cf-chl-bypass", "Just a moment..."} {
		echoed := strings.ReplaceAll(string(page), "Homem", q)
		if hasChallange([]byte(echoed)) {
			t.Errorf("page echoing q=%q reads as a challenge", q)
		}
	}
}
