package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"aisearch-probe/internal/probe"
)

// DOES ONE CUSTOMER'S DOWNLOAD SERVE ANOTHER CUSTOMER'S QUESTION?
//
// Everything else in this study measures one caller at a time. This measures
// whether the copy a provider keeps is private to the caller who caused it, or
// handed to anyone who asks for the same address.
//
// It matters more than the retention figure. If the copy is shared, then a page
// is stale for EVERYONE for the whole window because of one stranger's question,
// and nothing the site owner does can clear it.
//
// The two callers can differ by account (direct key vs OpenRouter — two unrelated
// paying customers, clean only where both routes bring the vendor's OWN fetcher;
// verified 2026-10-11 for OpenAI, Anthropic on claude-sonnet-5, and xAI), by model
// at one vendor (address by Tag), by OpenRouter end-user id, or not at all
// (-sharingApi-run-sameCaller: the same caller twice, the within-account reference).
//
// THE PAGE AND THE PROOF (redo of 2026-10-11)
//
// Each run makes two brand-new 2 KB depth pages (/depthhtmlpage/<id>/2048, the
// page the cache-control and fault runs use): the shared page and the control.
// Every answer is matched against that page's OWN code, recomputed here from the
// page-code secret — never the shared RCP marker, which is the same on every page
// and could be repeated without reading anything. So "quoted the code, and no
// fetch reached us" can only mean the caller held THIS page's text.
//
// THE ADDRESS IS DELIBERATELY SHARED, WHICH IS NORMALLY A BUG.
//
// Every other mode gives each caller its own address precisely so that one
// caller's download cannot satisfy another's question. Here the collision IS the
// experiment, so the address carries no per-caller tag — and the banner below
// exists so no later run inherits this quietly.
//
// WHY THE SECOND CALLER ASKS A DIFFERENT QUESTION
//
// If the second caller asked the same question as the first, a stored ANSWER
// would explain a silent step just as well as a stored PAGE. So every ask uses
// the cache-control run's sentence naming ONE marker by its location: the first
// caller the bottom one, the second caller middle ones nobody asked for before.
// If that code still turns up in an earlier answer about the page, the try is
// marked CAN'T TELL rather than counted.
//
// WHY THERE IS A CONTROL PAGE
//
// Silence from the second caller could mean it reused the copy, or that its
// route was broken at that moment. The control page — which nobody has touched —
// must produce a download, proving it was willing and able to fetch right then.

// stdout is a seam for the tests: the verdict logic is the part that could
// manufacture a false finding, so it has to be assertable.
var stdout io.Writer = os.Stdout

// sharingPageBytes is the depth page size: the cache-control run's 2 KB.
const sharingPageBytes = 2048

// sharingWarmQuestion is the first caller's question: the cache-control run's
// sentence, naming the bottom (95%) marker — the code that run grades on —
// instead of "the study reference code". The page has nothing labelled that, so
// the vague wording made Claude list all five codes (2026-10-11 smoke run),
// including the ones the second caller is later asked for, and the try could not
// be read. Named locations got one code back each time.
var sharingWarmQuestion = sharingAskQuestion(probe.DepthPercents[len(probe.DepthPercents)-1])

// The depths the second caller asks for. Middle markers on purpose: the warm
// answer usually quotes the top code (as the "id") and the bottom one already —
// on the 2026-10-07 cache-control run most answers quoted exactly those two.
const (
	sharingAskDepth  = 50
	sharingLateDepth = 75
)

// sharingAskQuestion is the standard question (standardQuestion, DECISIONS.md
// § D14): one named marker. The page labels each marker "…for the section at
// depth=N%", so the phrase points at exactly one code. Kept as a name because the
// sharing, leak, websharing, correction and heal runners call it; the wording lives
// only in standardQuestion.
func sharingAskQuestion(depth int) string {
	return standardQuestion(depth)
}

