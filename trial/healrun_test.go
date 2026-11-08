package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aisearch-probe/internal/probe"
)

// THE REMEMBERED-FAILURE RUN (heal redo). Agreed design, 2026-10-22:
//   - one fresh /healpage/ per vendor x check time x failure (503, 404, 403); the
//     control creates it broken, it is asked once (the "first ask", depth-95% code),
//     fixed 5 min after that answer came back (a timer, never a fetch count), and
//     asked again ONCE at its check time after the fix (depth-50% code)
//   - check times on a times-2 grid, each drawn at random inside its own step;
//     first asks in shuffled 90 s slots
//   - NOTHING is skipped: a page with a failed call, a first ask that never saw the
//     failure, or a stray early 200 still runs to the end; the report notes it
//   - each re-ask is reported as fetch yes/no x code yes/no
//   - Anthropic uses the fetch tool without dynamic filtering, pinned in the state
//   - same shared question as the sharing and correction runs (sharingAskQuestion)

const healTestSecret = "heal-test-secret"

func TestParseHealLists(t *testing.T) {
	got, err := parseHealChecks("4m, 2m,1h")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[2m0s 4m0s 1h0m0s]" {
		t.Errorf("checks = %v, want sorted [2m 4m 1h]", got)
	}
	for _, bad := range []string{"", "2m,0s", "2m,-1m", "2m,soon", "2m,2m"} {
		if _, err := parseHealChecks(bad); err == nil {
			t.Errorf("checks %q must be refused", bad)
		}
	}
	fails, err := parseHealFails("503, 404,403")
	if err != nil || fmt.Sprint(fails) != "[503 404 403]" {
		t.Errorf("fails = %v %v", fails, err)
	}
	for _, bad := range []string{"", "500", "503,503", "503,xyz"} {
		if _, err := parseHealFails(bad); err == nil {
			t.Errorf("fails %q must be refused", bad)
		}
	}
}

func healTestLayout() healLayout {
	seq := []float64{0.1, 0.9, 0.5, 0.3, 0.7, 0.2, 0.8, 0.4, 0.6}
	i := 0
	return healLayout{
		Checks: []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute},
		Fails:  []string{"503", "404", "403"}, Trials: 1, Size: 2048,
		Start:   time.Date(2026, 10, 23, 6, 0, 0, 0, time.UTC),
		WarmGap: 90 * time.Second, Jitter: true,
		Rand: func() float64 { v := seq[i%len(seq)]; i++; return v },
	}
}

func TestBuildHealPages(t *testing.T) {
	provs := []Caller{{Name: "or:openai", Model: "openai/gpt-5-mini"}, {Name: "anthropic", Model: "claude-sonnet-5"}}
	lay := healTestLayout()
	pages, err := buildHealPages(provs, "https://probe.example.org/", "run1", lay)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2*3*3 {
		t.Fatalf("got %d pages, want vendors x checks x failures = 18", len(pages))
	}
	ids, slots := map[string]bool{}, map[float64]bool{}
	for _, pg := range pages {
		if ids[pg.PageID] {
			t.Errorf("duplicate id %s", pg.PageID)
		}
		ids[pg.PageID] = true
		if want := "/healpage/" + pg.PageID + "/2048"; pg.Path != want || pg.URL != "https://probe.example.org"+want {
			t.Errorf("%s: path %q url %q", pg.PageID, pg.Path, pg.URL)
		}
		if !strings.Contains(pg.PageID, "-heal"+pg.Fail+"-") {
			t.Errorf("%s: id should name its failure %s (the address the fetcher sees does not)", pg.PageID, pg.Fail)
		}
		ci := -1
		for i, c := range lay.Checks {
			if c.String() == pg.Reask.Check {
				ci = i
			}
		}
		if ci < 0 {
			t.Fatalf("%s: check %s not on the grid", pg.PageID, pg.Reask.Check)
		}
		lo, hi := checkWindow(lay.Checks, ci)
		if w := mustDur(pg.Reask.Wait); w < lo || w > hi {
			t.Errorf("%s: wait %s outside its step %s..%s", pg.PageID, w, lo, hi)
		}
		if pg.Warm.Question != sharingAskQuestion(95) || pg.Reask.Question != sharingAskQuestion(50) {
			t.Errorf("%s: questions must be the shared depth-95%% / depth-50%% questions", pg.PageID)
		}
		if slots[pg.WarmDueUnix] {
			t.Errorf("two first asks share the slot at %v", pg.WarmDueUnix)
		}
		slots[pg.WarmDueUnix] = true
		off := pg.WarmDueUnix - unixf(lay.Start)
		if off < 0 || int(off)%90 != 0 || off > float64(17*90) {
			t.Errorf("%s: first-ask slot %vs after start, want a multiple of 90 s inside the run", pg.PageID, off)
		}
	}
	if _, err := buildHealPages(provs, "https://p.example", "r", healLayout{Checks: lay.Checks, Fails: []string{"500"}, Trials: 1, Size: 2048}); err == nil {
		t.Error("an unsupported failure must be refused at planning")
	}
}

