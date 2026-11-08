package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"aisearch-probe/internal/probe"
)

// THE CORRECTION RUN — when does a corrected page reach an answer?
// (doc/handoff-changeable-page.md; plan items 3 "visibility of a page correction"
// and 8 "content type vs update".)
//
// Each trial is one fresh changeable page (/verhtmlpage/ or /vertxtpage/, see
// proxy/verpage.go) for ONE check time, asked about by one vendor:
//
//	1. at its (randomised) warm-up time, set the page to version 1 (A) through the
//	   control and ask once (the warm-up)
//	2. wait SETTLE after the warm-up answer FINISHED — a timer, never a count of
//	   fetches: some fetchers fetch in parallel, so "the Nth request" is not a
//	   moment; "this answer is back and 5 minutes have passed" is
//	3. switch the page to version 2 (B) through the control; record when
//	4. ask again ONCE, at the page's check time after the switch
//
// ONE PAGE PER CHECK TIME, as in the DNS cache studies this design follows (Moura
// et al., IMC 2019; Schomp et al., IMC 2013): a second ask on the same page could
// itself trigger a re-fetch or keep a copy alive, and then a later check would no
// longer measure "a page left alone for this long". Each check time's actual wait
// is drawn at random INSIDE its own step (between the geometric midpoints with its
// neighbours, see checkWindow), and each warm-up starts at a random moment within
// the start spread, so checks never fall into step with a vendor's own cycle
// (RFC 2330 §11.1.1). Both are drawn once, at plan time, and stored.
//
// Version 2's codes did not exist before the switch, so a B code in an answer
// proves the model had B, and an A code after the switch means the user was told
// the old thing. Which version each FETCH got is read afterwards from our log
// (etag_sent), against the tags the control reported. "Fetched B, answered A" is
// kept distinct from ordinary stale reuse.
//
// Like the multi-day retention probe, the schedule lives in a state file and a
// tick fires whatever is due, saving after every step, so a crash or reboot never
// loses or repeats a step: the long checks are days out. Each sitting is its own
// state file with its own lock, so sittings can overlap.

// The two questions are the standard sentence naming one marker — the wording the
// sharing test asks of these same 2 KB depth pages (the generic "study reference
// code" leaves five codes to choose from: the Result 25 grading mismatch). The
// warm-up names the bottom (95%) marker; the ask-again names the middle (50%) one,
// which the warm-up answer never quoted, so an A answer at the ask-again must come
// from a stored copy of the PAGE, not a replayed earlier answer. Any of a version's
// five codes still counts when reading which version an answer holds.
var (
	correctionWarmQuestion  = sharingAskQuestion(probe.DepthPercents[len(probe.DepthPercents)-1])
	correctionReaskQuestion = sharingAskQuestion(50)
)

// Page status, in order.
const (
	corrNew     = "new"     // planned; not yet set to A or asked
	corrWarmed  = "warmed"  // set to A, warm-up answered with an A code; switch pending
	corrFlipped = "flipped" // switched to B; ask-agains pending
	corrDone    = "done"    // every ask-again answered
	corrVoid    = "void"    // the trial cannot show a correction; see Why
)

// verRoutePrefix maps a format to the changeable page's route.
var verRoutePrefix = map[string]string{"html": "/verhtmlpage/", "txt": "/vertxtpage/"}

// CorrectionAsk is one question to one page.
type CorrectionAsk struct {
	Kind     string  `json:"kind"`               // "warm" | "reask"
	Offset   string  `json:"offset,omitempty"`   // reask: the nominal check time (the grid step)
	Wait     string  `json:"wait,omitempty"`     // reask: the drawn wait after the switch (inside the step)
	WaitedS  float64 `json:"waited_s,omitempty"` // reask: seconds from the switch to actually sending (what the analysis uses)
	Question string  `json:"question,omitempty"`
	DueUnix  float64 `json:"due_unix,omitempty"`
	Status   string  `json:"status"` // pending | done
	SentUnix float64 `json:"sent_unix,omitempty"`
	DoneUnix float64 `json:"done_unix,omitempty"` // when the answer came back
	Answer   string  `json:"answer,omitempty"`
	Err      string  `json:"err,omitempty"`
	Version  string  `json:"version,omitempty"` // A | B | both | neither
	DepthsA  []int   `json:"depths_a,omitempty"`
	DepthsB  []int   `json:"depths_b,omitempty"`
	InTok    int     `json:"in_tok,omitempty"`
	OutTok   int     `json:"out_tok,omitempty"`
	Rounds   int     `json:"rounds,omitempty"`
	// FetchStatus is the provider's own report of the fetch (probe.Usage), kept
	// for analysis next to what our log saw.
	FetchStatus string `json:"fetch_status,omitempty"`
}