// SharingStep is one question in the sequence, with what we observed.
type SharingStep struct {
	N        int     `json:"step"`
	Label    string  `json:"label"`
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	User     string  `json:"end_user,omitempty"`
	URL      string  `json:"url"`
	Question string  `json:"question"`
	SentAt   string  `json:"sent_at"`
	SentUnix float64 `json:"sent_unix"`
	Answer   string  `json:"answer"`
	Error    string  `json:"error,omitempty"`
	// Usage carries the call's token cost to the standard record (json:"-" so the
	// bespoke file's schema is unchanged).
	Usage probe.Usage `json:"-"`
	// FetchStatus is the provider's own report of the fetch (Gemini's
	// urlRetrievalStatus per URL, from probe.Usage), kept on failure too.
	FetchStatus string `json:"fetch_status,omitempty"`
	// Web-app run only: when the reply was pasted, and requests for the page that
	// came AFTER it (they cannot have produced the answer; Fetches holds the ones
	// before it). The API run leaves both empty.
	AnsweredUnix float64  `json:"answered_unix,omitempty"`
	LateFetches  []string `json:"late_fetches,omitempty"`
	// Want is this page's own code at WantDepth; Marker is whether the answer
	// quoted it. The code is unique to the page, so a match proves the model held
	// this page's text.
	Want      string `json:"want"`
	WantDepth int    `json:"want_depth"`
	Marker    bool   `json:"marker"`
	// Leaked: the code this step asked for had already appeared in an earlier
	// answer about the same page, so a saved answer could explain a silence.
	Leaked  bool     `json:"leaked,omitempty"`
	Fetched bool     `json:"fetched"`
	Fetches []string `json:"fetches,omitempty"`
	Attrib  string   `json:"attribution"`
}

// sharingPlanStep is one planned ask.
type sharingPlanStep struct {
	label     string
	caller    Caller
	user      string
	id, url   string
	question  string
	wantDepth int
	want      string
	wait      time.Duration
}

func sharingPageURL(base, id string) string {
	return fmt.Sprintf("%s/depthhtmlpage/%s/%d", strings.TrimRight(base, "/"), id, sharingPageBytes)
}

// buildSharingPlan lays out the four asks on two fresh pages named by the run id.
func buildSharingPlan(base, runID, secret string, first, second Caller, firstUser, secondUser string,
	gap, longGap time.Duration) []sharingPlanStep {
	shared, control := "sh-"+runID, "sh-"+runID+"-ctl"
	deepest := probe.DepthPercents[len(probe.DepthPercents)-1]
	step := func(label string, c Caller, user, id, q string, depth int, wait time.Duration) sharingPlanStep {
		return sharingPlanStep{label: label, caller: c, user: user, id: id, url: sharingPageURL(base, id),
			question: q, wantDepth: depth, want: probe.DepthCode(secret, id, depth, probe.DepthPageRevision), wait: wait}
	}
	return []sharingPlanStep{
		step("1: first caller warms the shared page", first, firstUser, shared, sharingWarmQuestion, deepest, 0),
		step("2: second caller asks the shared page — THE MEASUREMENT", second, secondUser, shared, sharingAskQuestion(sharingAskDepth), sharingAskDepth, gap),
		// Straight after step 2: B's fetcher must be shown alive at the same moment.
		step("3: second caller asks a fresh page — the control", second, secondUser, control, sharingAskQuestion(sharingAskDepth), sharingAskDepth, 0),
		step("4: second caller asks the shared page again, after the long gap", second, secondUser, shared, sharingAskQuestion(sharingLateDepth), sharingLateDepth, longGap),
	}
}

// codeSeenEarlier reports whether code already appeared in an earlier answer
// about the same page.
func codeSeenEarlier(prev []SharingStep, url, code string) bool {
	if code == "" {
		return false
	}
	for _, s := range prev {
		if s.URL == url && containsFold(s.Answer, code) {
			return true
		}
	}
	return false
}

// validateSharingPair: the same caller twice is allowed only as the explicit
// within-account baseline, and the baseline must really be the same caller.
func validateSharingPair(a, b Caller, userA, userB string, baseline bool) error {
	same := a.Tag() == b.Tag() && userA == userB
	switch {
	case baseline && !same:
		return errors.New("-sharingApi-run-sameCaller means the SAME caller twice; drop it to compare two different callers")
	case !baseline && same:
		return errors.New("the two callers are identical. Differ by at least one of: a different account\n" +
			"  (direct key vs or:vendor), a different MODEL at one vendor (address by Tag, e.g.\n" +
			"  or:openai-gpt-5-mini), or different -sharingApi-firstUser/-sharing-second-user —\n" +
			"  or pass -sharingApi-run-sameCaller for the within-account reference")
	}
	return nil
}