// fakeHeal is an origin control and a model with a fake clock, recording calls.
type fakeHeal struct {
	now      time.Time
	calls    []string
	healedAt string
	askErr   map[string]error // question -> error
	answer   func(id, q string) string
}

func (f *fakeHeal) deps(st *HealState) healDeps {
	return healDeps{
		secret: healTestSecret,
		settle: 5 * time.Minute,
		now:    func() time.Time { return f.now },
		control: func(id, action, fail string) (healReply, error) {
			f.calls = append(f.calls, action+" "+id+" "+fail)
			rep := healReply{ID: id, Fail: fail, State: "broken", ETag: `"fixed-tag"`}
			if action == "heal" {
				if f.healedAt == "" {
					f.healedAt = f.now.UTC().Format(time.RFC3339)
				}
				rep.State, rep.HealedAt = "healed", f.healedAt
			}
			return rep, nil
		},
		ask: func(pg *HealPage, q string) (string, probe.Usage, error) {
			f.calls = append(f.calls, "ask "+pg.PageID+" "+q)
			if err := f.askErr[q]; err != nil {
				return "", probe.Usage{}, err
			}
			if f.answer != nil {
				return f.answer(pg.PageID, q), probe.Usage{}, nil
			}
			return "the page is unavailable", probe.Usage{}, nil
		},
		save: func(*HealState) error { return nil },
	}
}

func healOnePage(t *testing.T) *HealState {
	t.Helper()
	lay := healTestLayout()
	lay.Checks, lay.Fails, lay.Jitter = []time.Duration{16 * time.Minute}, []string{"503"}, false
	pages, err := buildHealPages([]Caller{{Name: "or:openai", Model: "openai/gpt-5-mini"}}, "https://p.example", "r1", lay)
	if err != nil {
		t.Fatal(err)
	}
	return &HealState{Settle: "5m", Pages: pages}
}

// The whole timeline: create broken -> first ask -> nothing until 5 min after the
// answer -> fix -> nothing until the check -> confirm still fixed -> re-ask.
func TestHealTimeline(t *testing.T) {
	st := healOnePage(t)
	pg := st.Pages[0]
	f := &fakeHeal{now: time.Unix(int64(pg.WarmDueUnix), 0).UTC()}
	d := f.deps(st)

	tickHeal(st, d)
	if pg.Status != healWarmed || len(f.calls) != 2 ||
		f.calls[0] != "create "+pg.PageID+" 503" || !strings.HasPrefix(f.calls[1], "ask "+pg.PageID+" "+sharingAskQuestion(95)) {
		t.Fatalf("first tick: status %s, calls %v", pg.Status, f.calls)
	}
	f.now = f.now.Add(4 * time.Minute)
	tickHeal(st, d)
	if pg.Status != healWarmed || len(f.calls) != 2 {
		t.Fatalf("before 5 min have passed nothing may happen: %s %v", pg.Status, f.calls)
	}
	f.now = f.now.Add(1 * time.Minute)
	tickHeal(st, d)
	if pg.Status != healFlipped || f.calls[2] != "heal "+pg.PageID+" " {
		t.Fatalf("the fix is due 5 min after the first answer: %s %v", pg.Status, f.calls)
	}
	if want := pg.FlipUnix + (16 * time.Minute).Seconds(); pg.Reask.DueUnix != want {
		t.Errorf("re-ask due %v, want fix + 16 min = %v", pg.Reask.DueUnix, want)
	}
	f.now = time.Unix(int64(pg.Reask.DueUnix), 0).UTC()
	tickHeal(st, d)
	if pg.Status != healDone || f.calls[3] != "heal "+pg.PageID+" " ||
		!strings.HasPrefix(f.calls[4], "ask "+pg.PageID+" "+sharingAskQuestion(50)) {
		t.Fatalf("re-ask: status %s, calls %v", pg.Status, f.calls)
	}
	if pg.Reask.WaitedS != 16*60 {
		t.Errorf("waited %vs, want the actual 960 s", pg.Reask.WaitedS)
	}
}

