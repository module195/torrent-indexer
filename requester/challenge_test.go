package requester

import "testing"

// The challenge check ignores case, as the (?i) regex it replaced did.
func TestHasChallange(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"<title>Just a moment...</title>", true},
		{"<TITLE>JUST A MOMENT</TITLE>", true},
		{`<form id="CF-CHL-BYPASS">`, true},
		{"site Under Attack mode", true},
		{"<html><body>Lupin S01E01 1080p</body></html>", false},
		{"just a mo ment", false},
		{"", false},
	}
	for _, c := range cases {
		if got := hasChallange([]byte(c.body)); got != c.want {
			t.Errorf("hasChallange(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}