// sharingLockPath gives each comparison its own lock, so different comparisons
// run side by side while the same one cannot be started twice. The pages are
// generated per run, so none of them touches probe-site/.
func sharingLockPath(a, b Caller, userA, userB string) string {
	name := a.Tag() + "--" + b.Tag()
	if userA != "" || userB != "" {
		name += "--" + userA + "-" + userB
	}
	r := strings.NewReplacer("/", "_", ":", "_", " ", "_")
	return "logs/.sharing-" + r.Replace(name) + ".lock"
}

// runSharing carries out the four-step sequence. Returns a process exit code.
// withControlGap sets the pause before the control step (step 3). Zero keeps the
// plan as before: the control straight after step 2. Gemini's free tier answered
// 503 "high demand" to the control, asked 20 s after step 2, in both of its tries
// (29 and 31 Oct 2026), so its side check can space the two calls out.
func withControlGap(plan []sharingPlanStep, d time.Duration) []sharingPlanStep {
	if d > 0 && len(plan) > 2 {
		plan[2].wait = d
	}
	return plan
}

func runSharing(base, secret string, first, second Caller,
	gap, longGap, controlGap time.Duration, force bool, outPath, firstUser, secondUser string, dryRun bool) int {

	fmt.Println()
	fmt.Println("\033[1m" + strings.Repeat("!", 74) + "\033[0m")
	fmt.Println("\033[1m  SHARED-ADDRESS MODE — both callers are given the IDENTICAL address.\033[0m")
	fmt.Println("\033[1m  This is deliberate and it is the experiment. It must NEVER be used for a\033[0m")
	fmt.Println("\033[1m  retention measurement: one caller's download would satisfy the other's\033[0m")
	fmt.Println("\033[1m  question and the result would read as the provider keeping a copy.\033[0m")
	fmt.Println("\033[1m" + strings.Repeat("!", 74) + "\033[0m")
	fmt.Println()

	if firstUser != "" || secondUser != "" {
		fmt.Printf("  PER-USER MODE: first caller user=%q, second caller user=%q (sent via OpenRouter's user field)\n\n", firstUser, secondUser)
	}
	if base == "" {
		fmt.Fprintln(os.Stderr, "error: -siteUrl (the origin, e.g. https://probe.attentionhub.org) is required")
		return 1
	}
	if secret == "" && !dryRun {
		fmt.Fprintln(os.Stderr, "error: -pageCodeSecret (or $PROBE_PAGE_CODE_SECRET) is required:")
		fmt.Fprintln(os.Stderr, "  every answer is matched against the page's own code, recomputed from it")
		return 1
	}

	// A run id names the pages and groups this sequence's standard records into
	// one episode.
	runID := runNonce()
	plan := withControlGap(buildSharingPlan(base, runID, secret, first, second, firstUser, secondUser, gap, longGap), controlGap)

	if dryRun {
		fmt.Println("[sharing] DRY RUN — no API calls, no waits. Plan:")
		for _, s := range plan {
			fmt.Printf("  %s\n    caller %s user=%q  wait %s\n    %s\n    asks: %s\n",
				s.label, s.caller.Tag(), s.user, s.wait, s.url, s.question)
		}
		return 0
	}

	// Both pages must be untouched, or "no download" in step 2 could simply mean
	// somebody already had a copy from before the experiment started.
	pageA, pageB := plan[0].url, plan[2].url
	if probeLogPath != "" {
		recs, _, err := probe.LoadRecords(probeLogPath, "", time.Time{})
		if err == nil {
			for _, pg := range []string{pageA, pageB} {
				if n, who := priorFetches(recs, pg); n > 0 {
					fmt.Fprintf(os.Stderr, "error: %s has already been fetched %d time(s) (%s).\n",
						pathOf(pg), n, who)
					fmt.Fprintf(os.Stderr, "  Both pages must be untouched on this address. Use a fresh address.\n")
					return 1
				}
			}
			fmt.Printf("  freshness: neither page has been fetched on this address — good\n\n")
		} else {
			fmt.Fprintf(os.Stderr, "warning: could not read %s, so freshness is unverified: %v\n", probeLogPath, err)
		}
	} else {
		// Refuse, not warn. Without the server log there is no way to see whether
		// a download happened, which is the entire measurement — so the run could
		// only ever produce answers and no result. It warned instead, once, and
		// spent two API calls proving that.
		fmt.Fprintln(os.Stderr, "error: -serverLog is required for the sharing experiment.")
		fmt.Fprintln(os.Stderr, "  Without the server log nothing can be observed: the measurement IS")
		fmt.Fprintln(os.Stderr, "  whether a download arrived, so the run would spend money for nothing.")
		return 1
	}

	// Both pages must actually be served right now, carrying the code we will
	// look for — which also proves the origin holds the same secret as we do.
	for _, s := range []sharingPlanStep{plan[0], plan[2]} {
		if l := checkPageLive(context.Background(), s.url, s.want); !l.OK {
			fmt.Fprintf(os.Stderr, "error: %s is not usable: %s\n", s.url, l)
			fmt.Fprintln(os.Stderr, "  Both pages must be live and carry their own code before the experiment starts.")
			return 1
		}
	}
	fmt.Printf("  both pages are live and carry their own codes\n\n")

	log, err := newTrialLog(outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer log.Close()

	steps := make([]SharingStep, 0, len(plan))
	for i, s := range plan {
		if s.wait > 0 {
			fmt.Printf("  waiting %s…\n", s.wait)
			time.Sleep(s.wait)
		}
		openRouterUser = s.user // per-step end-user id (empty unless the per-user test)
		st := askOnce(i+1, s.label, s.caller, s.url, s.question, s.want, force)
		openRouterUser = "" // do not leak into any later non-plan call
		st.Model, st.User, st.WantDepth = s.caller.Model, s.user, s.wantDepth
		st.Leaked = i > 0 && codeSeenEarlier(steps, s.url, s.want)
		steps = append(steps, st)
		if err := log.WriteAny(st); err != nil {
			fmt.Fprintf(os.Stderr, "  log write failed: %v\n", err)
		}
		sharingEmit(runID, st, s.caller, s.user, force)
		printStep(st)
	}

	fmt.Printf("\n[sharing] steps written to %s\n", outPath)
	return reportSharing(steps)
}

// priorFetches reports how many requests for this page already exist, so a
// polluted page is caught before any money is spent.
func priorFetches(recs []probe.Record, pageURL string) (int, string) {
	host := hostOfURL(pageURL)
	path := pathOf(pageURL)
	n := 0
	seen := map[string]bool{}
	for _, r := range recs {
		if r.PageKey != path || hostOfURL(r.URL) != host {
			continue
		}
		// Our own liveness and self-check requests are the instrument, not a
		// caller, and they do not put a copy into anybody's store.
		if strings.HasPrefix(r.Bot, "probe-") {
			continue
		}
		n++
		seen[r.Bot] = true
	}
	var who []string
	for b := range seen {
		who = append(who, b)
	}
	return n, strings.Join(who, ", ")
}

func hostOfURL(rawURL string) string {
	i := strings.Index(rawURL, "://")
	if i < 0 {
		return ""
	}
	rest := rawURL[i+3:]
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		return rest[:j]
	}
	return rest
}