// Nothing is skipped: a failed first ask still gets its fix and its re-ask, and the
// problem is kept as a note for the report.
func TestHealKeepsGoingAfterAFailedCall(t *testing.T) {
	st := healOnePage(t)
	pg := st.Pages[0]
	f := &fakeHeal{now: time.Unix(int64(pg.WarmDueUnix), 0).UTC(),
		askErr: map[string]error{sharingAskQuestion(95): fmt.Errorf("timeout")}}
	d := f.deps(st)
	for i := 0; i < 4 && pg.Status != healDone; i++ {
		tickHeal(st, d)
		f.now = f.now.Add(30 * time.Minute)
	}
	if pg.Status != healDone || pg.Reask.Status != "done" {
		t.Fatalf("a failed first ask must not stop the page: status %s", pg.Status)
	}
	if pg.Warm.Err == "" {
		t.Error("the failed call must be recorded")
	}
}

// If the origin's record of the fix changed (it lost the page and re-made it), the
// page is still asked, and the report says so.
func TestHealNotesALostFix(t *testing.T) {
	st := healOnePage(t)
	pg := st.Pages[0]
	f := &fakeHeal{now: time.Unix(int64(pg.WarmDueUnix), 0).UTC()}
	d := f.deps(st)
	tickHeal(st, d)
	f.now = f.now.Add(5 * time.Minute)
	tickHeal(st, d)
	f.healedAt = "2030-01-01T00:00:00Z" // the server now reports a different fix time
	f.now = time.Unix(int64(pg.Reask.DueUnix), 0).UTC()
	tickHeal(st, d)
	if pg.Status != healDone {
		t.Fatalf("status %s, want done", pg.Status)
	}
	if !strings.Contains(strings.Join(pg.Notes, " "), "fix time changed") {
		t.Errorf("notes %v must record the changed fix time", pg.Notes)
	}
}

