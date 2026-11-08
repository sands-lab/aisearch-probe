package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// THE REMEMBERED-FAILURE RUN (heal redo) — after a broken page is fixed, how long
// does a vendor keep answering "it's broken"? (doc/handoff-cacheheader-day2-and-heal.md
// Part 2; design agreed 2026-10-22.)
//
// Each trial is one fresh healable page (/heal/, proxy/healpage.go) for ONE
// check time and ONE failure, asked about by one vendor:
//
//	1. at its (shuffled) first-ask slot, create the page BROKEN with its failure
//	   (503, 404 or 403) through the control, and ask once (the "first ask")
//	2. wait SETTLE after that answer came back — a timer, never a count of fetches
//	   (xAI sends 4-7 requests per ask)
//	3. FIX the page through the control; the server records the exact time
//	4. at its check time after the fix, confirm through the control that the page is
//	   still fixed at the same time, and ask again ONCE
//
// One page per check time, a times-2 grid with each wait drawn at random inside its
// own step, and shuffled first-ask slots: the convention of the correction run
// (doc/correction-visibility-method.md), whose helpers this reuses (checkWindow,
// drawWait, parseStartAt). Old /fault/heal/<sec> healed a set time after the first
// request, which several-requests-per-ask vendors blur; here the fix time is ours.
//
// NOTHING IS SKIPPED. A failed call, a first ask that never saw the failure, a fixed
// page fetched before its check, or a changed fix time is NOTED and the page runs to
// the end (the user's rule: vendors sometimes behave oddly, and the full run is the
// record). The report reads each re-ask as fetch yes/no x code yes/no.
//
// The questions are the shared ones (sharingAskQuestion, as the sharing and
// correction runs ask): the first ask names the depth-95% code (the page is broken,
// so there is none to quote), the re-ask the depth-50% code. "Code yes" = the answer
// quotes any of the fixed page's five codes, which did not exist anywhere before the
// fix reached the fetcher.
//
// Anthropic uses the web fetch tool WITHOUT dynamic filtering (web_fetch_20250910),
// pinned in the state at planning and applied on every tick: on 18 Oct the two tool
// versions gave identical caching behaviour, and the filtering one costs about ten
// times more per call on a 2 KB page.

// defaultHealFetchVersion is the Anthropic tool the run uses unless planned otherwise.
const defaultHealFetchVersion = "web_fetch_20250910"

// defaultHealChecks is the agreed grid (2026-10-22): doubling from 5 min; the last
// step, 10 h 40, has a window reaching about 15 h ("around 16 h is good enough").
const defaultHealChecks = "5m,10m,20m,40m,1h20m,2h40m,5h20m,10h40m"

var (
	healWarmQuestion  = sharingAskQuestion(probe.DepthPercents[len(probe.DepthPercents)-1])
	healReaskQuestion = sharingAskQuestion(50)
)

// supportedHealFails mirrors the origin's healFailures (proxy/healpage.go).
var supportedHealFails = map[string]bool{"503": true, "404": true, "403": true}

// Page status, in order.
const (
	healNew     = "new"     // planned; not yet created or asked
	healWarmed  = "warmed"  // created broken and asked once; the fix is pending
	healFlipped = "flipped" // fixed; the re-ask is pending
	healDone    = "done"    // re-asked
)

// HealAsk is one question to one page.
type HealAsk struct {
	Kind     string  `json:"kind"`               // "first" | "reask"
	Check    string  `json:"check,omitempty"`    // reask: the nominal check time (the grid step)
	Wait     string  `json:"wait,omitempty"`     // reask: the drawn wait after the fix (inside the step)
	WaitedS  float64 `json:"waited_s,omitempty"` // reask: seconds from the fix to actually sending
	Question string  `json:"question"`
	DueUnix  float64 `json:"due_unix,omitempty"`
	Status   string  `json:"status"` // pending | done
	SentUnix float64 `json:"sent_unix,omitempty"`
	DoneUnix float64 `json:"done_unix,omitempty"`
	Answer   string  `json:"answer,omitempty"`
	Err      string  `json:"err,omitempty"`
	InTok    int     `json:"in_tok,omitempty"`
	OutTok   int     `json:"out_tok,omitempty"`
	Rounds   int     `json:"rounds,omitempty"`
}