// sharingEmit writes one sharing step's standard record. The four-step sequence
// is one episode; step is "step1".."step4". Token is empty by design; the fetched
// snapshot and attribution are kept in extra, the detailed hits in the bespoke
// file. The verdict stays derived (reportSharing / a later reader).
func sharingEmit(runID string, st SharingStep, p Caller, user string, force bool) {
	sharingEmitAs("api", runID, runID+"-sharing", st, p, user, force)
}

// sharingEmitAs is sharingEmit for any surface ("api" or "web-app") and episode.
func sharingEmitAs(surface, runID, episode string, st SharingStep, p Caller, user string, force bool) {
	m := resultMeta{
		RunID: runID, Experiment: "sharing",
		EpisodeID: episode, Step: fmt.Sprintf("step%d", st.N),
		Surface: surface, Vendor: p.Name, Model: p.Model, ToolForced: force, Marker: st.Want,
	}
	rec := resultFromAsk(m, fmt.Sprintf("%s-step%d", episode, st.N), askOutcome{
		URL: st.URL, Token: "", Question: st.Question, Answer: st.Answer,
		ErrStr: st.Error, MarkerPresent: st.Marker, SentUnix: st.SentUnix,
		InputTokens: st.Usage.InputTokens, OutputTokens: st.Usage.OutputTokens,
		FetchesSnapshot: len(st.Fetches),
	}, map[string]any{
		"label":            st.Label,
		"end_user":         user,
		"want_depth":       st.WantDepth,
		"leaked":           st.Leaked,
		"fetched_snapshot": st.Fetched,
		"attribution":      st.Attrib,
		"fetch_status":     st.FetchStatus,
	})
	if err := emitResults(m.Experiment, runID, []probe.ResultRecord{rec}); err != nil {
		fmt.Fprintf(os.Stderr, "  result record write failed (sharing step %d): %v\n", st.N, err)
	}
}