// The report reads each re-ask as fetch yes/no x code yes/no from our log and the
// answer, and notes anything odd — never dropping the page.
func TestHealOutcomeAndNotes(t *testing.T) {
	st := healOnePage(t)
	pg := st.Pages[0]
	pg.ETagFixed = `"fixed-tag"`
	pg.Warm.SentUnix, pg.Warm.DoneUnix, pg.Warm.Status = 1000, 1010, "done"
	pg.FlipUnix = 1310
	pg.Reask.SentUnix, pg.Reask.DoneUnix, pg.Reask.Status = 2270, 2280, "done"
	code50 := probe.DepthCode(healTestSecret, pg.PageID, 50, probe.DepthPageRevision)
	rec := func(ts float64, status int, etag string) probe.Record {
		return probe.Record{TSUnix: ts, PageKey: pg.Path, URL: "https://p.example" + pg.Path, StatusCode: status, ETagSent: etag, Bot: "openai-query"}
	}
	failOnWarm := rec(1005, 503, "")

	cases := []struct {
		name       string
		recs       []probe.Record
		answer     string
		fetch      bool
		code       bool
		wantInNote string
	}{
		{"remembered the failure", []probe.Record{failOnWarm}, "it is down", false, false, ""},
		{"went back", []probe.Record{failOnWarm, rec(2275, 200, `"fixed-tag"`)}, "the code is " + code50, true, true, ""},
		{"fetched but still said broken", []probe.Record{failOnWarm, rec(2275, 200, `"fixed-tag"`)}, "unavailable", true, false, ""},
		{"no fetch but quoted the code", []probe.Record{failOnWarm}, code50, false, true, ""},
		{"first ask never reached us", nil, "it is down", false, false, "no request reached us during the first ask"},
		{"first ask got a page, not the failure", []probe.Record{rec(1005, 200, `"fixed-tag"`)}, "x", false, false, "not the failure"},
		{"fixed page fetched before its check", []probe.Record{failOnWarm, rec(1500, 200, `"fixed-tag"`)}, "x", false, false, "before its check"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pg.Reask.Answer = c.answer
			o := healOutcome(pg, c.recs, "p.example", healTestSecret)
			if o.Fetch != c.fetch || o.Code != c.code {
				t.Errorf("fetch=%v code=%v, want fetch=%v code=%v", o.Fetch, o.Code, c.fetch, c.code)
			}
			notes := strings.Join(o.Notes, " | ")
			if c.wantInNote == "" && notes != "" {
				t.Errorf("unexpected notes: %s", notes)
			}
			if c.wantInNote != "" && !strings.Contains(notes, c.wantInNote) {
				t.Errorf("notes %q must mention %q", notes, c.wantInNote)
			}
		})
	}
}

// The Anthropic fetch tool is pinned in the state at planning (default: without
// dynamic filtering) and applied by every tick, so a loop started without the flag
// cannot quietly switch tools mid-run.
func TestHealPinsAnthropicTool(t *testing.T) {
	defer func() { anthropicFetchVersionOverride = nil }()
	anthropicFetchVersionOverride = nil
	applyHealFetchVersion(&HealState{AnthropicFetchVersion: "web_fetch_20250910"})
	if anthropicFetchVersionOverride == nil || *anthropicFetchVersionOverride != "web_fetch_20250910" {
		t.Errorf("the state's tool version must be applied, got %v", anthropicFetchVersionOverride)
	}
	if defaultHealFetchVersion != "web_fetch_20250910" {
		t.Errorf("default tool %q, want web_fetch_20250910 (no dynamic filtering)", defaultHealFetchVersion)
	}
}

// The agreed default grid (2026-10-22): doubling from 5 min, last step 10 h 40
// (its window reaches about 15 h, "around 16 h is good enough").
func TestHealDefaultChecks(t *testing.T) {
	if defaultHealChecks != "5m,10m,20m,40m,1h20m,2h40m,5h20m,10h40m" {
		t.Errorf("-heal-checks default = %q", defaultHealChecks)
	}
	got, err := parseHealChecks(defaultHealChecks)
	if err != nil || len(got) != 8 {
		t.Fatalf("default grid does not parse to 8 checks: %v %v", got, err)
	}
	for i := 1; i < len(got); i++ {
		if got[i] != 2*got[i-1] {
			t.Errorf("check %s is not twice %s", got[i], got[i-1])
		}
	}
}