// HealPage is one trial: one vendor, one failure, one check time, one fresh page.
type HealPage struct {
	PageID   string   `json:"page_id"`
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Fail     string   `json:"fail"`
	Trial    int      `json:"trial"`
	URL      string   `json:"url"`
	Path     string   `json:"path"` // page_key in probe.jsonl
	Status   string   `json:"status"`
	Notes    []string `json:"notes,omitempty"` // things noticed while running; never a reason to skip

	WarmDueUnix  float64 `json:"warm_due_unix"`
	ETagFixed    string  `json:"etag_fixed,omitempty"` // what the fixed page sends (from the control)
	CreatedAt    string  `json:"created_at,omitempty"` // the server's own record
	FlipUnix     float64 `json:"flip_unix,omitempty"`
	FlipHealedAt string  `json:"flip_healed_at,omitempty"` // the server's own record of the fix
	LastErr      string  `json:"last_err,omitempty"`       // a control step to retry next tick

	Warm  *HealAsk `json:"warm"`
	Reask *HealAsk `json:"reask"`
}

// HealState is the whole sitting, persisted between ticks.
type HealState struct {
	RunID                 string      `json:"run_id"`
	Created               string      `json:"created"`
	Base                  string      `json:"base"`
	Host                  string      `json:"host"`
	LocalOrigin           string      `json:"local_origin"`
	Size                  int         `json:"size"`
	Settle                string      `json:"settle"`
	Force                 bool        `json:"force"`
	WarmGap               string      `json:"warm_gap"`
	Jitter                bool        `json:"jitter"`
	Checks                string      `json:"checks"`
	Fails                 []string    `json:"fails"`
	AnthropicFetchVersion string      `json:"anthropic_fetch_version"`
	Pages                 []*HealPage `json:"pages"`
}

// healReply is the control's reply (proxy/healpage.go healControlReply).
type healReply struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	Fail         string `json:"fail"`
	CreatedAt    string `json:"created_at"`
	HealedAt     string `json:"healed_at"`
	LastModified string `json:"last_modified"`
	Size         int    `json:"size"`
	ETag         string `json:"etag"`
}

// healDeps is everything a tick touches outside the state, so a test can run the
// whole timeline with a fake clock, origin and model.
type healDeps struct {
	secret  string
	settle  time.Duration
	now     func() time.Time
	control func(id, action, fail string) (healReply, error)
	ask     func(p *HealPage, question string) (string, probe.Usage, error)
	save    func(*HealState) error
}

// --- planning ---------------------------------------------------------------

