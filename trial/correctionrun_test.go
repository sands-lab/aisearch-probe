package main

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"aisearch-probe/internal/probe"
)

// THE CORRECTION RUN (docs/experiments/correction.md; plan items 3 and 8), runner
// side. Spec pinned here before the code:
//   - ONE FRESH PAGE PER CHECK TIME, per (vendor, format, trial), on the changeable
//     page (/correction/html/ or /correction/txt/): warmed once, switched once, asked again
//     once — so no check can disturb another (Moura et al. IMC 2019 / Schomp et al.
//     IMC 2013 use one fresh name per probe for the same reason)
//   - each warm-up starts at a random moment within the start spread, and each
//     check lands at a random point INSIDE ITS OWN STEP (between the geometric
//     midpoints with its neighbours), so checks never fall into step with a vendor's
//     own cycle (RFC 2330 §11.1.1) and steps never overlap; drawn once at plan time
//     and stored
//   - warm-up ask; the switch to version 2 fires on a TIMER — settle time after the
//     warm-up answer FINISHED — never on a count of fetches
//   - a warm-up answer without a version-1 code voids the trial: no switch, no asks
//   - check times are per vendor, measured from the switch, with no default
//   - the warm-up asks for the 95% code; the ask-again asks for the 50% code, which
//     the warm-up answer never quoted, so an A answer must come from a stored PAGE,
//     not a replayed answer; an answer is read as A, B, both or neither from which
//     version's codes it quotes (any depth)
//   - each logged fetch is labelled by the tag we sent (A bytes, B bytes, or
//     "not modified"); each ask-again gets an outcome from the fetches during that
//     ask plus the answer's version; "fetched B, answered A" is kept distinct
//   - one lock per state file, so sittings planned at different times can overlap

const corrSecret = "test-page-code-secret"

func corrCallers() []Caller {
	return []Caller{
		{Name: "or:openai", Model: "openai/gpt-5-mini"},
		{Name: "anthropic", Model: "claude-sonnet-5"},
	}
}

// The ask-again spec names each vendor's offsets from the switch. "*" covers any
// vendor not named. Every active vendor must be covered; there is no default.
func TestParseReaskSpec(t *testing.T) {
	names := []string{"or:openai", "anthropic", "or:x-ai"}
	got, err := parseReaskSpec("or:openai=20m,10m; *=12h,24h", names)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got["or:openai"]) != "[10m0s 20m0s]" {
		t.Errorf("or:openai = %v, want sorted [10m 20m]", got["or:openai"])
	}
	if fmt.Sprint(got["anthropic"]) != "[12h0m0s 24h0m0s]" || fmt.Sprint(got["or:x-ai"]) != "[12h0m0s 24h0m0s]" {
		t.Errorf("fallback not applied: %v", got)
	}

	for spec, why := range map[string]string{
		"":                                "empty: there is no default",
		"or:openai=10m":                   "anthropic and or:x-ai not covered",
		"or:openai=10m;*=abc":             "bad duration",
		"or:openai=;*=1h":                 "empty list",
		"or:openai=-5m;*=1h":              "negative offset",
		"or:openai=0s;*=1h":               "zero offset: an ask at the switch instant",
		"or:openai=10m;or:openai=1h;*=1h": "vendor named twice",
		"or:gemini=10m;*=1h":              "vendor not active",
	} {
		if _, err := parseReaskSpec(spec, names); err == nil {
			t.Errorf("%q accepted; want an error (%s)", spec, why)
		}
	}
}

// testLayout is a plan with no randomness: every warm-up due at start, every check
// at its nominal time.
func testLayout(checks map[string][]time.Duration) correctionLayout {
	return correctionLayout{
		Formats: []string{"html", "txt"}, Trials: 3, Size: 2048, Checks: checks,
		Start: time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC),
	}
}

