package main

import (
	"testing"
	"time"
)

// The gap ladder is the whole schedule, so it is pinned before any network: it
// must start at firstPoll, step by cadence, and never pass window.
func TestTTLPollGaps(t *testing.T) {
	gaps := ttlPollGaps(9*time.Minute, 3*time.Minute, 45*time.Minute)
	if len(gaps) != 13 {
		t.Fatalf("want 13 gaps for 9..45 by 3, got %d: %v", len(gaps), gaps)
	}
	if gaps[0] != 9*time.Minute {
		t.Errorf("first gap = %s, want 9m", gaps[0])
	}
	if gaps[len(gaps)-1] != 45*time.Minute {
		t.Errorf("last gap = %s, want 45m", gaps[len(gaps)-1])
	}
	for i := 1; i < len(gaps); i++ {
		if gaps[i]-gaps[i-1] != 3*time.Minute {
			t.Errorf("gap %d->%d spacing = %s, want 3m", i-1, i, gaps[i]-gaps[i-1])
		}
	}
}

// A window that is not an exact multiple of the cadence must stop at the last gap
// that still fits, never step past it.
func TestTTLPollGapsNeverPastWindow(t *testing.T) {
	gaps := ttlPollGaps(10*time.Minute, 3*time.Minute, 45*time.Minute)
	last := gaps[len(gaps)-1]
	if last > 45*time.Minute {
		t.Fatalf("last gap %s exceeds window 45m", last)
	}
	if last != 43*time.Minute { // 10,13,...,43 ; next (46) would exceed 45
		t.Errorf("last gap = %s, want 43m", last)
	}
}

// The back-off poll schedule parser must keep the gaps ascending and above the 150s
// fetch-attribution floor — a mis-ordered or too-short gap would silently break the
// TTL join, so it is an error, not a reorder.
func TestParseDurations(t *testing.T) {
	got, err := parseDurations("15m, 30m ,1h,2h,4h,8h")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("gap %d = %s, want %s", i, got[i], want[i])
		}
	}
	for _, bad := range []string{
		"",        // empty
		"  ",      // blank
		"15m,abc", // junk
		"60s",     // below the 150s floor
		"30m,15m", // not ascending
		"15m,15m", // not strictly ascending
	} {
		if _, err := parseDurations(bad); err == nil {
			t.Errorf("parseDurations(%q) should have errored", bad)
		}
	}
}