// askOnce sends one question at the BARE address — no per-caller tag — and then
// looks in the probe log for a request it caused.
// sharingStepFromAsk records one ask: the answer and whether it carries the code,
// the cost, and the provider's own report of the fetch (Gemini's
// urlRetrievalStatus) — kept on a failed call too, where it matters most.
func sharingStepFromAsk(n int, label string, p Caller, url, question, want string, sent time.Time,
	answer string, usage probe.Usage, err error) SharingStep {
	st := SharingStep{
		N: n, Label: label, Provider: p.Name, URL: url, Question: question,
		SentAt: sent.Format(time.RFC3339Nano), SentUnix: float64(sent.UnixNano()) / 1e9,
		Answer: truncate(answer, 4000), Want: want, Marker: want != "" && containsFold(answer, want),
		Usage: usage, FetchStatus: usage.FetchStatus,
	}
	if err != nil {
		st.Error = err.Error()
	}
	return st
}

func askOnce(n int, label string, p Caller, url, question, want string, force bool) SharingStep {
	return askOnceTimeout(n, label, p, url, question, want, force, 5*time.Minute)
}

// askOnceTimeout is askOnce with the call's time limit given. The leak runner
// needs longer than 5 minutes for Anthropic's direct key (see leakAskTimeout).
func askOnceTimeout(n int, label string, p Caller, url, question, want string, force bool, timeout time.Duration) SharingStep {
	fmt.Printf("\n\033[1m--- step %s\033[0m\n", label)
	fmt.Printf("  caller  : %s\n", p.Tag())
	fmt.Printf("  address : %s\n", url)
	fmt.Printf("  question: %s\n", question)

	sent := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	answer, usage, err := p.Ask(ctx, os.Getenv(p.EnvKey), p.Model, url, question, force)

	st := sharingStepFromAsk(n, label, p, url, question, want, sent, answer, usage, err)
	if err != nil {
		return st
	}

	// Give a slow fetcher time to arrive before deciding nothing did.
	time.Sleep(15 * time.Second)
	observeFetches(&st, p.Name, sharingFetchWindow)
	return st
}

// samePage reports whether a request path is for the page at pageURL in ANY of
// its variants: every address under the page's id (/depthhtmlpage/<id>/...) carries
// the same codes, whatever the size, so fetching /4096 instead of /2048 is still
// a download of this page. Matching the exact path let such a fetch read as "no
// download" (Mistral's web app tried /4096 … /1048576 on 2026-10-11). The control
// page has a different id, so it never matches.
func samePage(reqPath, pageURL string) bool {
	p := pathOf(pageURL)
	i := strings.LastIndexByte(p, '/')
	if i <= 0 {
		return reqPath == p
	}
	dir := p[:i] // /depthhtmlpage/<id>
	return reqPath == dir || strings.HasPrefix(reqPath, dir+"/")
}