// One fresh page per vendor x format x trial x CHECK TIME, each at its own
// address, every address unique and carrying the vendor's tag (so two models never
// share a copy), each with exactly one ask-again at its own check time.
func TestBuildCorrectionPages(t *testing.T) {
	reask := map[string][]time.Duration{
		"or:openai": {10 * time.Minute, 20 * time.Minute},
		"anthropic": {12 * time.Hour},
	}
	pages, err := buildCorrectionPages(corrCallers(), "https://probe.example.org/", "run1", testLayout(reask))
	if err != nil {
		t.Fatal(err)
	}
	// openai: 2 checks x 2 formats x 3 trials = 12; anthropic: 1 x 2 x 3 = 6.
	if len(pages) != 18 {
		t.Fatalf("%d pages, want 18", len(pages))
	}
	perCheck := map[string]int{}
	for _, p := range pages {
		if len(p.Reasks) != 1 {
			t.Fatalf("%s: %d ask-agains, want exactly 1 (one page per check time)", p.PageID, len(p.Reasks))
		}
		perCheck[p.Provider+" "+p.Format+" "+p.Reasks[0].Offset]++
		if p.Reasks[0].Wait != p.Reasks[0].Offset {
			t.Errorf("%s: drawn wait %s differs from nominal %s with no jitter", p.PageID, p.Reasks[0].Wait, p.Reasks[0].Offset)
		}
		if p.WarmDueUnix != unixf(testLayout(nil).Start) {
			t.Errorf("%s: warm-up due %v, want the start with no spread", p.PageID, p.WarmDueUnix)
		}
	}
	for k, n := range perCheck {
		if n != 3 {
			t.Errorf("%s: %d pages, want 3 (one per trial)", k, n)
		}
	}
	seen := map[string]bool{}
	for _, p := range pages {
		if seen[p.PageID] || seen[p.URL] {
			t.Errorf("duplicate page %s", p.PageID)
		}
		seen[p.PageID], seen[p.URL] = true, true
		prefix := map[string]string{"html": "/correction/html/", "txt": "/correction/txt/"}[p.Format]
		if want := "https://probe.example.org" + prefix + p.PageID + "/2048"; p.URL != want {
			t.Errorf("url %q, want %q", p.URL, want)
		}
		if p.Path != prefix+p.PageID+"/2048" {
			t.Errorf("path %q", p.Path)
		}
		var tag string
		for _, c := range corrCallers() {
			if c.Name == p.Provider {
				tag = c.Tag()
			}
		}
		if !strings.Contains(p.PageID, tag) {
			t.Errorf("page id %q does not carry the caller tag %q", p.PageID, tag)
		}
		if p.Status != corrNew || p.Warm == nil || p.Warm.Status != "pending" {
			t.Errorf("page %s not planned as new with a pending warm-up", p.PageID)
		}
	}
	bad := testLayout(reask)
	bad.Formats = []string{"pdf"}
	if _, err := buildCorrectionPages(corrCallers(), "https://x", "r", bad); err == nil {
		t.Error("an unknown format was accepted")
	}
}

// Each check's random window runs between the geometric midpoints with its
// neighbours: on a doubling grid that is nominal/√2 to nominal·√2. Windows are
// contiguous and never overlap, so a "1 h" check can never land in the "2 h" step.
func TestCheckWindows(t *testing.T) {
	grid := []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour}
	prevHi := time.Duration(0)
	for i, nominal := range grid {
		lo, hi := checkWindow(grid, i)
		if !(lo < nominal && nominal < hi) {
			t.Errorf("%s: window [%s, %s) does not contain it", nominal, lo, hi)
		}
		ratioLo, ratioHi := float64(nominal)/float64(lo), float64(hi)/float64(nominal)
		if ratioLo < 1.41 || ratioLo > 1.42 || ratioHi < 1.41 || ratioHi > 1.42 {
			t.Errorf("%s: window [%s, %s), want nominal/√2 .. nominal·√2", nominal, lo, hi)
		}
		if i > 0 && lo != prevHi {
			t.Errorf("%s: window starts %s, previous ended %s — gap or overlap", nominal, lo, prevHi)
		}
		prevHi = hi
	}
	// A single check still gets a window around it.
	if lo, hi := checkWindow([]time.Duration{time.Hour}, 0); !(lo < time.Hour && time.Hour < hi) {
		t.Errorf("single check window [%s, %s)", lo, hi)
	}
}