// parseHealChecks reads "2m,4m,1h": positive, distinct, returned sorted.
func parseHealChecks(spec string) ([]time.Duration, error) {
	var out []time.Duration
	seen := map[time.Duration]bool{}
	for _, s := range strings.Split(spec, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, err
		}
		if d <= 0 {
			return nil, fmt.Errorf("check %s must be after the fix", d)
		}
		if seen[d] {
			return nil, fmt.Errorf("check %s given twice", d)
		}
		seen[d] = true
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no check times given")
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// parseHealFails reads "503,404,403" in the order given.
func parseHealFails(spec string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, s := range strings.Split(spec, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if !supportedHealFails[s] {
			return nil, fmt.Errorf("failure %q is not one the origin serves (503, 404, 403)", s)
		}
		if seen[s] {
			return nil, fmt.Errorf("failure %s given twice", s)
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no failures given")
	}
	return out, nil
}

// healLayout is what a plan is made of.
type healLayout struct {
	Checks  []time.Duration // sorted check times after the fix
	Fails   []string
	Trials  int
	Size    int
	Start   time.Time      // the first first-ask slot
	WarmGap time.Duration  // one first ask per slot, this far apart, shuffled
	Jitter  bool           // draw each check's wait at random inside its step
	Rand    func() float64 // [0,1); nil = math/rand
}

// buildHealPages is the plan: one fresh page per vendor x check time x failure x
// trial. The id carries the caller tag, the failure and the check time — the id is
// ours to read; the fetcher sees the same address shape for every failure. Random
// draws happen here, once, and are stored.
func buildHealPages(provs []Caller, base, runID string, lay healLayout) ([]*HealPage, error) {
	if lay.Trials < 1 {
		return nil, fmt.Errorf("trials must be at least 1")
	}
	for _, f := range lay.Fails {
		if !supportedHealFails[f] {
			return nil, fmt.Errorf("failure %q is not one the origin serves (503, 404, 403)", f)
		}
	}
	rnd := lay.Rand
	if rnd == nil {
		rnd = rand.Float64
	}
	base = strings.TrimRight(base, "/")
	var out []*HealPage
	for _, p := range provs {
		for ci, check := range lay.Checks {
			for _, fail := range lay.Fails {
				for tr := 1; tr <= lay.Trials; tr++ {
					id := fmt.Sprintf("%s-%s-heal%s-w%s-t%d", p.Tag(), runID, fail, compactDur(check), tr)
					path := healPagePrefixClient + id + "/" + strconv.Itoa(lay.Size)
					wait := check
					if lay.Jitter {
						wait = drawWait(lay.Checks, ci, rnd())
					}
					out = append(out, &HealPage{
						PageID: id, Provider: p.Name, Model: p.Model, Fail: fail, Trial: tr,
						URL: base + path, Path: path, Status: healNew,
						Warm:  &HealAsk{Kind: "first", Status: "pending", Question: healWarmQuestion},
						Reask: &HealAsk{Kind: "reask", Check: check.String(), Wait: wait.String(), Status: "pending", Question: healReaskQuestion},
					})
				}
			}
		}
	}
	// First-ask slots: one per slot, WarmGap apart, shuffled so vendors, failures and
	// check times are mixed (as in the correction run).
	slots := make([]int, len(out))
	for i := range slots {
		slots[i] = i
	}
	for i := len(slots) - 1; i > 0; i-- {
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

// healPagePrefixClient is the origin's healable-page route (proxy/healpage.go).
const healPagePrefixClient = "/heal/"

func healLockFor(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "."+filepath.Base(statePath)+".lock")
}

// --- the tick -------------------------------------------------------------------

// tickHeal fires every step now due, earliest first, saving after each. A control
// call that fails (our side) is left for the next tick; nothing else stops a page.
func tickHeal(st *HealState, d healDeps) {
	tried := map[*HealPage]bool{}
	for {
		var pg *HealPage
		due := 0.0
		for _, p := range st.Pages {
			if tried[p] {
				continue
			}
			if t, ok := healStepDue(p, d.settle); ok && (pg == nil || t < due) {
				pg, due = p, t
			}
		}
		if pg == nil || due > unixf(d.now()) {
			return
		}
		if !stepHeal(pg, d) {
			tried[pg] = true
		}
		if err := d.save(st); err != nil {
			fmt.Fprintf(os.Stderr, "[heal] state save failed: %v\n", err)
		}
	}
}

func healStepDue(pg *HealPage, settle time.Duration) (float64, bool) {
	switch pg.Status {
	case healNew:
		return pg.WarmDueUnix, true
	case healWarmed:
		return pg.Warm.DoneUnix + settle.Seconds(), true
	case healFlipped:
		return pg.Reask.DueUnix, true
	}
	return 0, false
}

// stepHeal does the page's next step. false = a control call failed; retry next tick.
func stepHeal(pg *HealPage, d healDeps) bool {
	switch pg.Status {
	case healNew:
		rep, err := d.control(pg.PageID, "create", pg.Fail)
		if err != nil {
			pg.LastErr = "create: " + err.Error()
			fmt.Fprintf(os.Stderr, "[heal] %s: %s (retry next tick)\n", pg.PageID, pg.LastErr)
			return false
		}
		pg.ETagFixed, pg.CreatedAt, pg.LastErr = rep.ETag, rep.CreatedAt, ""
		healAsk(pg, pg.Warm, d)
		pg.Status = healWarmed
		fmt.Printf("  first ask %-13s fail=%s check=%-4s  %s\n", pg.Provider, pg.Fail, compactDur(mustDur(pg.Reask.Check)), healAskSummary(pg.Warm))
		return true

	case healWarmed:
		rep, err := d.control(pg.PageID, "heal", "")
		if err != nil {
			pg.LastErr = "heal: " + err.Error()
			fmt.Fprintf(os.Stderr, "[heal] %s: %s (retry next tick)\n", pg.PageID, pg.LastErr)
			return false
		}
		now := d.now()
		pg.FlipUnix, pg.FlipHealedAt, pg.LastErr = unixf(now), rep.HealedAt, ""
		pg.Reask.DueUnix = unixf(now.Add(mustDur(pg.Reask.Wait)))
		pg.Status = healFlipped
		fmt.Printf("  FIXED     %-13s fail=%s check=%-4s  at %s\n", pg.Provider, pg.Fail, compactDur(mustDur(pg.Reask.Check)), now.UTC().Format("15:04:05"))
		return true

	case healFlipped:
		// Confirm the origin still has the page fixed at the SAME time. A repeated
		// fix is a no-op that returns the server's own fix time; a different time
		// means it lost the page and re-made it. Noted, never skipped.
		rep, err := d.control(pg.PageID, "heal", "")
		if err != nil {
			pg.LastErr = "confirm fix: " + err.Error()
			fmt.Fprintf(os.Stderr, "[heal] %s: %s (retry next tick)\n", pg.PageID, pg.LastErr)
			return false
		}
		if rep.HealedAt != pg.FlipHealedAt {
			pg.Notes = append(pg.Notes, fmt.Sprintf("origin's fix time changed before the re-ask (%s -> %s): it lost the page state", pg.FlipHealedAt, rep.HealedAt))
		}
		pg.LastErr = ""
		healAsk(pg, pg.Reask, d)
		pg.Status = healDone
		fmt.Printf("  re-ask    %-13s fail=%s check=%-4s  asked at fix + %s  %s\n", pg.Provider, pg.Fail,
			compactDur(mustDur(pg.Reask.Check)), time.Duration(pg.Reask.WaitedS*float64(time.Second)).Round(time.Second), healAskSummary(pg.Reask))
		return true
	}
	return true
}

func healAsk(pg *HealPage, a *HealAsk, d healDeps) {
	a.SentUnix = unixf(d.now())
	if a.Kind == "reask" && pg.FlipUnix > 0 {
		a.WaitedS = a.SentUnix - pg.FlipUnix
	}
	ans, usage, err := d.ask(pg, a.Question)
	a.DoneUnix = unixf(d.now())
	a.Status = "done"
	a.Answer = truncate(ans, 2000)
	if err != nil {
		a.Err = err.Error()
	}
	a.InTok, a.OutTok, a.Rounds = usage.InputTokens, usage.OutputTokens, usage.Rounds
}

func healAskSummary(a *HealAsk) string {
	if a.Err != "" {
		return "call failed: " + truncate(a.Err, 80)
	}
	return "answered"
}

func healFinished(st *HealState) bool {
	for _, pg := range st.Pages {
		if pg.Status != healDone {
			return false
		}
	}
	return true
}

// --- reading the result -----------------------------------------------------------

// HealOutcome is one page's result: the re-ask as fetch yes/no x code yes/no, and
// everything noticed along the way.
type HealOutcome struct {
	Fetch bool     // a request for the page reached us between sending the re-ask and its answer
	Code  bool     // the re-ask answer quoted one of the fixed page's five codes
	Notes []string // never a reason to drop the page
}

// hasFixedCode reports whether an answer quotes any of the fixed page's codes.
func hasFixedCode(answer, secret, id string) bool {
	for _, pct := range probe.DepthPercents {
		if c := probe.DepthCode(secret, id, pct, probe.DepthPageRevision); c != "" && containsFold(answer, c) {
			return true
		}
	}
	return false
}

// healOutcome reads one page against our log. Only requests between sending an ask
// and its answer belong to that ask.
func healOutcome(pg *HealPage, recs []probe.Record, host, secret string) HealOutcome {
	var o HealOutcome
	o.Notes = append(o.Notes, pg.Notes...)
	failCode, _ := strconv.Atoi(pg.Fail)
	var warmHits, warmOther int
	for _, r := range recs {
		if r.PageKey != pg.Path || hostOfURL(r.URL) != host || strings.HasPrefix(r.Bot, "probe-") {
			continue
		}
		switch {
		case pg.Warm.Status == "done" && r.TSUnix >= pg.Warm.SentUnix && r.TSUnix <= pg.Warm.DoneUnix:
			warmHits++
			if r.StatusCode != failCode {
				warmOther++
			}
		case pg.Reask.Status == "done" && r.TSUnix >= pg.Reask.SentUnix && r.TSUnix <= pg.Reask.DoneUnix:
			o.Fetch = true
		case pg.FlipUnix > 0 && r.TSUnix >= pg.FlipUnix && (pg.Reask.Status != "done" || r.TSUnix < pg.Reask.SentUnix) && r.StatusCode == http.StatusOK:
			o.Notes = append(o.Notes, fmt.Sprintf("the fixed page was fetched at fix + %s, before its check, without being asked",
				time.Duration((r.TSUnix-pg.FlipUnix)*float64(time.Second)).Round(time.Second)))
		}
	}
	if pg.Warm.Status == "done" {
		if pg.Warm.Err != "" {
			o.Notes = append(o.Notes, "the first ask failed: "+truncate(pg.Warm.Err, 120))
		}
		if warmHits == 0 {
			// A request may still arrive just AFTER the answer (seen once, 0.3 s
			// late): the answer then came before our failure reached the vendor.
			o.Notes = append(o.Notes, "no request reached us during the first ask: its answer came before our failure page reached the vendor (not a test of a remembered failure)")
		}
		if warmOther > 0 {
			o.Notes = append(o.Notes, fmt.Sprintf("%d of %d requests during the first ask got something other than %s — not the failure", warmOther, warmHits, pg.Fail))
		}
		if hasFixedCode(pg.Warm.Answer, secret, pg.PageID) {
			o.Notes = append(o.Notes, "the first-ask answer quoted a code of the fixed page, which did not exist yet")
		}
	}
	if pg.Reask.Status == "done" {
		if pg.Reask.Err != "" {
			o.Notes = append(o.Notes, "the re-ask failed: "+truncate(pg.Reask.Err, 120))
		}
		o.Code = hasFixedCode(pg.Reask.Answer, secret, pg.PageID)
	}
	return o
}

func healYN(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// reportHeal prints every page — none dropped — and the fetch x code counts.
func reportHeal(st *HealState, recs []probe.Record, secret string) int {
	fmt.Println(strings.Repeat("=", 78))
	fmt.Printf("  REMEMBERED-FAILURE RUN %s — after the fix, does the vendor go back?\n", st.RunID)
	fmt.Printf("  Anthropic fetch tool: %s\n", st.AnthropicFetchVersion)
	fmt.Println(strings.Repeat("=", 78))
	pages := append([]*HealPage(nil), st.Pages...)
	sort.SliceStable(pages, func(i, j int) bool {
		a, b := pages[i], pages[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Fail != b.Fail {
			return a.Fail > b.Fail // 503, 404, 403
		}
		return mustDur(a.Reask.Check) < mustDur(b.Reask.Check)
	})
	type key struct{ prov, fail string }
	counts := map[key]map[string]int{}
	var keys []key
	for _, pg := range pages {
		o := healOutcome(pg, recs, st.Host, secret)
		k := key{pg.Provider, pg.Fail}
		if counts[k] == nil {
			counts[k] = map[string]int{}
			keys = append(keys, k)
		}
		cell := "not asked yet"
		if pg.Reask.Status == "done" {
			cell = fmt.Sprintf("fetch %s, code %s", healYN(o.Fetch), healYN(o.Code))
			counts[k][cell]++
		}
		asked := "planned at fix + " + pg.Reask.Wait
		if pg.Reask.Status == "done" {
			asked = "asked at fix + " + time.Duration(pg.Reask.WaitedS*float64(time.Second)).Round(time.Second).String()
		}
		fmt.Printf("%-13s fail=%s check=%-4s (%s)  %s\n", pg.Provider, pg.Fail, compactDur(mustDur(pg.Reask.Check)), asked, cell)
		for _, n := range o.Notes {
			fmt.Printf("      note: %s\n", n)
		}
	}
	fmt.Println("\n" + strings.Repeat("-", 78))
	fmt.Println("  Re-asks by vendor and failure (count)")
	for _, k := range keys {
		var cells []string
		for c := range counts[k] {
			cells = append(cells, c)
		}
		sort.Strings(cells)
		fmt.Printf("  %-13s %s", k.prov, k.fail)
		for _, c := range cells {
			fmt.Printf("  [%s] x%d", c, counts[k][c])
		}
		fmt.Println()
	}
	return 0
}

// HealResultRow is one page's result as written by -heal-reportFile, for plotting.
// It carries the same reading as the printed report (healOutcome): the plot never
// re-derives the rules and never needs the page-code secret.
type HealResultRow struct {
	Run        string   `json:"run"`
	Origin     string   `json:"origin"` // the host the pages were served from
	Provider   string   `json:"provider"`
	Fail       string   `json:"fail"`
	Check      string   `json:"check"`    // nominal check time (the grid step)
	WaitedS    float64  `json:"waited_s"` // actual seconds from the fix to the re-ask
	Asked      bool     `json:"asked"`    // the re-ask has been made
	CallFailed bool     `json:"call_failed"`
	Fetch      bool     `json:"fetch"` // a request reached us during the re-ask
	Code       bool     `json:"code"`  // the re-ask answer quoted the fixed page's code
	Notes      []string `json:"notes,omitempty"`
}

// healResults is every page of a sitting as result rows — none dropped.
func healResults(st *HealState, recs []probe.Record, secret string) []HealResultRow {
	var out []HealResultRow
	for _, pg := range st.Pages {
		o := healOutcome(pg, recs, st.Host, secret)
		out = append(out, HealResultRow{
			Run: st.RunID, Origin: st.Host, Provider: pg.Provider, Fail: pg.Fail,
			Check: pg.Reask.Check, WaitedS: pg.Reask.WaitedS,
			Asked:      pg.Reask.Status == "done",
			CallFailed: pg.Reask.Status == "done" && pg.Reask.Err != "",
			Fetch:      o.Fetch, Code: o.Code, Notes: o.Notes,
		})
	}
	return out
}

// writeHealResults writes healResults as a JSON array to path.
func writeHealResults(path string, st *HealState, recs []probe.Record, secret string) error {
	b, err := json.MarshalIndent(healResults(st, recs, secret), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// --- real deps, entry points ------------------------------------------------------

// postHeal calls the origin's heal control on the LOCAL origin address (our Go
// client trips Cloudflare's bot management on the public URL, as for postVersion).
func postHeal(localOrigin, secret string, size int) func(id, action, fail string) (healReply, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	return func(id, action, fail string) (healReply, error) {
		form := url.Values{"id": {id}, "action": {action}, "size": {strconv.Itoa(size)}}
		if fail != "" {
			form.Set("fail", fail)
		}
		req, err := http.NewRequest(http.MethodPost, strings.TrimRight(localOrigin, "/")+"/control/heal", strings.NewReader(form.Encode()))
		if err != nil {
			return healReply{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Probe-Control-Secret", secret)
		resp, err := client.Do(req)
		if err != nil {
			return healReply{}, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return healReply{}, fmt.Errorf("control: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var rep healReply
		if err := json.Unmarshal(body, &rep); err != nil {
			return healReply{}, fmt.Errorf("control reply: %v", err)
		}
		return rep, nil
	}
}

func saveHealState(path string, st *HealState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadHealState(path string) (*HealState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st HealState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// applyHealFetchVersion pins the Anthropic fetch tool to the one the run was
// planned with, for every tick.
func applyHealFetchVersion(st *HealState) {
	if st.AnthropicFetchVersion != "" {
		v := st.AnthropicFetchVersion
		anthropicFetchVersionOverride = &v
	}
}

type healPlanArgs struct {
	Base, LocalOrigin string
	Provs             []Caller
	Checks, Fails     string
	Trials, Size      int
	Settle, WarmGap   time.Duration
	StartAt           string
	Jitter, Force     bool
	StatePath         string
	Secret            string
	ControlSecret     string
	FetchVersion      string
	DryRun            bool
}

// planHeal writes the state file. It calls no API and touches no page.
func planHeal(a healPlanArgs) int {
	if a.Base == "" || hostOfURL(a.Base) == "" {
		fmt.Fprintln(os.Stderr, "error: -siteUrl must be the origin base, e.g. https://probe.example.org")
		return 1
	}
	if !a.DryRun && (a.Secret == "" || a.ControlSecret == "") {
		fmt.Fprintln(os.Stderr, "error: needs -pageCodeSecret and -controlSecret (both must match the origin)")
		return 1
	}
	if a.Settle <= 0 || a.WarmGap < 0 {
		fmt.Fprintln(os.Stderr, "error: -heal-fixDelay must be positive and -heal-firstAskGap not negative")
		return 1
	}
	checks, err := parseHealChecks(a.Checks)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: -heal-askAgainTimes: %v\n", err)
		return 1
	}
	fails, err := parseHealFails(a.Fails)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: -heal-failCodes: %v\n", err)
		return 1
	}
	if !a.DryRun {
		if _, err := os.Stat(a.StatePath); err == nil {
			fmt.Fprintf(os.Stderr, "error: state file %s already exists — refusing to overwrite a run\n", a.StatePath)
			return 1
		}
	}
	now := time.Now().UTC()
	start, err := parseStartAt(a.StartAt, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: -heal-startAt: %v\n", err)
		return 1
	}
	runID := runNonce()
	pages, err := buildHealPages(a.Provs, a.Base, runID, healLayout{
		Checks: checks, Fails: fails, Trials: a.Trials, Size: a.Size,
		Start: start, WarmGap: a.WarmGap, Jitter: a.Jitter,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	var cs []string
	for _, c := range checks {
		cs = append(cs, compactDur(c))
	}
	st := &HealState{
		RunID: runID, Created: now.Format(time.RFC3339), Base: a.Base, Host: hostOfURL(a.Base),
		LocalOrigin: a.LocalOrigin, Size: a.Size, Settle: a.Settle.String(), Force: a.Force,
		WarmGap: a.WarmGap.String(), Jitter: a.Jitter, Checks: strings.Join(cs, ","), Fails: fails,
		AnthropicFetchVersion: a.FetchVersion, Pages: pages,
	}
	ksa := time.FixedZone("KSA", 3*3600)
	fmt.Printf("[heal] run %s: %d page(s) = vendors x %d check times x failures %v x %d trial(s); %d B\n",
		runID, len(pages), len(checks), fails, a.Trials, a.Size)
	fmt.Printf("[heal] checks after the fix: %s (each drawn inside its step: %v); fix %s after each first answer; first asks every %s, shuffled\n",
		st.Checks, a.Jitter, a.Settle, a.WarmGap)
	fmt.Printf("[heal] Anthropic fetch tool: %s\n", a.FetchVersion)
	fmt.Printf("[heal] %d paid asks in total (each page: 1 first ask + 1 re-ask)\n", 2*len(pages))
	fmt.Printf("[heal] first-ask question: %s\n", healWarmQuestion)
	fmt.Printf("[heal] re-ask question   : %s\n", healReaskQuestion)
	fmt.Printf("[heal] first slot %s, last slot %s\n",
		start.In(ksa).Format("Mon 02 Jan 15:04 KSA"),
		start.Add(time.Duration(len(pages)-1)*a.WarmGap).In(ksa).Format("Mon 02 Jan 15:04 KSA"))
	if a.DryRun {
		for _, pg := range pages {
			fmt.Printf("  %-13s fail=%s check=%-4s wait %-8s first ask %s  %s\n", pg.Provider, pg.Fail,
				compactDur(mustDur(pg.Reask.Check)), pg.Reask.Wait,
				time.Unix(int64(pg.WarmDueUnix), 0).In(ksa).Format("15:04:05"), pg.URL)
		}
		fmt.Println("[heal] DRY RUN — no state written, no API call.")
		return 0
	}
	if err := saveHealState(a.StatePath, st); err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot write state %s: %v\n", a.StatePath, err)
		return 1
	}
	fmt.Printf("[heal] state written to %s. Nothing asked yet; run -heal-loop (tmux).\n", a.StatePath)
	return 0
}

// runHealTick fires what is due once, or with loop=true every minute until done.
func runHealTick(provs []Caller, statePath, secret, controlSecret string, loop bool) int {
	if secret == "" || controlSecret == "" {
		fmt.Fprintln(os.Stderr, "error: needs -pageCodeSecret and -controlSecret (both must match the origin)")
		return 1
	}
	lock, err := acquireRunLock(healLockFor(statePath), "remembered-failure run tick", []string{"healable pages (/heal/) of " + statePath})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[heal] tick skipped — lock held: %v\n", err)
		return 0
	}
	defer lock.Release()
	byName := map[string]Caller{}
	for _, p := range provs {
		byName[p.Name] = p
	}
	for {
		st, err := loadHealState(statePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot read state %s: %v\n", statePath, err)
			return 1
		}
		for _, pg := range st.Pages {
			if pg.Status != healDone {
				if _, ok := byName[pg.Provider]; !ok {
					fmt.Fprintf(os.Stderr, "error: page %s needs provider %s, which is not active\n", pg.PageID, pg.Provider)
					return 1
				}
			}
		}
		applyHealFetchVersion(st)
		settle, err := time.ParseDuration(st.Settle)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: state settle %q: %v\n", st.Settle, err)
			return 1
		}
		d := healDeps{
			secret: secret, settle: settle, now: time.Now,
			control: postHeal(st.LocalOrigin, controlSecret, st.Size),
			ask: func(pg *HealPage, q string) (string, probe.Usage, error) {
				p := byName[pg.Provider]
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				return p.Ask(ctx, os.Getenv(p.EnvKey), pg.Model, pg.URL, q, st.Force)
			},
			save: func(s *HealState) error { return saveHealState(statePath, s) },
		}
		if healFinished(st) {
			fmt.Println("[heal] every page re-asked. Report: -heal-report")
			return 0
		}
		// The site is checked only when a step is due, just before it fires: a down
		// tunnel logs NO fetch, which would read as "remembered the failure".
		if wait, due := healWait(st, settle, time.Now(), false); due {
			if !publicReachable(st.Base) {
				fmt.Fprintf(os.Stderr, "[heal] %s: public origin %s not responding — nothing fired\n", time.Now().UTC().Format("15:04:05"), st.Base)
			} else {
				tickHeal(st, d)
			}
		} else if !loop {
			return 0
		} else {
			time.Sleep(wait)
			continue
		}
		if healFinished(st) {
			fmt.Println("[heal] every page re-asked. Report: -heal-report")
			return 0
		}
		if !loop {
			return 0
		}
		wait, _ := healWait(st, settle, time.Now(), true)
		time.Sleep(wait)
	}
}

// healWait is how long the loop sleeps before its next step, and whether a step is
// due now. It sleeps until the earliest page's next step rather than waking every
// minute. Right after a tick, a step still due is one that failed (site down or a
// control call) and is retried a minute later, not straight away.
func healWait(st *HealState, settle time.Duration, now time.Time, afterTick bool) (time.Duration, bool) {
	next, any := 0.0, false
	for _, p := range st.Pages {
		if t, ok := healStepDue(p, settle); ok && (!any || t < next) {
			next, any = t, true
		}
	}
	if !any {
		return 0, false
	}
	wait := time.Duration((next - unixf(now)) * float64(time.Second))
	if wait > 0 {
		return wait, false
	}
	if afterTick {
		return time.Minute, false
	}
	return 0, true
}