// sharingFetchWindow is how long after an ask a request still counts as caused by it.
const sharingFetchWindow = 150.0

// observeFetches fills in which requests for st's page reached the server in the
// window after st was sent. No token: on a shared address every request for the
// page in the window is a candidate, which is exactly what we want to count.
// Shared by the API run and the web-app run so both judge a download alike.
func observeFetches(st *SharingStep, provider string, window float64) {
	if probeLogPath == "" {
		st.Attrib = "not checked (no -serverLog)"
		return
	}
	recs, _, lerr := probe.LoadRecords(probeLogPath, "", time.Time{})
	if lerr != nil {
		st.Attrib = "log unreadable: " + lerr.Error()
		return
	}
	at := probe.AttributeFetch(provider, recs, pathOf(st.URL), st.SentUnix, window)
	st.Attrib = strings.Join(at.Seen, ", ")
	st.Fetched, st.Fetches = false, nil
	for _, r := range recs {
		if samePage(r.PageKey, st.URL) && hostOfURL(r.URL) == hostOfURL(st.URL) &&
			r.TSUnix >= st.SentUnix && r.TSUnix <= st.SentUnix+window &&
			!strings.HasPrefix(r.Bot, "probe-") {
			st.Fetched = true
			st.Fetches = append(st.Fetches,
				fmt.Sprintf("%s %s %d %dB", r.TS[11:19], r.Bot, r.StatusCode, r.BytesSent))
		}
	}
}

func printStep(st SharingStep) {
	if st.Error != "" {
		fmt.Printf("  \033[31mFAILED\033[0m: %s\n", truncate(st.Error, 160))
		if st.FetchStatus != "" {
			fmt.Printf("  provider's own fetch report: %s\n", st.FetchStatus)
		}
		return
	}
	if st.Fetched {
		fmt.Printf("  downloaded: \033[1mYES\033[0m — %s\n", strings.Join(st.Fetches, "; "))
	} else {
		fmt.Printf("  downloaded: \033[1mNOTHING REACHED US\033[0m\n")
	}
	fmt.Printf("  this page's depth=%d%% code in the answer: %v", st.WantDepth, st.Marker)
	if st.Leaked {
		fmt.Print("  (but it was already in an earlier answer about this page)")
	}
	fmt.Println()
	fmt.Printf("  answer  : %s\n", truncate(strings.ReplaceAll(st.Answer, "\n", " "), 240))
	if st.FetchStatus != "" {
		fmt.Printf("  provider's own fetch report: %s\n", st.FetchStatus)
	}
}