// With jitter on, the drawn wait stays inside the check's window; it is stored so
// a restart cannot redraw it.
func TestJitterStaysInWindow(t *testing.T) {
	grid := []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour}
	for _, u := range []float64{0, 0.5, 0.999999} {
		lay := testLayout(map[string][]time.Duration{"or:openai": grid, "anthropic": grid})
		lay.Jitter = true
		lay.Rand = func() float64 { return u }
		pages, err := buildCorrectionPages(corrCallers(), "https://probe.example.org", "run1", lay)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pages {
			nominal, _ := time.ParseDuration(p.Reasks[0].Offset)
			wait, err := time.ParseDuration(p.Reasks[0].Wait)
			if err != nil {
				t.Fatalf("%s: wait %q: %v", p.PageID, p.Reasks[0].Wait, err)
			}
			var idx int
			for i, g := range grid {
				if g == nominal {
					idx = i
				}
			}
			lo, hi := checkWindow(grid, idx)
			if wait < lo-time.Second || wait >= hi {
				t.Errorf("u=%v %s: wait %s outside [%s, %s)", u, nominal, wait, lo, hi)
			}
		}
	}
}

// Warm-ups go out one at a time, so each gets its own slot: exactly WarmGap apart,
// every slot used once, in a random order that mixes the vendors (so no vendor
// always warms first, and a slow vendor's asks never queue up the others').
func TestWarmSlots(t *testing.T) {
	grid := []time.Duration{15 * time.Minute, time.Hour, 4 * time.Hour}
	lay := testLayout(map[string][]time.Duration{"or:openai": grid, "anthropic": grid})
	lay.Trials = 1
	lay.WarmGap = 90 * time.Second
	lay.Rand = rand.New(rand.NewSource(7)).Float64
	pages, err := buildCorrectionPages(corrCallers(), "https://probe.example.org", "run1", lay)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 12 {
		t.Fatalf("%d pages, want 12", len(pages))
	}
	start := unixf(lay.Start)
	bySlot := map[int]*CorrectionPage{}
	for _, p := range pages {
		off := p.WarmDueUnix - start
		k := int(off / 90)
		if off != float64(k*90) || k < 0 || k >= len(pages) {
			t.Fatalf("%s: warm-up at start+%vs, not on a 90 s slot", p.PageID, off)
		}
		if bySlot[k] != nil {
			t.Fatalf("slot %d used twice", k)
		}
		bySlot[k] = p
	}
	// Mixed, not grouped: both vendors appear among the first half of the slots,
	// and the order is not simply the planning order.
	firstHalf := map[string]bool{}
	inPlanOrder := true
	for k := 0; k < len(pages); k++ {
		if k < len(pages)/2 {
			firstHalf[bySlot[k].Provider] = true
		}
		if bySlot[k] != pages[k] {
			inPlanOrder = false
		}
	}
	if len(firstHalf) != 2 {
		t.Errorf("first half of the slots holds only %v — vendors not mixed", firstHalf)
	}
	if inPlanOrder {
		t.Error("warm-up order is the planning order — not shuffled")
	}
}

// The ask-again records how long after the switch it was ACTUALLY sent. A queued
// or late tick moves that, and the report must use it, not the planned wait.
func TestActualWaitIsRecorded(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	w := &fakeWorld{clock: t0}
	d := w.deps(5 * time.Minute)
	st := pagesState(t, t0, []time.Duration{10 * time.Minute})
	pg := st.Pages[0]
	tickCorrection(st, d)
	w.clock = t0.Add(5 * time.Minute)
	tickCorrection(st, d)
	flip := w.clock
	w.clock = flip.Add(13 * time.Minute) // due at +10m, the loop got to it 3 min late
	tickCorrection(st, d)
	a := pg.Reasks[0]
	if a.Status != "done" || a.WaitedS != 13*60 {
		t.Fatalf("waited_s = %v (status %s), want 780", a.WaitedS, a.Status)
	}
	if got := actualWait(a, pg); got != 13*time.Minute {
		t.Errorf("actualWait = %s, want 13m", got)
	}
}

// Each sitting is its own state file with its own lock, so a sitting started on
// Thursday is not refused while Tuesday's is still waiting out its 64-hour checks.
func TestLockPerSitting(t *testing.T) {
	a := correctionLockFor("logs/correction-s1.state.json")
	b := correctionLockFor("logs/correction-s2.state.json")
	if a == b {
		t.Errorf("two sittings share the lock %s", a)
	}
	if a != correctionLockFor("logs/correction-s1.state.json") {
		t.Error("the same sitting must always map to the same lock")
	}
	if !strings.HasPrefix(a, "logs/") {
		t.Errorf("lock %s should sit next to its state file", a)
	}
}

