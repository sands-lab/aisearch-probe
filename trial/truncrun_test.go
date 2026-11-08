package main

import (
	"strconv"
	"strings"
	"testing"

	"aisearch-probe/internal/probe"
)

// The plan is pure (no network): Part A is one scan per (provider, size); Part B
// is a warm+reask pair per (provider, size) on ITS OWN page, warm on the top marker
// and reask on the bottom, so a Part A fetch can never satisfy a Part B question.
func TestTruncActionsExpand(t *testing.T) {
	provs := []Caller{{Name: "or:openai", Model: "openai/gpt-5-mini"}}
	sizes := truncLadder()
	acts := buildTruncActions(provs, "https://h", "run1", sizes, 1, "", false)

	var scans, warms, reasks int
	byPage := map[string][]*TruncAction{}
	for _, a := range acts {
		byPage[a.PageID] = append(byPage[a.PageID], a)
		switch a.Step {
		case "scan":
			scans++
		case "warm":
			warms++
		case "reask":
			reasks++
		}
		if strings.Contains(a.URL, "?") {
			t.Errorf("%s uses a query — a provider could strip it", a.URL)
		}
		want := "/depthpage/" + a.PageID + "/" + strconv.Itoa(a.Bytes)
		if !strings.Contains(a.URL, want) {
			t.Errorf("URL %q does not carry the size in the path (%q)", a.URL, want)
		}
	}

	n := len(provs) * len(sizes)
	if scans != n {
		t.Errorf("got %d Part A scans, want %d (one per provider*size)", scans, n)
	}
	if warms != n || reasks != n {
		t.Errorf("got %d warms / %d reasks, want %d each (Part B pair per provider*size)", warms, reasks, n)
	}

	// Every Part B page is exactly a warm+reask pair; warm targets the top, reask
	// the bottom (deepest depth). Part A pages are single scans.
	deepest := probe.DepthPercents[len(probe.DepthPercents)-1]
	for id, pair := range byPage {
		if pair[0].Part == "A" {
			if len(pair) != 1 || pair[0].Step != "scan" {
				t.Errorf("Part A page %s is not a single scan: %+v", id, pair)
			}
			continue
		}
		if len(pair) != 2 {
			t.Fatalf("Part B page %s has %d actions, want warm+reask", id, len(pair))
		}
		var warm, reask *TruncAction
		for _, a := range pair {
			switch a.Step {
			case "warm":
				warm = a
			case "reask":
				reask = a
			}
		}
		if warm == nil || reask == nil {
			t.Fatalf("Part B page %s missing warm or reask: %+v", id, pair)
		}
		if warm.URL != reask.URL || warm.Bytes != reask.Bytes {
			t.Errorf("Part B page %s: warm and reask must be the SAME address", id)
		}
		if warm.TargetPct != 0 {
			t.Errorf("Part B page %s: warm targets %d%%, want the top (0%%)", id, warm.TargetPct)
		}
		if reask.TargetPct != deepest {
			t.Errorf("Part B page %s: reask targets %d%%, want the bottom (%d%%)", id, reask.TargetPct, deepest)
		}
	}

	// Part A and Part B never share a page id.
	for id, pair := range byPage {
		parts := map[string]bool{}
		for _, a := range pair {
			parts[a.Part] = true
		}
		if len(parts) != 1 {
			t.Errorf("page %s mixes Part A and Part B", id)
		}
	}
}

// reps multiplies the plan: each (provider, size) gets `reps` independent Part A
// pages and `reps` independent Part B pairs, with distinct page ids (fresh pages),
// so a noisy tool is sampled n=reps rather than once.
func TestTruncActionsReps(t *testing.T) {
	provs := []Caller{{Name: "openai", Model: "gpt-5-mini"}}
	sizes := []int{64 << 10, 128 << 10}
	reps := 3
	acts := buildTruncActions(provs, "https://h", "run1", sizes, reps, "", false)

	var scans int
	ids := map[string]bool{}
	for _, a := range acts {
		if a.Step == "scan" {
			scans++
		}
		ids[a.PageID] = true
	}
	if scans != len(sizes)*reps {
		t.Errorf("got %d scans, want %d (size*reps)", scans, len(sizes)*reps)
	}
	// Distinct pages: reps * sizes for Part A + reps * sizes for Part B.
	if len(ids) != 2*len(sizes)*reps {
		t.Errorf("got %d distinct page ids, want %d (fresh page per rep)", len(ids), 2*len(sizes)*reps)
	}
	// reps <= 0 behaves as a single rep, never zero actions.
	if got := buildTruncActions(provs, "https://h", "run1", sizes, 0, "", false); len(got) != len(buildTruncActions(provs, "https://h", "run1", sizes, 1, "", false)) {
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