// CorrectionPage is one trial: one vendor, one format, one fresh page.
type CorrectionPage struct {
	PageID   string `json:"page_id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Format   string `json:"format"` // html | txt
	Trial    int    `json:"trial"`
	URL      string `json:"url"`
	Path     string `json:"path"` // page_key in probe.jsonl
	Status   string `json:"status"`
	Why      string `json:"why,omitempty"`

	WarmDueUnix float64 `json:"warm_due_unix"` // when the warm-up may start (random within the start spread)

	ETagA string `json:"etag_a,omitempty"` // what the page sends at version 1 / 2
	ETagB string `json:"etag_b,omitempty"`

	SetAUnix       float64 `json:"set_a_unix,omitempty"`
	FlipUnix       float64 `json:"flip_unix,omitempty"`
	FlipSwitchedAt string  `json:"flip_switched_at,omitempty"` // the server's own record of the switch
	LastErr        string  `json:"last_err,omitempty"`         // a step that will be retried next tick

	Warm   *CorrectionAsk   `json:"warm"`
	Reasks []*CorrectionAsk `json:"reasks"`
}

// CorrectionState is the whole run, persisted between ticks.
type CorrectionState struct {
	RunID       string            `json:"run_id"`
	Created     string            `json:"created"`
	Base        string            `json:"base"`
	Host        string            `json:"host"`
	LocalOrigin string            `json:"local_origin"`
	Size        int               `json:"size"`
	Settle      string            `json:"settle"`
	Force       bool              `json:"force"`
	WarmGap     string            `json:"warm_gap"`
	Jitter      bool              `json:"jitter"`
	Reask       map[string]string `json:"reask"` // vendor -> check times, as planned
	Pages       []*CorrectionPage `json:"pages"`
}

// versionReply is the control's reply (proxy/verpage.go versionControlReply).
type versionReply struct {
	ID           string                       `json:"id"`
	Version      int                          `json:"version"`
	SwitchedAt   string                       `json:"switched_at"`
	LastModified string                       `json:"last_modified"`
	Size         int                          `json:"size"`
	ETags        map[string]map[string]string `json:"etags"`
}

// correctionDeps is everything a tick touches outside the state, so a test can
// run the whole timeline with a fake clock, origin and model.
type correctionDeps struct {
	secret     string
	settle     time.Duration
	now        func() time.Time
	setVersion func(id string, version int) (versionReply, error)
	ask        func(p *CorrectionPage, question string) (string, probe.Usage, error)
	save       func(*CorrectionState) error
	// reachable checks the public site answers; nil = assume it does. Called at
	// most once per tick and only when a step is due, so an idle sitting sends the
	// server nothing (a down site must not read as "no new origin request").
	reachable func() bool
}

// --- planning ---------------------------------------------------------------

// parseReaskSpec reads "vendor=offsets;vendor=offsets;*=offsets". Offsets are
// durations after the switch, sorted. "*" covers every active vendor not named.
// There is no default: every active vendor must end up with at least one offset.
func parseReaskSpec(spec string, active []string) (map[string][]time.Duration, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, fmt.Errorf("no ask-again times given (e.g. \"or:openai=20m,40m;*=12h,24h\")")
	}
	isActive := map[string]bool{}
	for _, n := range active {
		isActive[n] = true
	}
	named := map[string][]time.Duration{}
	var fallback []time.Duration
	for _, entry := range strings.Split(spec, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, list, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("%q: want vendor=offsets", entry)
		}
		var offs []time.Duration
		for _, s := range strings.Split(list, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			d, err := time.ParseDuration(s)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", name, err)
			}
			if d <= 0 {
				return nil, fmt.Errorf("%s: offset %s must be after the switch", name, d)
			}
			offs = append(offs, d)
		}
		if len(offs) == 0 {
			return nil, fmt.Errorf("%s: no offsets", name)
		}
		sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })
		if name == "*" {
			if fallback != nil {
				return nil, fmt.Errorf("* given twice")
			}
			fallback = offs
			continue
		}
		if !isActive[name] {
			return nil, fmt.Errorf("%s is not an active vendor (%s)", name, strings.Join(active, ", "))
		}
		if _, dup := named[name]; dup {
			return nil, fmt.Errorf("%s given twice", name)
		}
		named[name] = offs
	}
	out := map[string][]time.Duration{}
	for _, n := range active {
		switch {
		case named[n] != nil:
			out[n] = named[n]
		case fallback != nil:
			out[n] = fallback
		default:
			return nil, fmt.Errorf("no ask-again times for %s (name it or add *=...)", n)
		}
	}
	return out, nil
}

// correctionLayout is what a plan is made of.
type correctionLayout struct {
	Formats []string
	Trials  int // pages per vendor x format x check time in this sitting
	Size    int
	Checks  map[string][]time.Duration // vendor -> sorted check times after the switch
	Start   time.Time                  // the first warm-up slot
	WarmGap time.Duration              // one warm-up per slot, this far apart, in shuffled order (0 = all at Start)
	Jitter  bool                       // draw each check's wait at random inside its step
	Rand    func() float64             // [0,1); nil = math/rand
}

// checkWindow is the step that check i owns on a sorted grid: from the geometric
// midpoint with the check before it to the geometric midpoint with the check after
// it. On a doubling grid that is nominal/√2 .. nominal·√2. The first and last
// checks mirror their inner side; a lone check gets the doubling-grid width.
// Windows are contiguous and never overlap, so a drawn wait always stays in its
// own step.
func checkWindow(grid []time.Duration, i int) (lo, hi time.Duration) {
	geoMid := func(a, b time.Duration) time.Duration {
		return time.Duration(math.Round(math.Sqrt(float64(a) * float64(b))))
	}
	t := float64(grid[i])
	switch {
	case len(grid) == 1:
		return time.Duration(math.Round(t / math.Sqrt2)), time.Duration(math.Round(t * math.Sqrt2))
	case i == 0:
		hi = geoMid(grid[0], grid[1])
		return time.Duration(math.Round(t * t / float64(hi))), hi
	case i == len(grid)-1:
		lo = geoMid(grid[i-1], grid[i])
		return lo, time.Duration(math.Round(t * t / float64(lo)))
	}
	return geoMid(grid[i-1], grid[i]), geoMid(grid[i], grid[i+1])
}

// drawWait picks a wait inside check i's step, uniform on a log scale (the grid is
// geometric, so each step gets equal weight across its width), to the second.
func drawWait(grid []time.Duration, i int, u float64) time.Duration {
	lo, hi := checkWindow(grid, i)
	w := float64(lo) * math.Pow(float64(hi)/float64(lo), u)
	d := time.Duration(w).Truncate(time.Second)
	if d < lo {
		d = lo.Truncate(time.Second) + time.Second
	}
	return d
}

// buildCorrectionPages is the plan: one fresh page per vendor x format x check
// time x trial, each with a single ask-again. The page id carries the caller tag
// (two models never share a copy) and the check time. Random draws happen here,
// once, and are stored in the pages.
func buildCorrectionPages(provs []Caller, base, runID string, lay correctionLayout) ([]*CorrectionPage, error) {
	if lay.Trials < 1 {
		return nil, fmt.Errorf("trials must be at least 1")
	}
	rnd := lay.Rand
	if rnd == nil {
		rnd = rand.Float64
	}
	base = strings.TrimRight(base, "/")
	var out []*CorrectionPage
	for _, p := range provs {
		grid := lay.Checks[p.Name]
		for _, f := range lay.Formats {
			prefix, ok := verRoutePrefix[f]
			if !ok {
				return nil, fmt.Errorf("unknown format %q (have html, txt)", f)
			}
			for ci, check := range grid {
				for tr := 1; tr <= lay.Trials; tr++ {
					id := fmt.Sprintf("%s-%s-cv-%s-w%s-t%d", p.Tag(), runID, f, compactDur(check), tr)
					path := prefix + id + "/" + strconv.Itoa(lay.Size)
					wait := check
					if lay.Jitter {
						wait = drawWait(grid, ci, rnd())
					}
					out = append(out, &CorrectionPage{
						PageID: id, Provider: p.Name, Model: p.Model, Format: f, Trial: tr,
						URL: base + path, Path: path, Status: corrNew, WarmDueUnix: unixf(lay.Start),
						Warm: &CorrectionAsk{Kind: "warm", Status: "pending", Question: correctionWarmQuestion},
						Reasks: []*CorrectionAsk{{Kind: "reask", Offset: check.String(), Wait: wait.String(),
							Status: "pending", Question: correctionReaskQuestion}},
					})
				}
			}
		}
	}
	// Warm-up slots. The loop sends one ask at a time, so each warm-up gets its own
	// slot, WarmGap apart (long enough for a slow ask to finish), in a shuffled
	// order: vendors and check times are mixed, so no vendor always warms first and
	// a slow vendor never queues the others' warm-ups behind it.
	slots := make([]int, len(out))
	for i := range slots {
		slots[i] = i
	}
	for i := len(slots) - 1; i > 0; i-- { // Fisher-Yates with the layout's source
		j := int(rnd() * float64(i+1))
		if j > i {
			j = i
		}
		slots[i], slots[j] = slots[j], slots[i]
	}
	for i, pg := range out {
		pg.WarmDueUnix = unixf(lay.Start.Add(time.Duration(slots[i]) * lay.WarmGap))
	}
	return out, nil
}

// actualWait is how long after the switch the ask-again was really sent — what the
// analysis uses. A queued or late tick moves it away from the planned wait.
func actualWait(a *CorrectionAsk, pg *CorrectionPage) time.Duration {
	s := a.WaitedS
	if s == 0 && a.SentUnix > 0 && pg.FlipUnix > 0 {
		s = a.SentUnix - pg.FlipUnix
	}
	if s == 0 {
		return mustDur(a.Wait)
	}
	return time.Duration(s * float64(time.Second)).Round(time.Second)
}

// correctionLockFor is the lock for one sitting's state file: next to it, named
// after it, so two sittings never block each other and one sitting never runs twice.
func correctionLockFor(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "."+filepath.Base(statePath)+".lock")
}

// --- reading an answer --------------------------------------------------------

// classifyVersion reads which version an answer holds from the codes it quotes:
// any of the five depths of version 1 (A) or version 2 (B), case-folded.
func classifyVersion(answer, secret, id string) (version string, depthsA, depthsB []int) {
	for _, pct := range probe.DepthPercents {
		if c := probe.DepthCode(secret, id, pct, 1); c != "" && containsFold(answer, c) {
			depthsA = append(depthsA, pct)
		}
		if c := probe.DepthCode(secret, id, pct, 2); c != "" && containsFold(answer, c) {
			depthsB = append(depthsB, pct)
		}
	}
	switch {
	case len(depthsA) > 0 && len(depthsB) > 0:
		return "both", depthsA, depthsB
	case len(depthsA) > 0:
		return "A", depthsA, depthsB
	case len(depthsB) > 0:
		return "B", depthsA, depthsB
	}
	return "neither", depthsA, depthsB
}

// --- the tick -------------------------------------------------------------------

// tickCorrection fires every step now due, earliest first, saving after each. A
// step that fails on our side (the control unreachable) is left for the next tick.
func tickCorrection(st *CorrectionState, d correctionDeps) {
	tried := map[*CorrectionPage]bool{} // one failed step per page per tick, not a hot loop
	checked := false
	for {
		pg, due := nextDue(st, d, tried)
		if pg == nil || due > unixf(d.now()) {
			return
		}
		// Something is due: check the site once for this tick before acting.
		if !checked {
			checked = true
			if d.reachable != nil && !d.reachable() {
				return // fire nothing; the same steps are due again next tick
			}
		}
		ok := stepCorrection(st, pg, d)
		if !ok {
			tried[pg] = true
		}
		if err := d.save(st); err != nil {
			fmt.Fprintf(os.Stderr, "[correction] state save failed: %v\n", err)
		}
	}
}

// nextDue finds the page whose next step is due soonest.
func nextDue(st *CorrectionState, d correctionDeps, skip map[*CorrectionPage]bool) (*CorrectionPage, float64) {
	var best *CorrectionPage
	bestDue := 0.0
	for _, pg := range st.Pages {
		if skip[pg] {
			continue
		}
		due, ok := stepDue(pg, d.settle)
		if !ok {
			continue
		}
		if best == nil || due < bestDue {
			best, bestDue = pg, due
		}
	}
	return best, bestDue
}

// stepDue is when the page's next step is due (ok=false: nothing left to do).
func stepDue(pg *CorrectionPage, settle time.Duration) (float64, bool) {
	switch pg.Status {
	case corrNew:
		return pg.WarmDueUnix, true
	case corrWarmed:
		return pg.Warm.DoneUnix + settle.Seconds(), true
	case corrFlipped:
		for _, a := range pg.Reasks {
			if a.Status == "pending" {
				return a.DueUnix, true
			}
		}
	}
	return 0, false
}

// stepCorrection does the page's next step. It reports false when the step could
// not be done for a reason on our side (to retry on a later tick).
func stepCorrection(st *CorrectionState, pg *CorrectionPage, d correctionDeps) bool {
	switch pg.Status {
	case corrNew:
		rep, err := d.setVersion(pg.PageID, 1)
		if err != nil {
			pg.LastErr = "set version 1: " + err.Error()
			fmt.Fprintf(os.Stderr, "[correction] %s: %s (retry next tick)\n", pg.PageID, pg.LastErr)
			return false
		}
		pg.ETagA, pg.ETagB = rep.ETags[pg.Format]["1"], rep.ETags[pg.Format]["2"]
		pg.SetAUnix = unixf(d.now())
		doAsk(pg, pg.Warm, d)
		pg.LastErr = ""
		switch {
		case pg.Warm.Err != "":
			pg.Status, pg.Why = corrVoid, "warm-up ask failed: "+pg.Warm.Err
		case pg.Warm.Version != "A":
			pg.Status, pg.Why = corrVoid, "warm-up answer quoted no version-1 code ("+pg.Warm.Version+"), so the model never held A"
		default:
			pg.Status = corrWarmed
		}
		fmt.Printf("  warm-up  %-13s %-4s t%d  answer=%-7s -> %s\n", pg.Provider, pg.Format, pg.Trial, pg.Warm.Version, pg.Status)
		return true

	case corrWarmed:
		rep, err := d.setVersion(pg.PageID, 2)
		if err != nil {
			pg.LastErr = "set version 2: " + err.Error()
			fmt.Fprintf(os.Stderr, "[correction] %s: %s (retry next tick)\n", pg.PageID, pg.LastErr)
			return false
		}
		if rep.Version != 2 {
			pg.LastErr = fmt.Sprintf("control answered version %d, want 2", rep.Version)
			return false
		}
		now := d.now()
		pg.FlipUnix, pg.FlipSwitchedAt, pg.LastErr = unixf(now), rep.SwitchedAt, ""
		for _, a := range pg.Reasks {
			wait, err := time.ParseDuration(a.Wait)
			if err != nil { // a state planned before Wait existed: the nominal time
				wait, _ = time.ParseDuration(a.Offset)
			}
			a.DueUnix = unixf(now.Add(wait))
		}
		pg.Status = corrFlipped
		fmt.Printf("  SWITCH   %-13s %-4s t%d  -> version 2 at %s\n", pg.Provider, pg.Format, pg.Trial, now.UTC().Format("15:04:05"))
		return true

	case corrFlipped:
		var a *CorrectionAsk
		for _, x := range pg.Reasks {
			if x.Status == "pending" {
				a = x
				break
			}
		}
		if a == nil {
			pg.Status = corrDone
			return true
		}
		// Confirm the origin still serves version 2 from the SAME switch before
		// spending an ask: a re-set version 2 is a no-op that returns the server's
		// own switch time. A different time means the origin lost the state and
		// re-made the page — the "correction" would then be a different event.
		rep, err := d.setVersion(pg.PageID, 2)
		if err != nil {
			pg.LastErr = "confirm version 2: " + err.Error()
			fmt.Fprintf(os.Stderr, "[correction] %s: %s (retry next tick)\n", pg.PageID, pg.LastErr)
			return false
		}
		if rep.SwitchedAt != pg.FlipSwitchedAt {
			pg.Status = corrVoid
			pg.Why = fmt.Sprintf("origin's switch time changed (%s -> %s): it lost the page state", pg.FlipSwitchedAt, rep.SwitchedAt)
			return true
		}
		pg.LastErr = ""
		doAsk(pg, a, d)
		fmt.Printf("  ask again (check %s, asked at switch + %s)  %-13s %-4s t%d  answer=%s\n", compactDur(mustDur(a.Offset)), actualWait(a, pg), pg.Provider, pg.Format, pg.Trial, a.Version)
		last := true
		for _, x := range pg.Reasks {
			if x.Status == "pending" {
				last = false
			}
		}
		if last {
			pg.Status = corrDone
		}
		return true
	}
	return true
}

// doAsk asks the question and records the answer and which version it holds.
func doAsk(pg *CorrectionPage, a *CorrectionAsk, d correctionDeps) {
	a.SentUnix = unixf(d.now())
	if a.Kind == "reask" && pg.FlipUnix > 0 {
		a.WaitedS = a.SentUnix - pg.FlipUnix
	}
	q := a.Question
	if q == "" { // a state planned before per-ask questions existed
		q = correctionWarmQuestion
	}
	ans, usage, err := d.ask(pg, q)
	a.DoneUnix = unixf(d.now())
	a.Status = "done"
	a.Answer = truncate(ans, 2000)
	if err != nil {
		a.Err = err.Error()
	}
	a.InTok, a.OutTok, a.Rounds = usage.InputTokens, usage.OutputTokens, usage.Rounds
	a.FetchStatus = usage.FetchStatus
	a.Version, a.DepthsA, a.DepthsB = classifyVersion(ans, d.secret, pg.PageID)
}

// correctionFinished reports whether every page is done or void.
func correctionFinished(st *CorrectionState) bool {
	for _, pg := range st.Pages {
		if pg.Status != corrDone && pg.Status != corrVoid {
			return false
		}
	}
	return true
}

// --- reading the log -----------------------------------------------------------

// FetchLabel is one logged request for a page, labelled by what we served.
type FetchLabel struct {
	TSUnix float64 `json:"ts_unix"`
	Bot    string  `json:"bot"`
	Status int     `json:"status"`
	Served string  `json:"served"` // A | B | not-modified | other
}

// labelFetches lists every request for the page on our host, in log order, and
// labels it by the tag we sent. Our own probes (bot "probe-…") never count.
func labelFetches(recs []probe.Record, pg *CorrectionPage, host string) []FetchLabel {
	var out []FetchLabel
	for _, r := range recs {
		if r.PageKey != pg.Path || hostOfURL(r.URL) != host || strings.HasPrefix(r.Bot, "probe-") {
			continue
		}
		served := "other"
		switch {
		case r.StatusCode == http.StatusNotModified:
			served = "not-modified"
		case r.StatusCode == http.StatusOK && r.ETagSent == pg.ETagA:
			served = "A"
		case r.StatusCode == http.StatusOK && r.ETagSent == pg.ETagB:
			served = "B"
		}
		out = append(out, FetchLabel{TSUnix: r.TSUnix, Bot: r.Bot, Status: r.StatusCode, Served: served})
	}
	return out
}

// reaskOutcome is "<what reached us during this ask> / <what the answer quoted>".
// Only fetches between sending the question and the answer coming back count: a
// later fetch cannot explain this answer. The three headline states are
// "no fetch / answer A", "fetch B / answer B" and "fetch B / answer A".
func reaskOutcome(a *CorrectionAsk, fetches []FetchLabel) string {
	if a.Status != "done" {
		return "not asked"
	}
	fetch := "no fetch"
	for _, f := range fetches {
		if f.TSUnix < a.SentUnix || f.TSUnix > a.DoneUnix {
			continue
		}
		switch f.Served {
		case "B":
			fetch = "fetch B"
		case "not-modified":
			if fetch == "no fetch" {
				fetch = "not modified"
			}
		case "A", "other":
			if fetch == "no fetch" {
				fetch = "fetch " + f.Served
			}
		}
	}
	answer := "answer " + a.Version
	switch {
	case a.Err != "":
		answer = "ask failed"
	case a.Version == "neither":
		answer = "no code"
	case a.Version == "both":
		answer = "both codes"
	}
	return fetch + " / " + answer
}

// firstEvents returns the two events the test is about, as unix times (0 = not
// seen): the first request after the switch served B's bytes, and the first
// ask-again answer that quoted B.
func firstEvents(pg *CorrectionPage, fetches []FetchLabel) (firstBFetch, firstBAnswer float64) {
	for _, f := range fetches {
		if f.Served == "B" && f.TSUnix >= pg.FlipUnix {
			firstBFetch = f.TSUnix
			break
		}
	}
	for _, a := range pg.Reasks {
		if a.Status == "done" && a.Version == "B" {
			firstBAnswer = a.DoneUnix
			break
		}
	}
	return firstBFetch, firstBAnswer
}

// --- real deps, entry points ------------------------------------------------------

// postVersion calls the origin's control. It goes to the LOCAL origin address,
// like the retention health check, because our own Go client trips Cloudflare's
// bot management on the public URL.
func postVersion(localOrigin, secret string, size int) func(id string, version int) (versionReply, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	return func(id string, version int) (versionReply, error) {
		form := url.Values{"id": {id}, "version": {strconv.Itoa(version)}, "size": {strconv.Itoa(size)}}
		req, err := http.NewRequest(http.MethodPost, strings.TrimRight(localOrigin, "/")+"/control/version", strings.NewReader(form.Encode()))
		if err != nil {
			return versionReply{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Probe-Control-Secret", secret)
		resp, err := client.Do(req)
		if err != nil {
			return versionReply{}, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return versionReply{}, fmt.Errorf("control: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var rep versionReply
		if err := json.Unmarshal(body, &rep); err != nil {
			return versionReply{}, fmt.Errorf("control reply: %v", err)
		}
		return rep, nil
	}
}

func saveCorrectionState(path string, st *CorrectionState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path) // atomic
}

func loadCorrectionState(path string) (*CorrectionState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st CorrectionState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// planCorrection writes the state file. It calls no API and touches no page: the
// first tick at or after each page's warm-up time sets it to version 1 and asks.
func planCorrection(base, localOrigin string, provs []Caller, formats []string, trials, size int,
	settle, warmGap time.Duration, startAt string, jitter bool, reaskSpec string, force bool,
	statePath, secret, controlSecret string, dryRun bool) int {

	if base == "" || hostOfURL(base) == "" {
		fmt.Fprintln(os.Stderr, "error: -siteUrl must be the origin base, e.g. https://probe.example.org")
		return 1
	}
	if !dryRun && (secret == "" || controlSecret == "") {
		fmt.Fprintln(os.Stderr, "error: needs -pageCodeSecret and -controlSecret (both must match the origin)")
		return 1
	}
	if settle <= 0 {
		fmt.Fprintln(os.Stderr, "error: -correction-switchDelay must be positive")
		return 1
	}
	var names []string
	for _, p := range provs {
		names = append(names, p.Name)
	}
	reask, err := parseReaskSpec(reaskSpec, names)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: -correction-askAgainTimes: %v\n", err)
		return 1
	}
	if !dryRun {
		if _, err := os.Stat(statePath); err == nil {
			fmt.Fprintf(os.Stderr, "error: state file %s already exists — refusing to overwrite a run\n", statePath)
			return 1
		}
	}
	if warmGap < 0 {
		fmt.Fprintln(os.Stderr, "error: -correction-firstAskGap must not be negative")
		return 1
	}
	runID := runNonce()
	now := time.Now().UTC()
	start, err := parseStartAt(startAt, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: -correction-startAt: %v\n", err)
		return 1
	}
	if start.After(now) {
		fmt.Printf("[correction] first warm-up slot: %s (a loop started now waits until then)\n",
			start.In(time.FixedZone("KSA", 3*3600)).Format("Mon 02 Jan 15:04 KSA"))
	}
	pages, err := buildCorrectionPages(provs, base, runID, correctionLayout{
		Formats: formats, Trials: trials, Size: size, Checks: reask,
		Start: start, WarmGap: warmGap, Jitter: jitter,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	st := &CorrectionState{
		RunID: runID, Created: now.Format(time.RFC3339), Base: base, Host: hostOfURL(base),
		LocalOrigin: localOrigin, Size: size, Settle: settle.String(), Force: force,
		WarmGap: warmGap.String(), Jitter: jitter, Reask: map[string]string{}, Pages: pages,
	}
	asks := 0
	for _, pg := range pages {
		asks += 1 + len(pg.Reasks)
	}
	fmt.Printf("[correction] run %s: %d page(s) = one per vendor x %v x check time x %d trial(s); %d B\n",
		runID, len(pages), formats, trials, size)
	fmt.Printf("[correction] one warm-up every %s in shuffled order; switch %s after each warm-up answer; check times drawn inside their step: %v\n",
		warmGap, settle, jitter)
	for _, n := range names {
		var s []string
		for _, d := range reask[n] {
			s = append(s, compactDur(d))
		}
		st.Reask[n] = strings.Join(s, ",")
		fmt.Printf("  %-13s check at switch + %s\n", n, st.Reask[n])
	}
	fmt.Printf("[correction] %d paid asks in total (each page: 1 warm-up + 1 ask-again)\n", asks)
	fmt.Printf("[correction] warm-up question : %s\n", correctionWarmQuestion)
	fmt.Printf("[correction] ask-again question: %s\n", correctionReaskQuestion)
	fmt.Println("[correction] pages (warm-up time KSA, check, drawn wait):")
	ksa := time.FixedZone("KSA", 3*3600)
	for _, pg := range pages {
		wu := time.Unix(int64(pg.WarmDueUnix), 0).In(ksa)
		fmt.Printf("  %-13s %-4s t%d  warm %s  check %-4s wait %s\n", pg.Provider, pg.Format, pg.Trial,
			wu.Format("Mon 15:04"), compactDur(mustDur(pg.Reasks[0].Offset)), pg.Reasks[0].Wait)
	}
	if dryRun {
		fmt.Println("[correction] DRY RUN — no state written, no API call.")
		return 0
	}
	if err := saveCorrectionState(statePath, st); err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot write state %s: %v\n", statePath, err)
		return 1
	}
	fmt.Printf("[correction] state written to %s. Nothing has been asked yet; run -correction-loop (tmux) or -correction-step (cron).\n", statePath)
	return 0
}

// realCorrectionDeps wires the tick to the live origin and the providers.
func realCorrectionDeps(st *CorrectionState, provs []Caller, secret, controlSecret, statePath string) (correctionDeps, error) {
	settle, err := time.ParseDuration(st.Settle)
	if err != nil {
		return correctionDeps{}, fmt.Errorf("state settle %q: %v", st.Settle, err)
	}
	byName := map[string]Caller{}
	for _, p := range provs {
		byName[p.Name] = p
	}
	return correctionDeps{
		secret:     secret,
		settle:     settle,
		now:        time.Now,
		setVersion: postVersion(st.LocalOrigin, controlSecret, st.Size),
		ask: func(pg *CorrectionPage, question string) (string, probe.Usage, error) {
			p, ok := byName[pg.Provider]
			if !ok {
				return "", probe.Usage{}, fmt.Errorf("provider %s not active (missing key or -vendors?)", pg.Provider)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			return p.Ask(ctx, os.Getenv(p.EnvKey), pg.Model, pg.URL, question, st.Force)
		},
		save: func(s *CorrectionState) error { return saveCorrectionState(statePath, s) },
	}, nil
}

// runCorrectionTick fires what is due once (cron), or with loop=true keeps
// ticking every minute until every page is done or void (tmux).
func runCorrectionTick(provs []Caller, statePath, secret, controlSecret string, loop bool) int {
	if secret == "" || controlSecret == "" {
		fmt.Fprintln(os.Stderr, "error: needs -pageCodeSecret and -controlSecret (both must match the origin)")
		return 1
	}
	lock, err := acquireRunLock(correctionLockFor(statePath), "correction run tick", []string{"changeable pages (/verhtmlpage/, /vertxtpage/) of " + statePath})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[correction] tick skipped — lock held: %v\n", err)
		return 0
	}
	defer lock.Release()

	for {
		st, err := loadCorrectionState(statePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot read state %s: %v\n", statePath, err)
			return 1
		}
		// Only the provider of a pending page is needed; a page whose provider is
		// not active fails its ask and voids, so refuse up front instead.
		for _, pg := range st.Pages {
			if pg.Status == corrDone || pg.Status == corrVoid {
				continue
			}
			found := false
			for _, p := range provs {
				found = found || p.Name == pg.Provider
			}
			if !found {
				fmt.Fprintf(os.Stderr, "error: page %s needs provider %s, which is not active\n", pg.PageID, pg.Provider)
				return 1
			}
		}
		d, err := realCorrectionDeps(st, provs, secret, controlSecret, statePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		// A down site logs NO fetch, which would read as "held the old copy", so a
		// due step waits for the site to answer. The tick checks only when
		// something is due — an idle minute sends the server nothing.
		d.reachable = func() bool {
			if publicReachable(st.Base) {
				return true
			}
			fmt.Fprintf(os.Stderr, "[correction] %s: public origin %s not responding — nothing fired\n", time.Now().UTC().Format("15:04:05"), st.Base)
			return false
		}
		tickCorrection(st, d)
		if correctionFinished(st) {
			fmt.Println("[correction] every page is done or void. Report: -correction-report")
			return 0
		}
		if !loop {
			return 0
		}
		time.Sleep(time.Minute)
	}
}

// reportCorrection prints each page's timeline and the outcome counts, reading
// which version each fetch got from the probe log. Safe on a partly-run state.
func reportCorrection(st *CorrectionState, recs []probe.Record) int {
	fmt.Println(strings.Repeat("=", 78))
	fmt.Printf("  CORRECTION RUN %s — when does version B reach an answer?\n", st.RunID)
	fmt.Println(strings.Repeat("=", 78))
	type key struct{ prov, format string }
	counts := map[key]map[string]int{}
	var keys []key
	for _, pg := range st.Pages {
		fetches := labelFetches(recs, pg, st.Host)
		k := key{pg.Provider, pg.Format}
		if counts[k] == nil {
			counts[k] = map[string]int{}
			keys = append(keys, k)
		}
		fmt.Printf("\n%s  %s  trial %d  [%s]\n", pg.Provider, pg.Format, pg.Trial, pg.Status)
		fmt.Printf("  %s\n", pg.URL)
		if pg.Why != "" {
			fmt.Printf("  why: %s\n", pg.Why)
		}
		if pg.Warm.Status == "done" {
			fmt.Printf("  warm-up: answer %s, %d fetch(es) during it\n", pg.Warm.Version, len(fetchesIn(fetches, pg.Warm)))
		}
		for _, a := range pg.Reasks {
			o := reaskOutcome(a, fetches)
			if a.Status == "done" {
				counts[k][o]++
			}
			fmt.Printf("  %s: %s\n", checkLabel(a, pg), o)
		}
		if pg.FlipUnix > 0 {
			fb, ab := firstEvents(pg, fetches)
			fmt.Printf("  first fetch of B: %s   first answer with B: %s\n", sinceFlip(pg.FlipUnix, fb), sinceFlip(pg.FlipUnix, ab))
		}
	}
	fmt.Println("\n" + strings.Repeat("-", 78))
	fmt.Println("  Outcome of each ask-again, by vendor and file type (count of asks)")
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].prov != keys[j].prov {
			return keys[i].prov < keys[j].prov
		}
		return keys[i].format < keys[j].format
	})
	for _, k := range keys {
		var outs []string
		for o := range counts[k] {
			outs = append(outs, o)
		}
		sort.Strings(outs)
		fmt.Printf("  %-13s %-4s", k.prov, k.format)
		for _, o := range outs {
			fmt.Printf("  [%s] x%d", o, counts[k][o])
		}
		fmt.Println()
	}
	return 0
}

// parseStartAt reads -correction-startAt: when a sitting's first warm-up slot
// is. Empty = now. The time must carry an explicit zone (a bare "10:00" is
// ambiguous between Riyadh and UTC), and a time in the past or more than 30 days
// out is refused as a likely typo.
func parseStartAt(s string, now time.Time) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return now, nil
	}
	var t time.Time
	var err error
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00"} {
		if t, err = time.Parse(layout, s); err == nil {
			break
		}
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("%q: want a time with its zone, e.g. 2026-10-15T10:00+03:00", s)
	}
	if t.Before(now.Add(-time.Minute)) {
		return time.Time{}, fmt.Errorf("%q is in the past", s)
	}
	if t.After(now.Add(30 * 24 * time.Hour)) {
		return time.Time{}, fmt.Errorf("%q is more than 30 days out", s)
	}
	return t, nil
}

// checkLabel is how the report names a check: the actual wait once it was asked,
// the planned one (labelled as planned) before.
func checkLabel(a *CorrectionAsk, pg *CorrectionPage) string {
	check := compactDur(mustDur(a.Offset))
	if a.Status != "done" {
		return fmt.Sprintf("check %-4s (planned at switch + %s)", check, mustDur(a.Wait))
	}
	return fmt.Sprintf("check %-4s (actually asked at switch + %s)", check, actualWait(a, pg))
}

// mustDur parses a duration the runner itself wrote; a bad one prints as 0s.
func mustDur(s string) time.Duration {
	d, _ := time.ParseDuration(s)
	return d
}

func fetchesIn(fetches []FetchLabel, a *CorrectionAsk) []FetchLabel {
	var out []FetchLabel
	for _, f := range fetches {
		if f.TSUnix >= a.SentUnix && f.TSUnix <= a.DoneUnix {
			out = append(out, f)
		}
	}
	return out
}

func sinceFlip(flip, t float64) string {
	if t == 0 {
		return "not seen"
	}
	return "switch + " + time.Duration((t-flip)*float64(time.Second)).Round(time.Second).String()
}