// An answer is read by which version's codes it quotes — any of the five depths.
// Both versions is unreadable; neither means it never had the page.
func TestClassifyVersion(t *testing.T) {
	id := "pg-1"
	a := func(pct int) string { return probe.DepthMarkerPrefix + probe.DepthCode(corrSecret, id, pct, 1) }
	b := func(pct int) string { return probe.DepthMarkerPrefix + probe.DepthCode(corrSecret, id, pct, 2) }
	cases := []struct {
		answer string
		want   string
	}{
		{"The code is " + a(95), "A"},
		{"Top marker " + strings.ToUpper(probe.DepthCode(corrSecret, id, 0, 2)), "B"},
		{a(95) + " and " + b(95), "both"},
		{"I could not open the page.", "neither"},
		{"code " + probe.DepthCode(corrSecret, "other-page", 95, 2), "neither"},
	}
	for _, c := range cases {
		if got, _, _ := classifyVersion(c.answer, corrSecret, id); got != c.want {
			t.Errorf("%q -> %s, want %s", c.answer, got, c.want)
		}
	}
	_, depA, depB := classifyVersion(a(0)+" "+a(95), corrSecret, id)
	if fmt.Sprint(depA) != "[0 95]" || len(depB) != 0 {
		t.Errorf("depths A=%v B=%v, want [0 95] and none", depA, depB)
	}
}

// fakeWorld stands in for the origin and the model: the control sets a version,
// and an ask "reads" whatever version the page is at — or a canned answer.
type fakeWorld struct {
	version   map[string]int
	clock     time.Time
	asks      []string // page ids asked, in order
	questions []string // the question of each ask, in order
	flips     []time.Time
	answerAs  map[string]string // page id -> "A"/"B"/"none" override
	askTook   time.Duration
}

func (w *fakeWorld) deps(settle time.Duration) correctionDeps {
	return correctionDeps{
		secret: corrSecret,
		settle: settle,
		now:    func() time.Time { return w.clock },
		setVersion: func(id string, v int) (versionReply, error) {
			if w.version == nil {
				w.version = map[string]int{}
			}
			if v == 2 {
				w.flips = append(w.flips, w.clock)
			}
			w.version[id] = v
			return versionReply{ID: id, Version: v, ETags: map[string]map[string]string{
				"html": {"1": `"tagA-` + id + `"`, "2": `"tagB-` + id + `"`},
				"txt":  {"1": `"tagA-` + id + `"`, "2": `"tagB-` + id + `"`},
			}}, nil
		},
		ask: func(p *CorrectionPage, question string) (string, probe.Usage, error) {
			w.asks = append(w.asks, p.PageID)
			w.questions = append(w.questions, question)
			w.clock = w.clock.Add(w.askTook)
			v := w.version[p.PageID]
			switch w.answerAs[p.PageID] {
			case "A":
				v = 1
			case "B":
				v = 2
			case "none":
				return "I could not fetch it.", probe.Usage{}, nil
			}
			return "id " + p.PageID + " code " + probe.DepthMarkerPrefix + probe.DepthCode(corrSecret, p.PageID, 95, v), probe.Usage{InputTokens: 100}, nil
		},
		save: func(*CorrectionState) error { return nil },
	}
}

// pagesState plans one vendor, HTML only, one trial, with the given check times
// and start: one page per check time, no randomness.
func pagesState(t *testing.T, start time.Time, offsets []time.Duration) *CorrectionState {
	t.Helper()
	lay := correctionLayout{Formats: []string{"html"}, Trials: 1, Size: 2048, Start: start,
		Checks: map[string][]time.Duration{"or:openai": offsets}}
	pages, err := buildCorrectionPages(corrCallers()[:1], "https://probe.example.org", "run1", lay)
	if err != nil {
		t.Fatal(err)
	}
	return &CorrectionState{RunID: "run1", Host: "probe.example.org", Size: 2048, Pages: pages}
}

func onePageState(t *testing.T, offsets []time.Duration) *CorrectionState {
	return pagesState(t, time.Time{}, offsets)
}