// reportSharing prints what was observed, then the reading. Step 4 (the late
// re-ask) is optional: the web-app run has only steps 1-3.
func reportSharing(steps []SharingStep) int {
	if len(steps) < 3 {
		fmt.Fprintln(stdout, "\nincomplete run — no verdict")
		return 1
	}
	s1, s2, s3 := steps[0], steps[1], steps[2]
	hasLate := len(steps) >= 4
	var s4 SharingStep
	if hasLate {
		s4 = steps[3]
	}

	fmt.Fprintln(stdout, "\n"+strings.Repeat("=", 74))
	fmt.Fprintln(stdout, "  WHAT WAS OBSERVED")
	fmt.Fprintln(stdout, strings.Repeat("=", 74))
	fmt.Fprintf(stdout, "  %-52s %s\n", "1. first caller asks the shared page", yesNo(s1.Fetched, "downloaded", "nothing"))
	fmt.Fprintf(stdout, "  %-52s %s\n", "2. second caller asks the shared page (the measurement)", yesNo(s2.Fetched, "downloaded", "nothing"))
	fmt.Fprintf(stdout, "  %-52s %s\n", "3. second caller asks a fresh page (the control)", yesNo(s3.Fetched, "downloaded", "nothing"))
	if hasLate {
		fmt.Fprintf(stdout, "  %-52s %s\n", "4. second caller asks the shared page after the long gap", yesNo(s4.Fetched, "downloaded", "nothing"))
	}
	// Steps 2 and 3 are the same caller, question and page size; only whether a
	// copy was waiting differs. So their billed input tokens compare directly.
	// (The web-app run has no token counts, so it prints nothing here.)
	if s2.Usage.InputTokens > 0 || s3.Usage.InputTokens > 0 {
		fmt.Fprintf(stdout, "  input tokens billed — step 2 (shared page): %d · step 3 (fresh page): %d\n",
			s2.Usage.InputTokens, s3.Usage.InputTokens)
	}

	fmt.Fprintln(stdout, "\n"+strings.Repeat("=", 74))
	fmt.Fprintln(stdout, "  THE READING")
	fmt.Fprintln(stdout, strings.Repeat("=", 74))

	switch {
	case s1.Error != "" || s2.Error != "" || s3.Error != "":
		fmt.Fprintln(stdout, "  VOID — a call failed. Nothing can be concluded.")
		return 1
	case !s1.Fetched:
		fmt.Fprintln(stdout, "  VOID — the first caller never downloaded the page, so there was")
		fmt.Fprintln(stdout, "  no copy for the second one to share. Check the address is reachable")
		fmt.Fprintln(stdout, "  and that the tool call is being forced.")
		return 1
	case !s3.Fetched:
		fmt.Fprintln(stdout, "  VOID — the control failed. The second caller did not download a page")
		fmt.Fprintln(stdout, "  nobody had touched, so its silence in step 2 says nothing about sharing;")
		fmt.Fprintln(stdout, "  it may simply not have been fetching at all.")
		return 1
	case s2.Fetched:
		fmt.Fprintln(stdout, "  NOT SHARED — the second caller downloaded the page for itself.")
		fmt.Fprintln(stdout, "  Each caller keeps its own copy.")
	case !s2.Marker:
		fmt.Fprintln(stdout, "  VOID — the second caller downloaded nothing AND its answer lacks this")
		fmt.Fprintln(stdout, "  page's code. It never had the content, which is a different finding")
		fmt.Fprintln(stdout, "  and not evidence of sharing.")
		return 1
	case s2.Leaked:
		fmt.Fprintln(stdout, "  CAN'T TELL — the second caller quoted the code without a download, but")
		fmt.Fprintln(stdout, "  that code was already in the first caller's answer, so a saved ANSWER")
		fmt.Fprintln(stdout, "  explains it as well as a saved page. This try does not count.")
		return 1
	default:
		fmt.Fprintln(stdout, "  SHARED — the second caller quoted this page's own code without ever")
		fmt.Fprintln(stdout, "  downloading it, for a question nobody had asked, and the code is unique")
		fmt.Fprintln(stdout, "  to this page: the copy can only have come from the first caller's download.")
		if !hasLate {
			break
		}
		fmt.Fprintln(stdout)
		switch {
		case s4.Leaked:
			fmt.Fprintln(stdout, "  The long-gap step is unreadable: its code was already in an earlier answer.")
		case !s4.Fetched && s4.Marker:
			fmt.Fprintln(stdout, "  AND IT IS A REAL STORE, not a warm machine: still shared after the")
			fmt.Fprintln(stdout, "  long gap, which is too long for the second request to have landed on")
			fmt.Fprintln(stdout, "  the same process by chance.")
		case s4.Fetched:
			fmt.Fprintln(stdout, "  BUT ONLY BRIEFLY: by the long gap it downloaded for itself. Consistent")
			fmt.Fprintln(stdout, "  with the two requests happening to hit one warm machine rather than a")
			fmt.Fprintln(stdout, "  shared store — or simply with the copy having expired. Compare the gap")
			fmt.Fprintln(stdout, "  against the retention window before choosing between those.")
		default:
			fmt.Fprintln(stdout, "  The long-gap step is unreadable (no download and no page code), so")
			fmt.Fprintln(stdout, "  whether this is a store or one warm machine is still open.")
		}
	}
	return 0
}

// yesNo prints bold, not coloured: yellow and green are unreadable on a white
// terminal (user, 2026-10-11).
func yesNo(b bool, y, n string) string {
	if b {
		return "\033[1m" + y + "\033[0m"
	}
	return "\033[1m" + n + "\033[0m"
}
