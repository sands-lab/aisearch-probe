package probe

import (
	"fmt"
	"strings"
	"testing"
)

// DepthOffset turns a percentage into a byte offset: 0% is byte 0 (so the top
// marker can be warmed on), 100% is the end, and it rounds in between.
func TestDepthOffset(t *testing.T) {
	const n = 65536
	if got := DepthOffset(0, n); got != 0 {
		t.Errorf("DepthOffset(0, %d) = %d, want 0", n, got)
	}
	if got := DepthOffset(100, n); got != n {
		t.Errorf("DepthOffset(100, %d) = %d, want %d", n, got, n)
	}
	if got := DepthOffset(50, n); got != n/2 {
		t.Errorf("DepthOffset(50, %d) = %d, want %d", n, got, n/2)
	}
	// Deeper percent => larger offset, strictly, so markers never share a spot.
	prev := -1
	for _, pct := range DepthPercents {
		off := DepthOffset(pct, n)
		if off <= prev {
			t.Errorf("offsets not strictly increasing at pct %d: %d after %d", pct, off, prev)
		}
		prev = off
	}
}

// DepthCode is HMAC-derived: fixed length, distinct per depth, empty without a
// secret (so the route can refuse), and NOT reproducible without the secret.
func TestDepthCode(t *testing.T) {
	if DepthCode("", "dp-a", 25, DepthPageRevision) != "" {
		t.Error("no secret must yield an empty code so the route can refuse")
	}
	seen := map[string]bool{}
	for _, pct := range DepthPercents {
		c := DepthCode("s3cr3t", "dp-a", pct, DepthPageRevision)
		if c == "" {
			t.Fatalf("depth %d: empty code with a secret set", pct)
		}
		if seen[c] {
			t.Errorf("depth %d: code collides with a shallower depth", pct)
		}
		seen[c] = true
		if DepthCode("other", "dp-a", pct, DepthPageRevision) == c {
			t.Errorf("depth %d: code reproducible under a different secret — forgeable", pct)
		}
	}
	// Distinct pages never share a code (id is keyed in).
	if DepthCode("s3cr3t", "dp-a", 50, DepthPageRevision) == DepthCode("s3cr3t", "dp-b", 50, DepthPageRevision) {
		t.Error("two page ids produced the same code")
	}
}

// DeepestDepth reads the deepest marker whose code appears in an answer — the cut.
// It returns -1 when no code appears (a distinct finding: the model quoted none),
// and it must never be fooled by a code the page never carried.
func TestDeepestDepth(t *testing.T) {
	const secret, id = "s3cr3t", "dp-a"
	code := func(pct int) string { return DepthCode(secret, id, pct, DepthPageRevision) }

	// The model quoted codes up to 50% (and some prose around them).
	ans := fmt.Sprintf("I can see %s near the top, then %s, and %s halfway down.",
		code(0), code(25), code(50))
	if got := DeepestDepth(ans, secret, id, DepthPageRevision); got != 50 {
		t.Errorf("deepest = %d, want 50", got)
	}

	// No code at all -> -1, not 0.
	if got := DeepestDepth("the tool returned an error; I could not read the page", secret, id, DepthPageRevision); got != -1 {
		t.Errorf("deepest with no code = %d, want -1", got)
	}

	// A code from ANOTHER page/secret must not count.
	forged := DepthCode("wrong", id, 95, DepthPageRevision)
	if got := DeepestDepth("here is a code "+forged, secret, id, DepthPageRevision); got != -1 {
		t.Errorf("deepest from a foreign code = %d, want -1", got)
	}

	// The deepest quoted wins even if listed out of order.
	both := code(95) + " and also " + code(25)
	if got := DeepestDepth(both, secret, id, DepthPageRevision); got != 95 {
		t.Errorf("deepest out-of-order = %d, want 95", got)
	}
}

// The marker prefix must stand out from both the filler (lowercase 'x' + newlines)
// and the codes (lowercase hex): it carries an uppercase letter, which neither can
// produce, so filler can never spell a marker and a code can never be mistaken for
// the prefix.
func TestDepthMarkerPrefixStandsOut(t *testing.T) {
	if DepthMarkerPrefix == "" {
		t.Fatal("empty marker prefix")
	}
	if strings.ToLower(DepthMarkerPrefix) == DepthMarkerPrefix {
		t.Errorf("marker prefix %q has no uppercase letter — filler/codes (all lowercase) could collide with it", DepthMarkerPrefix)
	}
}
