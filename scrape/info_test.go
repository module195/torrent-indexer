package goscrape

import (
	"testing"
	"time"
)

func TestPeersTTL(t *testing.T) {
	cases := []struct {
		seeders int
		want    time.Duration
	}{
		{0, noPeersTTL},
		{1, 2 * time.Hour},
		{10, 2 * time.Hour},
		{11, 12 * time.Hour},
		{5000, 12 * time.Hour},
	}
	for _, tc := range cases {
		if got := peersTTL(tc.seeders); got != tc.want {
			t.Errorf("peersTTL(%d) = %v, want %v", tc.seeders, got, tc.want)
		}
	}
}
