package probe

import (
	"strings"
	"testing"
)

// The two codes on a coded page must be UNFORGEABLE (depend on the server secret,
// so they can't be produced from the id in the URL), DISTINCT per field and per
// page, STABLE (so we recompute to verify), and VERSIONED (so a mutated page's
// code differs). No secret must produce no code, so the route can refuse rather
// than emit a guessable one.
func TestCpageCodeProperties(t *testing.T) {
	const (
		s1 = "secret-one-abcdef0123"
		s2 = "secret-two-abcdef0123"
		id = "cr05-a9f3"
	)
	ref := CpageCode(s1, id, CpageFieldRef, 1)
	rec := CpageCode(s1, id, CpageFieldRec, 1)

	if ref == "" || rec == "" {
		t.Fatal("codes must be non-empty when a secret is set")
	}
	if ref == rec {
		t.Error("the two field codes on one page must differ (ref vs rec)")
	}
	if ref != CpageCode(s1, id, CpageFieldRef, 1) {
		t.Error("a code must be stable for the same inputs, so we can recompute to verify")
	}
	if ref == CpageCode(s2, id, CpageFieldRef, 1) {
		t.Error("a code must depend on the secret — otherwise it is forgeable from the id alone")
	}
	if ref == CpageCode(s1, "cr06-a9f3", CpageFieldRef, 1) {
		t.Error("a code must depend on the id — every page needs a different code")
	}
	if ref == CpageCode(s1, id, CpageFieldRef, 2) {
		t.Error("a code must depend on the revision — v1 and v2 differ (mutation/staleness)")
	}
	if CpageCode("", id, CpageFieldRef, 1) != "" {
		t.Error("no secret must yield no code, so the route can refuse instead of emitting a guessable one")
	}
}

// The subject register must be non-trivial, its phrases unique, and each must read
// like a named thing (has a space, not a stub) — a gibberish phrase makes a blank
// answer meaningless.
func TestCpageSubjectRegisterIsUniqueAndPlausible(t *testing.T) {
	subs := CpageSubjects()
	if len(subs) < 4 {
		t.Fatalf("register too small (%d) to run trials without collisions", len(subs))
	}
	seen := map[string]bool{}
	for i, p := range subs {
		if len(strings.TrimSpace(p)) < 10 || !strings.Contains(p, " ") {
			t.Errorf("phrase %d does not read like a named thing: %q", i, p)
		}
		if seen[p] {
			t.Errorf("duplicate phrase in register: %q", p)
		}
		seen[p] = true
	}
}

// An id carries the register index (cr<NN>-<nonce>), so the driver picks the
// phrase and the server serves that exact one. Malformed or out-of-range ids must
// not resolve.
func TestCpageSubjectForID(t *testing.T) {
	subs := CpageSubjects()
	got, ok := CpageSubjectForID("cr05-a9f3")
	if !ok {
		t.Fatal("cr05-a9f3 should resolve to a subject")
	}
	if got != subs[5] {
		t.Errorf("cr05 -> %q, want register[5] = %q", got, subs[5])
	}
	if g0, ok := CpageSubjectForID("cr0-x"); !ok || g0 != subs[0] {
		t.Errorf("cr0 -> %q,%v want register[0] = %q", g0, ok, subs[0])
	}
	for _, bad := range []string{"", "nope", "cr-x", "crab-x", "cr9999-x", "page05-x", "cr05abc"} {
		if _, ok := CpageSubjectForID(bad); ok {
			t.Errorf("%q must not resolve to a subject", bad)
		}
	}
}
