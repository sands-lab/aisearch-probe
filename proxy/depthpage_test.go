package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aisearch-probe/internal/probe"
)

func depthServer(t *testing.T, secret string) *staticServer {
	t.Helper()
	st := newStaticServer(t.TempDir(), "public, max-age=3600")
	st.pageCodeSecret = secret
	return st
}

const depthTestSecret = "test-depth-secret"

// Byte-exact: a request for N bytes returns exactly N bytes. The size is in the
// path, so nothing rides on a strippable query.
func TestDepthPageReturnsExactBytes(t *testing.T) {
	for _, n := range []int{64 << 10, 256 << 10, 1 << 20} {
		got := renderDepthPage(depthTestSecret, "dp-a", n)
		if len(got) != n {
			t.Errorf("renderDepthPage(_, _, %d) returned %d bytes, want exactly %d", n, len(got), n)
		}
	}
}

// Pure function of (secret, id, bytes): the same address twice is byte-identical
// (so the ETag is stable and revalidation works); two sizes differ.
func TestDepthPageIsPureFunctionOfAddress(t *testing.T) {
	if renderDepthPage(depthTestSecret, "dp-a", 65536) != renderDepthPage(depthTestSecret, "dp-a", 65536) {
		t.Error("same (id, size) produced different bytes — the ETag would not be stable")
	}
	if renderDepthPage(depthTestSecret, "dp-a", 65536) == renderDepthPage(depthTestSecret, "dp-a", 131072) {
		t.Error("two sizes produced identical bytes")
	}
}

// THE GUARD (DepthMarkersMatchThePage): every depth's unforgeable code sits at the
// byte offset it claims, and the printed offset matches. This pairs the shared
// marker spec (internal/probe) to the served page so they cannot drift — the
// deepest returned code is only a valid cut reading if each code truly sits where
// we think it does.
func TestDepthMarkersMatchThePage(t *testing.T) {
	const n = 256 << 10
	body := renderDepthPage(depthTestSecret, "dp-a", n)

	for _, pct := range probe.DepthPercents {
		code := probe.DepthCode(depthTestSecret, "dp-a", pct, probe.DepthPageRevision)
		if code == "" {
			t.Fatalf("depth %d: empty code", pct)
		}
		off := probe.DepthOffset(pct, n)
		block := probe.DepthMarkerPrefix + code
		if off+len(block) > len(body) {
			t.Fatalf("depth %d: marker runs past the end (off %d + %d > %d)", pct, off, len(block), len(body))
		}
		if got := body[off : off+len(block)]; got != block {
			t.Errorf("depth %d: marker not at byte %d — found %q, want %q", pct, off, got, block)
		}
		if !strings.Contains(body, fmt.Sprintf("offset=%d", off)) {
			t.Errorf("depth %d: page does not print offset=%d next to its code", pct, off)
		}
	}
	// The top marker (0%) must sit at byte 0, so Part B can warm on it.
	if probe.DepthOffset(0, n) != 0 {
		t.Errorf("depth 0%% offset = %d, want 0", probe.DepthOffset(0, n))
	}
}

// No collision: the marker prefix appears EXACTLY once per depth and never in the
// filler. This is the structural guarantee that a returned code was really read,
// not spuriously matched against filler.
func TestDepthPageMarkerPrefixCountEqualsDepths(t *testing.T) {
	body := renderDepthPage(depthTestSecret, "dp-a", 128<<10)
	got := strings.Count(body, probe.DepthMarkerPrefix)
	if got != len(probe.DepthPercents) {
		t.Errorf("found %d %q occurrences, want exactly %d (one per depth; filler must never spell it)",
			got, probe.DepthMarkerPrefix, len(probe.DepthPercents))
	}
}

// The codes are unforgeable: distinct per depth, and NOT derivable without the
// server secret (a wrong secret yields different codes). This is why a fixed
// pattern was rejected — a model could infer a deeper predictable code and inflate
// the measured cut.
func TestDepthPageCodesAreUnforgeable(t *testing.T) {
	body := renderDepthPage(depthTestSecret, "dp-a", 128<<10)
	seen := map[string]bool{}
	for _, pct := range probe.DepthPercents {
		right := probe.DepthCode(depthTestSecret, "dp-a", pct, probe.DepthPageRevision)
		wrong := probe.DepthCode("not-the-secret", "dp-a", pct, probe.DepthPageRevision)
		if right == wrong {
			t.Errorf("depth %d: code is the same with the wrong secret — it would be forgeable", pct)
		}
		if !strings.Contains(body, right) {
			t.Errorf("depth %d: real code missing from the page", pct)
		}
		if strings.Contains(body, wrong) {
			t.Errorf("depth %d: page contains a code derived from the WRONG secret", pct)
		}
		if seen[right] {
			t.Errorf("depth %d: code collides with a shallower depth", pct)
		}
		seen[right] = true
	}
}

// A malformed address must 404, never read as a real page.
func TestDepthPageMalformedIs404(t *testing.T) {
	srv, _, _ := newTestProxy(t, depthServer(t, depthTestSecret), "")
	for _, p := range []string{
		"/depthpage/",          // nothing
		"/depthpage/dp-a",      // no size segment
		"/depthpage/dp-a/",     // empty size
		"/depthpage//65536",    // empty id
		"/depthpage/dp-a/abc",  // non-numeric size
		"/depthpage/dp-a/0",    // zero
		"/depthpage/dp-a/-1",   // negative
		"/depthpage/dp-a/64/9", // extra segment
	} {
		resp := get(t, srv.URL+p, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s returned %d, want 404", p, resp.StatusCode)
		}
	}
}

// A size too small to hold the markers without overlap is refused, not served
// corrupted; an absurd size is refused too.
func TestDepthPageBadSizesAreRefused(t *testing.T) {
	srv, _, _ := newTestProxy(t, depthServer(t, depthTestSecret), "")
	for _, p := range []string{"/depthpage/dp-a/64", "/depthpage/dp-a/999999999"} {
		resp := get(t, srv.URL+p, nil)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s was served (status %d); it must be refused", p, resp.StatusCode)
		}
	}
}

// The frozen filler corpus must never contain the marker prefix, or the filler
// could fake a marker. This proves the no-collision guarantee on the shipped
// corpus itself (the generator sanitises for it; this locks it in the binary).
func TestDepthCorpusHasNoReservedPrefix(t *testing.T) {
	if len(depthCorpus) < 1024 {
		t.Fatalf("embedded corpus is suspiciously small (%d bytes) — did depthcorpus.txt embed?", len(depthCorpus))
	}
	if strings.Contains(depthCorpus, probe.DepthMarkerPrefix) {
		t.Errorf("filler corpus contains the reserved prefix %q — a marker could be faked by filler", probe.DepthMarkerPrefix)
	}
}