// The state round-trips, and planning refuses to overwrite an existing run.
func TestHealStateFile(t *testing.T) {
	st := healOnePage(t)
	st.RunID, st.AnthropicFetchVersion = "r1", defaultHealFetchVersion
	path := filepath.Join(t.TempDir(), "heal.state.json")
	if err := saveHealState(path, st); err != nil {
		t.Fatal(err)
	}
	back, err := loadHealState(path)
	if err != nil || back.RunID != "r1" || len(back.Pages) != 1 || back.Pages[0].Fail != "503" || back.AnthropicFetchVersion != defaultHealFetchVersion {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	if _, err := os.Stat(healLockFor(path)); err == nil {
		t.Error("saving must not create the lock")
	}
	code := planHeal(healPlanArgs{Base: "https://p.example", LocalOrigin: "http://localhost:8899",
		Provs: []Caller{{Name: "or:openai", Model: "m"}}, Checks: "2m", Fails: "503", Trials: 1, Size: 2048,
		Settle: 5 * time.Minute, WarmGap: 90 * time.Second, StatePath: path,
		Secret: "s", ControlSecret: "c", FetchVersion: defaultHealFetchVersion})
	if code == 0 {
		t.Error("planning over an existing state file must be refused")
	}
}

// -heal-report-json writes one row per page with the SAME reading as the printed
// report (healOutcome), so a plot never re-derives the rules or needs the secret.
func TestHealResultsRows(t *testing.T) {
	st := healOnePage(t)
	st.RunID, st.Host = "run-x", "p.example"
	pg := st.Pages[0]
	pg.Warm.SentUnix, pg.Warm.DoneUnix, pg.Warm.Status = 1000, 1010, "done"
	pg.FlipUnix = 1310
	pg.Reask.SentUnix, pg.Reask.DoneUnix, pg.Reask.Status, pg.Reask.WaitedS = 2270, 2280, "done", 960
	pg.Reask.Answer = "the code is " + probe.DepthCode(healTestSecret, pg.PageID, 50, probe.DepthPageRevision)
	recs := []probe.Record{
		{TSUnix: 1005, PageKey: pg.Path, URL: "https://p.example" + pg.Path, StatusCode: 503, Bot: "openai-query"},
		{TSUnix: 2275, PageKey: pg.Path, URL: "https://p.example" + pg.Path, StatusCode: 200, Bot: "openai-query"},
	}
	rows := healResults(st, recs, healTestSecret)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Run != "run-x" || r.Origin != "p.example" || r.Provider != "or:openai" || r.Fail != "503" ||
		r.Check != "16m0s" || r.WaitedS != 960 || !r.Asked || !r.Fetch || !r.Code || r.CallFailed {
		t.Errorf("row = %+v", r)
	}
	// A failed re-ask is marked, never dropped.
	pg.Reask.Err = "timeout"
	if r := healResults(st, recs, healTestSecret)[0]; !r.CallFailed {
		t.Error("a failed re-ask must be marked call_failed")
	}
	// A page not re-asked yet is listed, marked not asked.
	pg.Reask.Status = "pending"
	if r := healResults(st, recs, healTestSecret)[0]; r.Asked {
		t.Error("a pending re-ask must be marked asked=false")
	}
}

// The loop sleeps until the next step instead of waking (and checking the site)
// every minute; a step that failed is retried a minute later, not straight away;
// a finished sitting has nothing to wait for.
func TestHealWaitSleepsUntilNextStep(t *testing.T) {
	st := healOnePage(t)
	pg := st.Pages[0]
	settle := 5 * time.Minute
	warm := time.Unix(int64(pg.WarmDueUnix), 0).UTC()

	if w, due := healWait(st, settle, warm.Add(-10*time.Minute), false); due || w != 10*time.Minute {
		t.Fatalf("before the first ask: wait %v due %v, want 10m and not due", w, due)
	}
	if _, due := healWait(st, settle, warm, false); !due {
		t.Fatal("the first ask is due at its slot")
	}
	if w, due := healWait(st, settle, warm, true); due || w != time.Minute {
		t.Fatalf("still due right after a tick = it failed: wait %v due %v, want a 1m retry", w, due)
	}

	f := &fakeHeal{now: warm}
	d := f.deps(st)
	tickHeal(st, d)
	if w, due := healWait(st, settle, f.now, true); due || w != settle {
		t.Fatalf("after the first answer: wait %v due %v, want the 5m settle", w, due)
	}
	f.now = f.now.Add(settle)
	tickHeal(st, d)
	if w, _ := healWait(st, settle, f.now, true); w != 16*time.Minute {
		t.Fatalf("after the fix: wait %v, want the 16m re-ask wait", w)
	}
	f.now = time.Unix(int64(pg.Reask.DueUnix), 0).UTC()
	tickHeal(st, d)
	if !healFinished(st) {
		t.Fatal("sitting should be finished")
	}
	if w, due := healWait(st, settle, f.now, true); due || w != 0 {
		t.Fatalf("finished: wait %v due %v, want nothing", w, due)
	}
}