// The full timeline for one page: nothing before its warm-up time; set to A and
// warm-up (asking for the 95% code); the switch exactly settle after the warm-up
// answer finished (not before, even if ticks come early); the single ask-again at
// switch + its wait (asking for the 50% code). A tick never fires anything twice.
func TestCorrectionTimeline(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	w := &fakeWorld{clock: t0.Add(-time.Minute), askTook: 40 * time.Second}
	d := w.deps(5 * time.Minute)
	st := pagesState(t, t0, []time.Duration{10 * time.Minute})
	pg := st.Pages[0]

	// Before the warm-up's due time nothing happens.
	tickCorrection(st, d)
	if len(w.asks) != 0 || w.version[pg.PageID] != 0 {
		t.Fatal("acted before the warm-up was due")
	}

	// At its time: registers version 1 and warms. The answer finishes 40 s later.
	w.clock = t0
	tickCorrection(st, d)
	if w.version[pg.PageID] != 1 || len(w.asks) != 1 || pg.Status != corrWarmed {
		t.Fatalf("after warm-up: version %d, asks %d, status %s", w.version[pg.PageID], len(w.asks), pg.Status)
	}
	if pg.Warm.Version != "A" || pg.ETagA == "" || pg.ETagB == "" {
		t.Errorf("warm-up read %s, tags %q/%q", pg.Warm.Version, pg.ETagA, pg.ETagB)
	}
	if w.questions[0] != correctionWarmQuestion || !strings.Contains(correctionWarmQuestion, "depth=95%") {
		t.Errorf("warm-up asked %q, want the 95%% question", w.questions[0])
	}
	warmDone := t0.Add(40 * time.Second)
	if pg.Warm.DoneUnix != unixf(warmDone) {
		t.Errorf("warm-up done at %v, want %v", pg.Warm.DoneUnix, unixf(warmDone))
	}

	// Ticks before warmDone+5m must not switch.
	w.clock = warmDone.Add(5*time.Minute - time.Second)
	tickCorrection(st, d)
	if len(w.flips) != 0 {
		t.Fatal("switched before the settle time had passed")
	}
	w.clock = warmDone.Add(5 * time.Minute)
	tickCorrection(st, d)
	if len(w.flips) != 1 || pg.Status != corrFlipped || pg.FlipUnix != unixf(w.clock) {
		t.Fatalf("switch not done at warmDone+settle: flips %d status %s", len(w.flips), pg.Status)
	}
	flip := w.clock

	// The ask-again is due at flip+10m, not before.
	w.clock = flip.Add(10*time.Minute - time.Second)
	tickCorrection(st, d)
	if len(w.asks) != 1 {
		t.Fatal("asked again before its wait")
	}
	w.clock = flip.Add(10 * time.Minute)
	tickCorrection(st, d)
	if len(w.asks) != 2 || pg.Reasks[0].Status != "done" || pg.Reasks[0].Version != "B" || pg.Status != corrDone {
		t.Fatalf("ask-again: asks %d status %s version %s page %s", len(w.asks), pg.Reasks[0].Status, pg.Reasks[0].Version, pg.Status)
	}
	if w.questions[1] != correctionReaskQuestion || !strings.Contains(correctionReaskQuestion, "depth=50%") {
		t.Errorf("ask-again asked %q, want the 50%% question (a code the warm-up never quoted)", w.questions[1])
	}
	// A late tick fires nothing more.
	w.clock = flip.Add(48 * time.Hour)
	tickCorrection(st, d)
	if len(w.asks) != 2 {
		t.Errorf("a finished page was asked again (%d asks)", len(w.asks))
	}
	if !correctionFinished(st) {
		t.Error("state with every page done does not read as finished")
	}
}

// Pages for different check times run side by side and independently: each is
// warmed, switched and asked once on its own clock.
func TestPagesPerCheckAreIndependent(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	w := &fakeWorld{clock: t0}
	d := w.deps(5 * time.Minute)
	st := pagesState(t, t0, []time.Duration{15 * time.Minute, time.Hour})
	if len(st.Pages) != 2 {
		t.Fatalf("%d pages, want 2", len(st.Pages))
	}
	tickCorrection(st, d) // both warm
	w.clock = t0.Add(5 * time.Minute)
	tickCorrection(st, d) // both switch
	w.clock = t0.Add(5*time.Minute + 15*time.Minute)
	tickCorrection(st, d) // only the 15-minute page asks again
	asked := map[string]int{}
	for _, id := range w.asks {
		asked[id]++
	}
	short, long := st.Pages[0], st.Pages[1]
	if short.Reasks[0].Offset != "15m0s" {
		short, long = long, short
	}
	if asked[short.PageID] != 2 || asked[long.PageID] != 1 || short.Status != corrDone || long.Status != corrFlipped {
		t.Errorf("after 15 min: short asked %d (%s), long asked %d (%s)", asked[short.PageID], short.Status, asked[long.PageID], long.Status)
	}
}

