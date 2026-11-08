package main

import (
	"strings"
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

// buildTTLPages is the pure plan: one fresh page per provider per replicate, each
// with a unique id that carries the caller's identity and rides in the /page/ path
// (so the fetch join keys on something no query-strip can move).
func TestBuildTTLPages(t *testing.T) {
	provs := []Caller{
		{Name: "openai", Model: "gpt-5-mini"},
		{Name: "openai", Model: "gpt-5.5"},
	}
	pages := buildTTLPages(defaultTTLArms(), provs, "https://probe.example.org/", "run123", 3, "", "RCP-2026-08")
	if len(pages) != 6 {
		t.Fatalf("want 6 pages (1 arm x 2 providers x 3 reps), got %d", len(pages))
	}
	seen := map[string]bool{}
	for _, pg := range pages {
		if seen[pg.PageID] {
			t.Errorf("duplicate page id %q — two canaries could satisfy each other", pg.PageID)
		}
		seen[pg.PageID] = true
		if !strings.Contains(pg.PageID, "run123") {
			t.Errorf("page id %q missing run id", pg.PageID)
		}
		wantURL := "https://probe.example.org/page/" + pg.PageID
		if pg.URL != wantURL {
			t.Errorf("url = %q, want %q", pg.URL, wantURL)
		}
		if !strings.Contains(pg.URL, "/page/") {
			t.Errorf("url %q not on the /page/ route", pg.URL)
		}
	}
	// The two models must not collide on any id, or one model's fetch reads as the
	// other's silence — the exact trap Tag() exists to prevent.
	if len(seen) != 6 {
		t.Errorf("want 6 distinct ids, got %d", len(seen))
	}
}

// The depth eviction sweep must build one arm per size on the REAL-content
// /depthpage/ route (never /sizepage/), with the byte size in the path so the
// fetch join and the size both ride something no query-strip can move.
func TestDepthSweepArms(t *testing.T) {
	arms := depthSweepArms([]int{4096, 65536, 1048576})
	if len(arms) != 3 {
		t.Fatalf("want 3 arms, got %d", len(arms))
	}
	provs := []Caller{{Name: "openai", Model: "gpt-5-mini"}}
	pages := buildTTLPages(arms, provs, "https://probe.example.org", "run1", 6, "", "RCP-2026-08")
	if len(pages) != 18 {
		t.Fatalf("want 18 pages (3 sizes x 6 reps), got %d", len(pages))
	}
	wantSuffix := map[string]string{"depth-4k": "/4096", "depth-64k": "/65536", "depth-1m": "/1048576"}
	seen := map[string]bool{}
	for _, pg := range pages {
		if seen[pg.PageID] {
			t.Errorf("duplicate page id %q", pg.PageID)
		}
		seen[pg.PageID] = true
		if !strings.Contains(pg.URL, "/depthpage/") {
			t.Errorf("arm %q url %q not on /depthpage/", pg.Arm, pg.URL)
		}
		if strings.Contains(pg.URL, "/sizepage/") {
			t.Errorf("arm %q must NOT use /sizepage/ (stripped filler): %q", pg.Arm, pg.URL)
		}
		suf, ok := wantSuffix[pg.Arm]
		if !ok {
			t.Errorf("unexpected arm %q", pg.Arm)
			continue
		}
		if !strings.HasSuffix(pg.URL, suf) {
			t.Errorf("arm %q url %q must end in %q", pg.Arm, pg.URL, suf)
		}
	}
}

// The size list parser must reject junk rather than silently sweep the wrong sizes.
func TestParseByteSizes(t *testing.T) {
	got, err := parseByteSizes("4096, 65536 ,1048576")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 || got[0] != 4096 || got[2] != 1048576 {
		t.Errorf("parsed %v, want [4096 65536 1048576]", got)
	}
	for _, bad := range []string{"", "0", "-1", "4096,abc", "  "} {
		if _, err := parseByteSizes(bad); err == nil {
			t.Errorf("parseByteSizes(%q) should have errored", bad)
		}
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
