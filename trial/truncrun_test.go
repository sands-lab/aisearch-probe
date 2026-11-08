package main

import (
	"strconv"
	"strings"
	"testing"

	"aisearch-probe/internal/probe"
)

// The plan is pure (no network): one scan per (provider, size), each on its own
// page, with the size in the path.
func TestTruncActionsExpand(t *testing.T) {
	provs := []Caller{{Name: "or:openai", Model: "openai/gpt-5-mini"}}
	sizes := truncLadder()
	acts := buildTruncActions(provs, "https://h", "run1", sizes, 1, "")
	ids := map[string]bool{}
	for _, a := range acts {
		if a.Step != "scan" {
			t.Errorf("%s: step %q, want only scans", a.ID, a.Step)
		}
		if strings.Contains(a.URL, "?") {
			t.Errorf("%s uses a query — a provider could strip it", a.URL)
		}
		want := "/depth/html/" + a.PageID + "/" + strconv.Itoa(a.Bytes)
		if !strings.Contains(a.URL, want) {
			t.Errorf("URL %q does not carry the size in the path (%q)", a.URL, want)
		}
		ids[a.PageID] = true
	}
	if n := len(provs) * len(sizes); len(acts) != n || len(ids) != n {
		t.Errorf("got %d actions on %d pages, want %d of each", len(acts), len(ids), n)
	}
}

// reps multiplies the plan: each (provider, size) gets `reps` independent pages,
// so a noisy tool is sampled n=reps rather than once.
func TestTruncActionsReps(t *testing.T) {
	provs := []Caller{{Name: "openai", Model: "gpt-5-mini"}}
	sizes := []int{64 << 10, 128 << 10}
	reps := 3
	acts := buildTruncActions(provs, "https://h", "run1", sizes, reps, "")
	ids := map[string]bool{}
	for _, a := range acts {
		ids[a.PageID] = true
	}
	if len(acts) != len(sizes)*reps || len(ids) != len(sizes)*reps {
		t.Errorf("got %d scans on %d pages, want %d (size*reps)", len(acts), len(ids), len(sizes)*reps)
	}
	// reps <= 0 behaves as a single rep, never zero actions.
	if got := buildTruncActions(provs, "https://h", "run1", sizes, 0, ""); len(got) != len(sizes) {
		t.Error("reps<=0 should behave as reps=1")
	}
}

// The ladder is the octave range the handover fixed: 64 KiB..4 MiB, ascending.
func TestTruncLadderRange(t *testing.T) {
	sizes := truncLadder()
	if sizes[0] != 64<<10 {
		t.Errorf("ladder starts at %d, want 64 KiB", sizes[0])
	}
	if sizes[len(sizes)-1] != 4<<20 {
		t.Errorf("ladder ends at %d, want 4 MiB", sizes[len(sizes)-1])
	}
	for i := 1; i < len(sizes); i++ {
		if sizes[i] <= sizes[i-1] {
			t.Errorf("ladder not ascending at %d", i)
		}
	}
}

// capLadder drops rungs above a byte cap so a run can skip the expensive multi-MB
// pages; a zero/negative cap leaves the ladder whole.
func TestCapLadder(t *testing.T) {
	full := truncLadder()
	if got := capLadder(full, 0); len(got) != len(full) {
		t.Errorf("cap 0 changed the ladder: %v", got)
	}
	capped := capLadder(full, 2<<20) // 2 MiB
	for _, s := range capped {
		if s > 2<<20 {
			t.Errorf("capped ladder kept %d, above the 2 MiB cap", s)
		}
	}
	if len(capped) == 0 || capped[len(capped)-1] != 2<<20 {
		t.Errorf("cap 2 MiB should keep rungs up to and including 2 MiB, got %v", capped)
	}
	if len(capped) >= len(full) {
		t.Errorf("cap 2 MiB should have dropped the 4 MiB rung, got %v", capped)
	}
	// A cap below every rung still yields at least the smallest, so a run is never empty.
	if got := capLadder(full, 1); len(got) != 1 || got[0] != full[0] {
		t.Errorf("a tiny cap should keep just the smallest rung, got %v", got)
	}
}

// parseSizes turns an explicit "2k,8k,4m" list into bytes, so a run can target the
// small end (measuring xAI's web_search extract window) or any custom ladder.
func TestParseSizes(t *testing.T) {
	got, err := parseSizes("2k, 8k ,4m,1024")
	if err != nil {
		t.Fatalf("parseSizes error: %v", err)
	}
	want := []int{2048, 8192, 4 << 20, 1024}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %d, want %d", i, got[i], want[i])
		}
	}
	if _, err := parseSizes("2k,nope"); err == nil {
		t.Error("a bad token should error")
	}
	if _, err := parseSizes(""); err == nil {
		t.Error("an empty list should error")
	}
}

// The emit writes a schema-valid truncprobe record that carries the model's own
// answer, joins on the page id, and records the deepest depth actually quoted as a
// FACT in extra (the cut for this fetch), plus the size and the part.
func TestTruncEmitCarriesDepthAndAnswer(t *testing.T) {
	dir := t.TempDir()
	old := resultsDir
	resultsDir = dir
	defer func() { resultsDir = old }()

	const secret = "s3cr3t"
	id := "or:openai-run1-a-256k"
	code := func(pct int) string { return probe.DepthCode(secret, id, pct, probe.DepthPageRevision) }
	ans := "I can quote " + code(0) + ", " + code(25) + ", and " + code(50) + " but nothing deeper."

	a := &TruncAction{
		ID: id + "#0", Provider: "or:openai", Model: "openai/gpt-5-mini",
		PageID: id, URL: "https://h/depthpage/" + id + "/262144", Bytes: 262144,
		Part: "A", Step: "scan", Seq: 0, Answer: ans, FiredUnix: 1,
	}
	truncEmit("run1", a, secret, true)

	got, err := probe.ReadResultsDir(dir)
	if err != nil {
		t.Fatalf("ReadResultsDir: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	r := got[0]
	if err := r.Validate(); err != nil {
		t.Errorf("record invalid: %v", err)
	}
	if r.Experiment != "truncprobe" {
		t.Errorf("experiment = %q, want truncprobe", r.Experiment)
	}
	if r.AnswerText != ans {
		t.Errorf("answer not carried: %q", r.AnswerText)
	}
	if r.Token != id {
		t.Errorf("token = %q, want the page id %q", r.Token, id)
	}
	if dd, ok := r.Extra["deepest_depth"].(float64); !ok || int(dd) != 50 {
		t.Errorf("extra.deepest_depth = %v, want 50", r.Extra["deepest_depth"])
	}
	if r.Extra["part"] != "A" {
		t.Errorf("extra.part = %v, want A", r.Extra["part"])
	}
	if sb, ok := r.Extra["size_bytes"].(float64); !ok || int(sb) != 262144 {
		t.Errorf("extra.size_bytes = %v, want 262144", r.Extra["size_bytes"])
	}
}
