package limiter

import (
	"testing"
	"time"
)

func TestAllowConsumesInitialToken(t *testing.T) {
	now := time.Unix(100, 0)
	l := NewWithClock(func() time.Time { return now })
	if !l.Allow("k", 2) || !l.Allow("k", 2) {
		t.Fatal("initial burst should contain two requests")
	}
	if l.Allow("k", 2) {
		t.Fatal("third immediate request must be rate limited")
	}
}

func TestFractionalRateRefillsToOneToken(t *testing.T) {
	now := time.Unix(100, 0)
	l := NewWithClock(func() time.Time { return now })
	if !l.Allow("k", 0.5) {
		t.Fatal("first request should pass")
	}
	if l.Allow("k", 0.5) {
		t.Fatal("second immediate request must be rate limited")
	}
	now = now.Add(2 * time.Second)
	if !l.Allow("k", 0.5) {
		t.Fatal("0.5 QPS should refill one token in two seconds")
	}
}

func TestSetRateSettlesBeforeChangingRate(t *testing.T) {
	now := time.Unix(100, 0)
	l := NewWithClock(func() time.Time { return now })
	if !l.Allow("k", 2) {
		t.Fatal("first request should pass")
	}
	now = now.Add(500 * time.Millisecond)
	l.SetRate("k", 4)
	if !l.Allow("k", 4) {
		t.Fatal("settled tokens should be available after rate update")
	}
}