// A warm-up answer with no version-1 code voids the trial: the model never had A,
// so a later B answer could not show a correction. No switch, no paid asks.
func TestWarmWithoutCodeVoidsTrial(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	w := &fakeWorld{clock: t0, answerAs: map[string]string{}}
	d := w.deps(5 * time.Minute)
	st := onePageState(t, []time.Duration{10 * time.Minute})
	w.answerAs[st.Pages[0].PageID] = "none"

	tickCorrection(st, d)
	w.clock = t0.Add(48 * time.Hour)
	tickCorrection(st, d)
	pg := st.Pages[0]
	if pg.Status != corrVoid || pg.Why == "" {
		t.Errorf("status %s why %q, want void with a reason", pg.Status, pg.Why)
	}
	if len(w.flips) != 0 || len(w.asks) != 1 {
		t.Errorf("void trial still switched (%d) or asked again (%d asks)", len(w.flips), len(w.asks))
	}
	if !correctionFinished(st) {
		t.Error("a void page must count as finished")
	}
}

// Each logged request for the page is labelled by the tag we sent: A bytes, B
// bytes, or "not modified". Our own probes, other hosts and other pages never count.
func TestLabelFetches(t *testing.T) {
	pg := &CorrectionPage{PageID: "pg-1", Path: "/correction/html/pg-1/2048", ETagA: `"aaa"`, ETagB: `"bbb"`}
	rec := func(ts float64, url string, status int, tag, bot string) probe.Record {
		return probe.Record{TSUnix: ts, URL: url, PageKey: strings.SplitN(strings.TrimPrefix(url, "https://probe.example.org"), "?", 2)[0],
			StatusCode: status, ETagSent: tag, Bot: bot}
	}
	u := "https://probe.example.org/correction/html/pg-1/2048"
	recs := []probe.Record{
		rec(100, u, 200, `"aaa"`, "ChatGPT-User"),
		rec(200, u, 200, `"bbb"`, "ChatGPT-User"),
		rec(300, u, 304, `"bbb"`, "ChatGPT-User"),
		rec(400, u, 200, `"bbb"`, "probe-selfcheck"),                                        // our own
		rec(500, "https://other.example.org/correction/html/pg-1/2048", 200, `"bbb"`, "X"),  // other host
		rec(600, "https://probe.example.org/correction/html/pg-12/2048", 200, `"zzz"`, "X"), // other page
		rec(700, u+"?x=1", 200, `"bbb"`, "Claude-User"),                                     // query kept as the same page
	}
	got := labelFetches(recs, pg, "probe.example.org")
	var labels []string
	for _, f := range got {
		labels = append(labels, fmt.Sprintf("%.0f:%s", f.TSUnix, f.Served))
	}
	if strings.Join(labels, " ") != "100:A 200:B 300:not-modified 700:B" {
		t.Errorf("labels = %v", labels)
	}
}

// Each ask-again gets an outcome from the fetches DURING that ask and the version
// its answer quotes. The three headline states, and the rest kept as themselves.
func TestReaskOutcome(t *testing.T) {
	fetches := []FetchLabel{
		{TSUnix: 1005, Served: "B"},
		{TSUnix: 2005, Served: "not-modified"},
	}
	ask := func(sent, done float64, version string) *CorrectionAsk {
		return &CorrectionAsk{Kind: "reask", Status: "done", SentUnix: sent, DoneUnix: done, Version: version}
	}
	cases := []struct {
		a    *CorrectionAsk
		want string
	}{
		{ask(500, 600, "A"), "no fetch / answer A"},
		{ask(1000, 1010, "B"), "fetch B / answer B"},
		{ask(1000, 1010, "A"), "fetch B / answer A"},
		{ask(2000, 2010, "B"), "not modified / answer B"},
		{ask(1006, 1010, "A"), "no fetch / answer A"}, // the B fetch came before this ask
		{ask(500, 600, "neither"), "no fetch / no code"},
		{ask(1000, 1010, "both"), "fetch B / both codes"},
		{&CorrectionAsk{Kind: "reask", Status: "done", SentUnix: 1000, DoneUnix: 1010, Err: "timeout", Version: "neither"}, "fetch B / ask failed"},
	}
	for _, c := range cases {
		if got := reaskOutcome(c.a, fetches); got != c.want {
			t.Errorf("ask %.0f-%.0f %s: %q, want %q", c.a.SentUnix, c.a.DoneUnix, c.a.Version, got, c.want)
		}
	}
	if got := reaskOutcome(&CorrectionAsk{Kind: "reask", Status: "pending"}, fetches); got != "not asked" {
		t.Errorf("pending ask outcome %q, want %q", got, "not asked")
	}
}

