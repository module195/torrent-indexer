package goscrape

import (
	"testing"
	"time"
)

func TestResolveTrackerKeepsIPs(t *testing.T) {
	addr, err := resolveTracker("93.158.213.92:1337", time.Second)
	if err != nil || addr != "93.158.213.92:1337" {
		t.Fatalf("got %q, %v", addr, err)
	}
}

func TestResolveTrackerUsesCache(t *testing.T) {
	trackerAddrs.Store("tracker.example.invalid:6969", trackerAddr{addr: "10.0.0.1:6969", expires: time.Now().Add(time.Minute)})
	addr, err := resolveTracker("tracker.example.invalid:6969", time.Second)
	if err != nil || addr != "10.0.0.1:6969" {
		t.Fatalf("got %q, %v", addr, err)
	}
}

func TestResolveTrackerBadHostport(t *testing.T) {
	if _, err := resolveTracker("no-port", time.Second); err == nil {
		t.Fatal("expected an error")
	}
}