// The two events the test is about, kept separate: the first fetch after the
// switch that got B's bytes, and the first answer that quoted B.
func TestFirstEvents(t *testing.T) {
	pg := &CorrectionPage{FlipUnix: 1000, Reasks: []*CorrectionAsk{
		{Kind: "reask", Status: "done", SentUnix: 1500, DoneUnix: 1510, Version: "A"},
		{Kind: "reask", Status: "done", SentUnix: 3000, DoneUnix: 3020, Version: "B"},
	}}
	fetches := []FetchLabel{
		{TSUnix: 900, Served: "A"},
		{TSUnix: 1505, Served: "B"},
		{TSUnix: 3010, Served: "B"},
	}
	fb, ab := firstEvents(pg, fetches)
	if fb != 1505 || ab != 3020 {
		t.Errorf("first B fetch %v, first B answer %v; want 1505 and 3020", fb, ab)
	}
	fb, ab = firstEvents(&CorrectionPage{FlipUnix: 1000}, nil)
	if fb != 0 || ab != 0 {
		t.Errorf("nothing seen must be 0/0, got %v/%v", fb, ab)
	}
}

// A sitting can be planned now and start later (repeats are spread across days and
// hours). The start must carry an explicit time zone — a bare "10:00" is ambiguous
// between local time and UTC — and a time in the past or more than 30 days out is
// almost certainly a typo, so it is refused. Empty means "now".
func TestParseStartAt(t *testing.T) {
	now := time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC)
	got, err := parseStartAt("", now)
	if err != nil || !got.Equal(now) {
		t.Errorf(`"" -> %v, %v; want now`, got, err)
	}
	want := time.Date(2026, 10, 15, 7, 0, 0, 0, time.UTC) // Thu 10:00 at UTC+3
	for _, s := range []string{"2026-10-15T10:00:00+03:00", "2026-10-15T10:00+03:00", "2026-10-15T07:00Z"} {
		got, err := parseStartAt(s, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q -> %v, %v; want %v", s, got, err, want)
		}
	}
	for s, why := range map[string]string{
		"2026-10-15T10:00":       "no time zone",
		"2026-10-14T13:00+03:00": "in the past (13:00 at UTC+3 = 10:00 UTC)",
		"2026-11-20T10:00+03:00": "more than 30 days out",
		"Thursday 10am":          "not a date",
	} {
		if _, err := parseStartAt(s, now); err == nil {
			t.Errorf("%q accepted; want an error (%s)", s, why)
		}
	}
}

// The first warm-up slot is the start time; the rest follow WarmGap apart.
func TestPlanStartsAtStartTime(t *testing.T) {
	start := time.Date(2026, 10, 15, 7, 0, 0, 0, time.UTC)
	lay := testLayout(map[string][]time.Duration{"or:openai": {time.Hour}})
	lay.Start, lay.WarmGap = start, 90*time.Second
	pages, err := buildCorrectionPages(corrCallers()[:1], "https://probe.example.org", "run1", lay)
	if err != nil {
		t.Fatal(err)
	}
	earliest := pages[0].WarmDueUnix
	for _, p := range pages {
		if p.WarmDueUnix < earliest {
			earliest = p.WarmDueUnix
		}
	}
	if earliest != unixf(start) {
		t.Errorf("first warm-up at %v, want the start time %v", earliest, unixf(start))
	}
}

// The report says "planned" for a check not yet asked and "actually asked" only
// once it was — never a planned time dressed up as an actual one.
func TestCheckLabel(t *testing.T) {
	pg := &CorrectionPage{FlipUnix: 1000}
	pending := &CorrectionAsk{Kind: "reask", Status: "pending", Offset: "1h0m0s", Wait: "1h12m0s"}
	if got := checkLabel(pending, pg); !strings.Contains(got, "planned") || strings.Contains(got, "actually") {
		t.Errorf("pending label %q, want 'planned …'", got)
	}
	done := &CorrectionAsk{Kind: "reask", Status: "done", Offset: "1h0m0s", Wait: "1h12m0s", SentUnix: 1000 + 4500, WaitedS: 4500}
	if got := checkLabel(done, pg); !strings.Contains(got, "actually asked at switch + 1h15m0s") {
		t.Errorf("done label %q, want the actual wait 1h15m0s", got)
	}
}

// THE SITE-UP CHECK runs only when something is due. It exists so a down site
// never reads as "old answer, no new origin request" — which only matters right
// before a step fires. A tick with nothing due sends nothing to the server (the
// loop ticks every minute for days; checking every tick was ~5,800 requests a
// sitting for nothing).
func TestNoSiteCheckWhenNothingDue(t *testing.T) {
	t0 := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	w := &fakeWorld{clock: t0}
	d := w.deps(5 * time.Minute)
	checks := 0
	d.reachable = func() bool { checks++; return true }
	st := pagesState(t, t0.Add(time.Hour), []time.Duration{15 * time.Minute}) // warm-up due in 1 h
	tickCorrection(st, d)
	if checks != 0 || len(w.asks) != 0 {
		t.Errorf("nothing due: %d site checks, %d asks; want 0 and 0", checks, len(w.asks))
	}
}

// When steps are due, the site is checked ONCE for the tick, however many fire.
func TestOneSiteCheckPerTickWithWork(t *testing.T) {
	t0 := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	w := &fakeWorld{clock: t0}
	d := w.deps(5 * time.Minute)
	checks := 0
	d.reachable = func() bool { checks++; return true }
	st := pagesState(t, t0, []time.Duration{15 * time.Minute, time.Hour, 4 * time.Hour}) // 3 warm-ups due now
	tickCorrection(st, d)
	if len(w.asks) != 3 || checks != 1 {
		t.Errorf("3 due warm-ups: %d asks, %d site checks; want 3 and 1", len(w.asks), checks)
	}
}

// A site that does not answer fires nothing — no ask, no switch — and the same
// steps fire on the next tick once it answers.
func TestSiteDownFiresNothing(t *testing.T) {
	t0 := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	w := &fakeWorld{clock: t0}
	d := w.deps(5 * time.Minute)
	up := false
	d.reachable = func() bool { return up }
	st := pagesState(t, t0, []time.Duration{15 * time.Minute})
	tickCorrection(st, d)
	if len(w.asks) != 0 || w.version[st.Pages[0].PageID] != 0 || st.Pages[0].Status != corrNew {
		t.Fatalf("site down: %d asks, version %d, status %s; want nothing done", len(w.asks), w.version[st.Pages[0].PageID], st.Pages[0].Status)
	}
	up = true
	w.clock = t0.Add(time.Minute)
	tickCorrection(st, d)
	if len(w.asks) != 1 || st.Pages[0].Status != corrWarmed {
		t.Errorf("site back: %d asks, status %s; want the warm-up done", len(w.asks), st.Pages[0].Status)
	}
}

// Every ask keeps the provider's OWN report of its fetch (probe.Usage.FetchStatus:
// Anthropic web_fetch error_code, OpenRouter annotations, …), warm-up and
// ask-again alike, so analysis can tell "the provider says the fetch failed" from
// "no request reached us".
func TestAskKeepsProviderFetchStatus(t *testing.T) {
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	w := &fakeWorld{clock: t0}
	d := w.deps(5 * time.Minute)
	inner := d.ask
	n := 0
	d.ask = func(p *CorrectionPage, q string) (string, probe.Usage, error) {
		ans, u, err := inner(p, q)
		n++
		u.FetchStatus = fmt.Sprintf("web_fetch:ok#%d", n)
		return ans, u, err
	}
	st := pagesState(t, t0, []time.Duration{10 * time.Minute})
	pg := st.Pages[0]
	tickCorrection(st, d) // warm-up
	w.clock = t0.Add(5 * time.Minute)
	tickCorrection(st, d) // switch
	w.clock = w.clock.Add(10 * time.Minute)
	tickCorrection(st, d) // ask-again
	if pg.Warm.FetchStatus != "web_fetch:ok#1" || pg.Reasks[0].FetchStatus != "web_fetch:ok#2" {
		t.Errorf("fetch status warm %q, ask-again %q; want the provider's report on each", pg.Warm.FetchStatus, pg.Reasks[0].FetchStatus)
	}
}
