package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"aisearch-probe/internal/probe"
)

// ttlConcurrency bounds how many asks are in flight at once. Warming and each poll
// round fan out to this many, so a 30-page round finishes well inside the cadence
// and the warm order does not smear one arm's pages minutes apart from another's.
const ttlConcurrency = 6

// runConcurrent runs fn(0..n-1) with at most `workers` in flight. Each call writes
// only its own page/poll struct, so there is no shared state to race.
func runConcurrent(n, workers int, fn func(i int)) {
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// THE TTL-MAP runner (client side).
//
// The question (TODO.md § "Does cache TTL differ between MODELS of the same
// vendor"): does one vendor hand two of its models the SAME copy lifetime? Same
// TTL points to one URL-keyed cache behind all the vendor's models; different TTL
// points to per-model caches. It also corroborates the cross-model SHARING finding
// (Results 12/13): those models already share the stored copy, so if they also
// share the TTL it is provably one cache.
//
// METHOD (the shape of Result 4 and the robots-ttl poller, at minute scale for
// content pages). Warm N fresh /page/<id> pages at once, then ask about all of
// them on a fixed cadence, and for each page record the FIRST gap at which a fetch
// reaches our origin (read from probe.jsonl). That first re-fetch is that copy's
// lifetime. Polling does not extend the copy (Result 7, OpenAI: the clock runs
// from the download, not from use), so one page yields one clean TTL and the N
// pages poll in parallel — the whole map is one session.
//
// DISCIPLINE (the confound that bit the object-size sweep, where a header effect
// was assumed and never tested): only the MODEL varies. Page size, the (normal)
// header and the /page/ route are held fixed, so a TTL difference can only be the
// model. Run it once per model (Stage 1 gpt-5-mini, Stage 2 gpt-5.5) by passing a
// different -openaiModel; the runner reads the active provider set as given.
//
// GROUND TRUTH is the FETCH in our log, never the answer text. The page prints a
// site-wide reference code, so marker-present only confirms the model saw A page,
// not THE page; the unique page id in the URL is what countHits joins on. This is
// the same rule retention and negcache follow.
//
// This runner is DRIVER-ONLY: /page/<id> already exists (proxy/pages.go), so there
// is no origin change to deploy and the content freeze cannot block it. It emits
// only the standard result record (internal/probe.ResultRecord, experiment
// "ttlmap"); the TTL per page is DERIVED from the fetch join, printed here as a
// live convenience and re-derivable from logs/results.

// ttlmapQuestion asks for facts carried in the resource, so an answer with no fetch
// is a genuine cache hit, not a guessed reply. It is FORMAT-NEUTRAL on purpose: the
// object-type run asks it of JSON, PDF and DOCX as well as HTML, and the word "page"
// biases those reads (a JSON document is not a "page"). It is DECOUPLED from
// retentionQuestion — that run depends on its exact wording, so rewording here must
// not touch it.
const ttlmapQuestion = "Fetch this document and tell me its id and the study reference code in it. Quote them exactly."

// Cache-header test, day 2 on (2026-10-14): the question NAMES one of the page's
// five depth codes, by the words the page prints next to it. Day 1 asked for "the
// study reference code", which no line on the depth page is called, so a model that
// quoted a different code of the same page was scored as not having read it.
// The depth cycles with each ask — first ask 0%, then 25, 50, 75, 95, 0, 25 — so no
// ask repeats an earlier code until all five have been asked for.

// rotatedDepth is the depth asked for on ask number ask (0 = the first ask).
func rotatedDepth(ask int) int {
	return probe.DepthPercents[ask%len(probe.DepthPercents)]
}

// rotatedQuestion asks for exactly one named code.
// Since 2026-10-25 it is the standard question (DECISIONS.md § D14); 14–21 Oct runs
// asked "quote exactly the code that is the reference marker for the section at
// depth=N%" — the same meaning, recorded in their result records.
func rotatedQuestion(pct int) string {
	return standardQuestion(pct)
}

// askFor returns the question and the exact string an answer must carry for ask
// number ask on this page. Pages that do not rotate keep ttlmapQuestion and their
// one marker, unchanged.
func (pg *ttlPage) askFor(ask int) (question, marker string) {
	switch {
	case pg.depthCodes != nil:
		pct := rotatedDepth(ask)
		return rotatedQuestion(pct), pg.depthCodes[pct]
	case pg.standardDepth != nil:
		// A depth page checked against one code: ask for exactly that code.
		return standardQuestion(*pg.standardDepth), pg.Marker
	}
	// Legacy pages without depth codes (/page/, the old /depthpage/ size sweep):
	// DECISIONS.md § D14 keeps their original question.
	return ttlmapQuestion, pg.Marker
}

// ttlPoll is one scheduled ask about one page at one gap. Fetches is the live
// snapshot read shortly after the ask; the authoritative count is the probe.jsonl
// join on the page id, which this snapshot mirrors.
type ttlPoll struct {
	NominalGap time.Duration // the schedule label
	GapSec     float64       // the ACTUAL gap since warm — used for the TTL, so round drift never corrupts it
	FiredUnix  float64
	Answer     string
	Marker     bool
	Err        string
	Fetches    int
	Usage      probe.Usage
	Asked      string // the question this ask sent
	Wanted     string // the exact code the answer had to carry
}

// ttlPage is one fresh canary: warmed once, then polled until it first re-fetches
// (then retired, because the re-fetch resets its clock and later polls would time
// a new copy) or the window ends.
type ttlPage struct {
	Arm      string
	Provider string
	Model    string
	Rep      int
	PageID   string
	URL      string
	Marker   string // the exact string the read looks for: the global marker, or this page's per-page code

	// Cache-header test only (empty Setting otherwise): the arm's declared setting,
	// and the log record of the warm fetch — the headers we ACTUALLY sent, which is
	// what gradeCacheReuse grades against.
	Setting    string
	Validators probe.ValidatorMode
	Served     probe.Record
	ServedOK   bool

	// depthCodes, when set, makes every ask name one code (askFor); nil keeps the
	// one question and one Marker every other run uses.
	depthCodes map[int]string
	// standardDepth, when set (and depthCodes is nil), makes every ask the standard
	// question for that depth, checked against Marker (that depth's code).
	standardDepth *int
	WarmAsked     string // the question the first ask sent
	WarmWanted    string // the exact code it had to carry

	WarmUnix    float64
	WarmAnswer  string
	WarmMarker  bool
	WarmErr     string
	WarmFetches int
	WarmUsage   probe.Usage

	Polls         []ttlPoll
	Refetched     bool
	FirstRefetchS float64 // actual gap (seconds) of the first re-fetch; valid iff Refetched
	Retired       bool
	pendingPoll   *ttlPoll // stash between firing a round and reading the log once
}

// ttlPollGaps is the pure gap ladder: firstPoll, firstPoll+cadence, … up to and
// including window. Unit-tested, so the schedule is correct before any network.
func ttlPollGaps(firstPoll, cadence, window time.Duration) []time.Duration {
	var gaps []time.Duration
	for g := firstPoll; g <= window; g += cadence {
		gaps = append(gaps, g)
	}
	return gaps
}

// ttlArm is one route under test: a label and how to turn a page id into the URL
// to poll. The header-TTL sweep runs several arms in ONE session so they are
// directly comparable (same model, same day); the default run is one arm on /page/.
//
// Code is how the arm's read is judged. Most arms leave it nil and the read keys on
// the global site-wide marker (-marker). The object-type arms set it: each page
// carries a per-page UNFORGEABLE code, so the read proves the model parsed THAT
// document's body, not replayed a shared marker it could have learned on another arm
// in the same session. buildTTLPages calls it with the secret and the page id.
type ttlArm struct {
	Label string
	URL   func(base, id string) string
	Code  func(secret, id string) string
	// Setting and Validators are set only on cache-header test arms: the
	// probe.CacheSettings word and the version info the page is served with. An
	// empty Setting means the arm is not graded against the caching rules.
	Setting    string
	Validators probe.ValidatorMode
	// RotateDepth: each ask names one of the page's depth codes in turn
	// (rotatedQuestion) instead of asking ttlmapQuestion. Cache-header test only.
	RotateDepth bool
	// AskDepth, when set, makes every ask the standard question for that one depth
	// (its code is the arm's Code). Depth pages that check one code: the format runs.
	AskDepth *int
}

// ttlmapKeepAsking (-cacheHeaders-askAfterRefetch) keeps re-asking a page after the vendor
// fetches it again, instead of retiring it. Off by default: a fetch-again restarts
// the copy's clock, so later polls would time a new copy. On for the Anthropic
// no-store follow-up (2026-10-18), whose question is whether a no-store page is
// fetched fresh on EVERY re-ask or only once. Same package-global pattern as the
// other run-wide switches.
var ttlmapKeepAsking bool

// retireAfter reports whether a page leaves the poll set after this round.
func retireAfter(fetchedAgain bool) bool {
	return fetchedAgain && !ttlmapKeepAsking
}

// selectArms keeps only the arms named in a comma list (-cacheHeaders-settings), in the
// list's own order. Empty keeps all. An unknown label is an error, so a typo
// cannot silently shrink a paid run.
func selectArms(arms []ttlArm, labels string) ([]ttlArm, error) {
	if strings.TrimSpace(labels) == "" {
		return arms, nil
	}
	want := map[string]bool{}
	for _, l := range strings.Split(labels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			want[l] = true
		}
	}
	var out []ttlArm
	for _, a := range arms {
		if want[a.Label] {
			out = append(out, a)
			delete(want, a.Label)
		}
	}
	if len(want) > 0 {
		var unknown []string
		for l := range want {
			unknown = append(unknown, l)
		}
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown arm label(s): %s", strings.Join(unknown, ", "))
	}
	return out, nil
}

// ccDepthArms is the CACHE-HEADER TEST: one arm per (setting, version info) on
// /ccdepthpage/, the shared depth HTML at a fixed size, so only the caching
// headers vary. The settings come from probe.CacheSettings (RFC 9111 + the most
// common real-world combination). Every "don't reuse without asking" setting runs
// with and without version info (for no-cache, max-age=0 and combo, "check first"
// with nothing to check against can only be a full re-download; for no-store,
// private and expired the spec expects no difference, so only a difference is a
// finding); the timed settings run with the date; with no header the last-changed
// date sets the spec's suggested limit, so none runs with old, recent and no date.
// 19 arms. Each arm is graded by gradeCacheReuse against
// probe.SharedReuseLimit for the headers the log shows we actually sent.
func ccDepthArms(size int) []ttlArm {
	deepest := probe.DepthPercents[len(probe.DepthPercents)-1]
	code := func(secret, id string) string {
		return probe.DepthCode(secret, id, deepest, probe.DepthPageRevision)
	}
	arm := func(setting string, v probe.ValidatorMode) ttlArm {
		return ttlArm{
			Label: "cc-" + setting + "-" + string(v),
			URL: func(base, id string) string {
				return strings.TrimRight(base, "/") + "/ccdepthpage/" + setting + "/" + string(v) + "/" + id + "/" + strconv.Itoa(size)
			},
			Code:        code,
			RotateDepth: true,
			Setting:     setting,
			Validators:  v,
		}
	}
	tag, recent, notag := probe.ValidatorsOld, probe.ValidatorsRecent, probe.ValidatorsNone
	return []ttlArm{
		arm("nostore", tag), arm("nostore", notag),
		arm("private", tag), arm("private", notag),
		arm("nocache", tag), arm("nocache", notag),
		arm("maxage0", tag), arm("maxage0", notag),
		arm("combo", tag), arm("combo", notag),
		arm("expired", tag), arm("expired", notag),
		arm("max60", tag), arm("max60reval", tag), arm("smax60", tag), arm("max3600", tag),
		arm("none", tag), arm("none", recent), arm("none", notag),
	}
}

// parseDurations turns "15m,30m,1h,2h,4h,8h" into a back-off poll schedule. Each gap
// must parse, be at least 150s (the fetch-attribution floor the fixed cadence also
// enforces), and the list must be strictly ascending. A bad, too-short, out-of-order
// or empty list is an error, not a silent reorder — a mis-ordered gap would break the
// TTL join. The point of the back-off is to poll densely early (catch a short TTL like
// OpenAI's ~30 min) and sparsely late (just checkpoint a long-lived cache), so the
// long-held pages are not re-asked every round for nothing.
func parseDurations(s string) ([]time.Duration, error) {
	var out []time.Duration
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		d, err := time.ParseDuration(part)
		if err != nil {
			return nil, fmt.Errorf("bad duration %q: %w", part, err)
		}
		if d < 150*time.Second {
			return nil, fmt.Errorf("gap %s is below the 150s fetch-attribution floor", d)
		}
		if len(out) > 0 && d <= out[len(out)-1] {
			return nil, fmt.Errorf("gaps must be strictly ascending: %s is not after %s", d, out[len(out)-1])
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no gaps given")
	}
	return out, nil
}

// buildTTLPages is the pure plan: one fresh page per arm per provider per
// replicate, each id unique per (arm, model, run, replicate) so nothing two
// canaries share can let one satisfy the other and the fetch join keys on it. No
// network, no state.
func buildTTLPages(arms []ttlArm, provs []Caller, base, runID string, replicates int, secret, globalMarker string) []*ttlPage {
	var out []*ttlPage
	for _, arm := range arms {
		for _, p := range provs {
			for rep := 1; rep <= replicates; rep++ {
				id := fmt.Sprintf("%s-%s-%s-ttl-r%d", arm.Label, p.Tag(), runID, rep)
				// The read keys on the per-page code when the arm defines one (the
				// object-type arms), otherwise on the global marker.
				mk := globalMarker
				if arm.Code != nil {
					mk = arm.Code(secret, id)
				}
				pg := &ttlPage{
					Arm: arm.Label, Provider: p.Name, Model: p.Model, Rep: rep,
					PageID: id, URL: arm.URL(base, id), Marker: mk,
					Setting: arm.Setting, Validators: arm.Validators,
					standardDepth: arm.AskDepth,
				}
				if arm.RotateDepth {
					pg.depthCodes = map[int]string{}
					for _, pct := range probe.DepthPercents {
						pg.depthCodes[pct] = probe.DepthCode(secret, id, pct, probe.DepthPageRevision)
					}
				}
				out = append(out, pg)
			}
		}
	}
	return out
}

// runTTLMap is the live path: warm every page, then poll the still-live pages at
// each gap, reading the probe log ONCE per round (fetches land during the ask, so
// a short settle then one read attributes each page's fetch to its own ask).
func runTTLMap(base string, provs []Caller, arms []ttlArm, replicates int, firstPoll, cadence, window time.Duration, pollGaps []time.Duration, secret, marker string, force, dry bool) int {
	if base == "" {
		fmt.Fprintln(os.Stderr, "error: -siteUrl is required (origin base)")
		return 1
	}
	if probeLogPath == "" {
		fmt.Fprintln(os.Stderr, "error: -serverLog is required; the TTL is read from what reaches our server")
		return 1
	}
	// Any arm that judges the read by a per-page code needs the secret to produce
	// (and later recompute) it. Without it the code would be empty and every read
	// would score false — a silent wrong result, so fail loudly instead.
	if !dry {
		for _, a := range arms {
			if a.Code != nil && secret == "" {
				fmt.Fprintln(os.Stderr, "error: -pageCodeSecret (PROBE_PAGE_CODE_SECRET) is required for this run — the format pages carry per-page HMAC codes the runner must recompute")
				return 1
			}
		}
	}
	host := hostOfURL(base)
	if host == "" {
		fmt.Fprintf(os.Stderr, "error: -siteUrl %q has no host\n", base)
		return 1
	}
	runID := runNonce()
	pages := buildTTLPages(arms, provs, base, runID, replicates, secret, marker)
	// An explicit back-off schedule (parseDurations) overrides the fixed first-poll /
	// cadence / window ladder; empty falls back to the even cadence.
	gaps := pollGaps
	if len(gaps) == 0 {
		gaps = ttlPollGaps(firstPoll, cadence, window)
	}

	provNames := make([]string, 0, len(provs))
	for _, p := range provs {
		provNames = append(provNames, p.Tag())
	}
	armLabels := make([]string, 0, len(arms))
	for _, a := range arms {
		armLabels = append(armLabels, a.Label)
	}
	fmt.Printf("[ttlmap] run %s: providers [%s], arms [%s], %d pages (%d per arm/provider), cadence %s, first poll %s, window %s (%d poll rounds)\n",
		runID, strings.Join(provNames, ", "), strings.Join(armLabels, ", "), len(pages), replicates, cadence, firstPoll, window, len(gaps))

	if dry {
		fmt.Println("[ttlmap] DRY RUN — nothing warmed, nothing asked. Poll gaps:")
		labels := make([]string, len(gaps))
		for i, g := range gaps {
			labels[i] = g.String()
		}
		fmt.Println("  " + strings.Join(labels, ", "))
		for _, pg := range pages {
			fmt.Printf("  %s -> %s\n", pg.PageID, pg.URL)
		}
		return 0
	}

	byName := map[string]Caller{}
	for _, p := range provs {
		byName[p.Name] = p
	}

	fmt.Printf("[ttlmap] about to make up to ~%d API calls (1 warm + up to %d polls per page) — Ctrl-C within 5s to abort\n",
		len(pages)*(1+len(gaps)), len(gaps))
	time.Sleep(5 * time.Second)

	t0 := time.Now()

	// Warm every page (first sight — always a fetch), concurrently so all 30 land
	// within seconds of each other, then read the log once.
	fmt.Println("[ttlmap] warming all pages …")
	runConcurrent(len(pages), ttlConcurrency, func(i int) {
		fireTTLAsk(pages[i], byName[pages[i].Provider], force, ttlmapQuestion)
	})
	time.Sleep(20 * time.Second)
	warmRecs, _, _ := probe.LoadRecords(probeLogPath, "", time.Time{})
	for _, pg := range pages {
		warmHits := fetchesFor(warmRecs, pg.PageID, host, pg.WarmUnix, 150)
		pg.WarmFetches = len(warmHits)
		if len(warmHits) > 0 {
			pg.Served, pg.ServedOK = warmHits[0], true
		}
		ttlmapEmitWarm(runID, pg, force)
		if pg.Setting != "" {
			if g := gradeCacheReuse(pg); g.Drift != "" {
				fmt.Printf("  warm DRIFT %s: %s — this page's grade is about the wrong header\n", pg.PageID, g.Drift)
			}
		}
		if pg.WarmFetches == 0 {
			fmt.Printf("  warm WARNING %s: no fetch reached us on warm — its held polls are not judgeable\n", pg.PageID)
		}
	}

	// Poll rounds. Asks within a round run concurrently so a 30-page round finishes
	// well inside the cadence; each page's gap is measured from ITS OWN warm, so the
	// warm order never biases one arm's TTL against another's.
	for _, g := range gaps {
		target := t0.Add(g)
		if d := time.Until(target); d > 0 {
			time.Sleep(d)
		}
		var live []*ttlPage
		for _, pg := range pages {
			if pg.Retired {
				continue
			}
			pg.pendingPoll = &ttlPoll{NominalGap: g}
			live = append(live, pg)
		}
		if len(live) == 0 {
			fmt.Println("[ttlmap] every page has re-fetched; stopping early.")
			break
		}
		fmt.Printf("[ttlmap] poll round nominal gap %s — %d live page(s) …\n", g, len(live))
		runConcurrent(len(live), ttlConcurrency, func(i int) {
			firePoll(live[i], byName[live[i].Provider], live[i].pendingPoll, force)
		})
		time.Sleep(20 * time.Second) // let this round's fetches land before the single read
		recs, _, _ := probe.LoadRecords(probeLogPath, "", time.Time{})
		for _, pg := range live {
			poll := pg.pendingPoll
			poll.Fetches = countHits(recs, pg.PageID, host, poll.FiredUnix, 150)
			pg.Polls = append(pg.Polls, *poll)
			ttlmapEmitPoll(runID, pg, *poll, force)
			status := "held"
			if poll.Fetches > 0 {
				status = "RE-FETCHED"
				if !pg.Refetched {
					pg.Refetched = true
					pg.FirstRefetchS = poll.GapSec
				}
				pg.Retired = retireAfter(true)
			}
			fmt.Printf("  %-20s r%-2d gap=%5.1fm -> %s (marker=%v)\n",
				pg.Arm, pg.Rep, poll.GapSec/60, status, poll.Marker)
			pg.pendingPoll = nil
		}
	}
	return reportTTLMap(pages)
}

// fireTTLAsk warms one page (Seq 0). Fetch counting is done by the caller in one
// batched read after all warms, so this only asks and records the reply.
func fireTTLAsk(pg *ttlPage, p Caller, force bool, question string) {
	q, wanted := pg.askFor(0)
	if pg.depthCodes == nil {
		q = question
	}
	pg.WarmAsked, pg.WarmWanted = q, wanted
	sent := time.Now().UTC()
	pg.WarmUnix = unixf(sent)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	ans, usage, err := p.Ask(ctx, os.Getenv(p.EnvKey), p.Model, pg.URL, q, force)
	cancel()
	pg.WarmAnswer = logAnswer(ans, usage, 500)
	pg.WarmMarker = containsFold(ans, wanted)
	pg.WarmUsage = usage
	warnIfAnomalous(fmt.Sprintf("ttlmap warm %s [%s]", pg.Arm, pg.Provider), usage)
	if err != nil {
		pg.WarmErr = err.Error()
	}
}

// firePoll asks one page at one round and stamps the ACTUAL gap since THIS PAGE's
// own warm, so neither a late round nor the warm order can distort the copy's age.
func firePoll(pg *ttlPage, p Caller, poll *ttlPoll, force bool) {
	// Ask number: the first ask is 0, so this page's next re-ask is len(Polls)+1.
	poll.Asked, poll.Wanted = pg.askFor(len(pg.Polls) + 1)
	sent := time.Now().UTC()
	poll.FiredUnix = unixf(sent)
	poll.GapSec = poll.FiredUnix - pg.WarmUnix
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	ans, usage, err := p.Ask(ctx, os.Getenv(p.EnvKey), p.Model, pg.URL, poll.Asked, force)
	cancel()
	poll.Answer = logAnswer(ans, usage, 500)
	poll.Marker = containsFold(ans, poll.Wanted)
	poll.Usage = usage
	warnIfAnomalous(fmt.Sprintf("ttlmap poll %s [%s] gap=%.0fm", pg.Arm, pg.Provider, poll.GapSec/60), usage)
	if err != nil {
		poll.Err = err.Error()
	}
}

func ttlmapEmitWarm(runID string, pg *ttlPage, force bool) {
	m := resultMeta{
		RunID: runID, Experiment: "ttlmap",
		EpisodeID: runID + "-" + pg.PageID, Step: "warm",
		Surface: "api", Vendor: pg.Provider, Model: pg.Model, ToolForced: force, Marker: pg.WarmWanted,
	}
	rec := resultFromAsk(m, pg.PageID+"#warm", askOutcome{
		URL: pg.URL, Token: pg.PageID, Question: pg.WarmAsked, Answer: pg.WarmAnswer,
		ErrStr: pg.WarmErr, MarkerPresent: pg.WarmMarker, SentUnix: pg.WarmUnix, FetchesSnapshot: pg.WarmFetches,
		InputTokens: pg.WarmUsage.InputTokens, OutputTokens: pg.WarmUsage.OutputTokens, Rounds: pg.WarmUsage.Rounds,
	}, map[string]any{
		"arm":              pg.Arm,
		"replicate":        float64(pg.Rep),
		"gap_sec":          float64(0),
		"first_refetch":    false,
		"fetches_snapshot": float64(pg.WarmFetches),
	})
	if err := emitResults(m.Experiment, runID, []probe.ResultRecord{rec}); err != nil {
		fmt.Fprintf(os.Stderr, "  result record write failed (ttlmap warm %s): %v\n", pg.PageID, err)
	}
}

func ttlmapEmitPoll(runID string, pg *ttlPage, poll ttlPoll, force bool) {
	m := resultMeta{
		RunID: runID, Experiment: "ttlmap",
		EpisodeID: runID + "-" + pg.PageID, Step: "poll",
		Surface: "api", Vendor: pg.Provider, Model: pg.Model, ToolForced: force, Marker: poll.Wanted,
	}
	rec := resultFromAsk(m, fmt.Sprintf("%s#poll-%s", pg.PageID, compactDur(poll.NominalGap)), askOutcome{
		URL: pg.URL, Token: pg.PageID, Question: poll.Asked, Answer: poll.Answer,
		ErrStr: poll.Err, MarkerPresent: poll.Marker, SentUnix: poll.FiredUnix, FetchesSnapshot: poll.Fetches,
		InputTokens: poll.Usage.InputTokens, OutputTokens: poll.Usage.OutputTokens, Rounds: poll.Usage.Rounds,
	}, map[string]any{
		"arm":              pg.Arm,
		"replicate":        float64(pg.Rep),
		"nominal_gap":      poll.NominalGap.String(),
		"gap_sec":          poll.GapSec,
		"first_refetch":    poll.Fetches > 0,
		"fetches_snapshot": float64(poll.Fetches),
	})
	if err := emitResults(m.Experiment, runID, []probe.ResultRecord{rec}); err != nil {
		fmt.Fprintf(os.Stderr, "  result record write failed (ttlmap poll %s): %v\n", pg.PageID, err)
	}
}

// cacheGrade is one cache-header test page measured against RFC 9111.
type cacheGrade struct {
	Judged        bool             // a graded arm whose warm fetch reached us
	Limit         probe.ReuseLimit // what the spec allows a shared cache for the headers we sent
	LongestReuseS float64          // longest gap at which the vendor answered from its copy
	Past          bool             // some reuse went past Limit (never for LimitNone)
	Drift         string           // non-empty when what we sent is not what the arm declared
}

// gradeCacheReuse grades one page. A REUSE is a poll where no fetch reached us AND
// the answer carried this page's code — no fetch without the code means the model
// never had the page, which is a different finding, not a cache hit. The limit is
// computed from the headers the log shows we actually sent on the warm fetch,
// dated by that fetch, so a mis-deployed origin cannot be graded as if it were right.
func gradeCacheReuse(pg *ttlPage) cacheGrade {
	var g cacheGrade
	if pg.Setting == "" || !pg.ServedOK || pg.WarmFetches == 0 {
		return g
	}
	g.Judged = true
	s := pg.Served
	if want := probe.CacheSettings[pg.Setting]; s.CacheControlResp != want.CacheControl || s.ExpiresSent != want.Expires {
		g.Drift = fmt.Sprintf("served Cache-Control %q / Expires %q, but setting %q declares %q / %q",
			s.CacheControlResp, s.ExpiresSent, pg.Setting, want.CacheControl, want.Expires)
	}
	if (pg.Validators == probe.ValidatorsNone) != (s.ETagSent == "" && s.LastModifiedSent == "") {
		g.Drift = strings.TrimPrefix(g.Drift+"; ", "; ") + fmt.Sprintf("version info %q but served ETag %q / Last-Modified %q", pg.Validators, s.ETagSent, s.LastModifiedSent)
	}
	var lm time.Time
	if s.LastModifiedSent != "" {
		lm, _ = http.ParseTime(s.LastModifiedSent)
	}
	date := time.Unix(0, int64(s.TSUnix*1e9)).UTC()
	g.Limit = probe.SharedReuseLimit(s.CacheControlResp, s.ExpiresSent, date, lm)
	for _, p := range pg.Polls {
		if p.Fetches > 0 || !p.Marker {
			continue
		}
		if p.GapSec > g.LongestReuseS {
			g.LongestReuseS = p.GapSec
		}
		if g.Limit.Kind != probe.LimitNone && p.GapSec > g.Limit.Max.Seconds() {
			g.Past = true
		}
	}
	return g
}

// reportTTLMap prints each page's copy lifetime (the gap of its first re-fetch)
// and the per-model spread. FACTS with the derived TTL; the honest caveat is the
// resolution is the cadence and a page whose warm never fetched is dropped.
func reportTTLMap(pages []*ttlPage) int {
	fmt.Println("\n" + strings.Repeat("=", 74))
	fmt.Println("  TTL MAP — how long each model's copy lives (gap of the first re-fetch)")
	fmt.Println(strings.Repeat("=", 74))
	fmt.Println("  (resolution = the poll cadence; a page whose warm never fetched is dropped)")

	type grp struct {
		key   string
		ttls  []float64 // minutes, only pages that re-fetched
		never int       // still held at the end of the window
		unjud int       // warm never fetched
		// cache-header test only
		graded  int
		past    int
		longest float64 // seconds
		limit   probe.ReuseLimit
		drift   int
		// keep-asking runs: every re-ask, and the ones that reached our server
		reasks, reasksFetched int
	}
	groups := map[string]*grp{}
	var order []string
	for _, pg := range pages {
		key := pg.Arm + "  [" + pg.Provider + "-" + shortModel(pg.Model) + "]"
		g := groups[key]
		if g == nil {
			g = &grp{key: key}
			groups[key] = g
			order = append(order, key)
		}
		if pg.WarmFetches == 0 {
			g.unjud++
			continue
		}
		for _, p := range pg.Polls {
			g.reasks++
			if p.Fetches > 0 {
				g.reasksFetched++
			}
		}
		if cg := gradeCacheReuse(pg); cg.Judged {
			if g.graded == 0 {
				g.limit = cg.Limit // the same headers on every page of an arm; a guess varies by seconds
			}
			g.graded++
			if cg.Past {
				g.past++
			}
			if cg.LongestReuseS > g.longest {
				g.longest = cg.LongestReuseS
			}
			if cg.Drift != "" {
				g.drift++
			}
		}
		if pg.Refetched {
			g.ttls = append(g.ttls, pg.FirstRefetchS/60)
		} else {
			g.never++
		}
	}
	sort.Strings(order)
	for _, key := range order {
		g := groups[key]
		fmt.Printf("\n  %s\n", key)
		if ttlmapKeepAsking {
			// Every page was asked at every gap, so this is the direct answer to
			// "fetched fresh every time, once, or never". (The longest-reuse figure
			// below is timed from the first ask, so after a fetch-again it overstates.)
			fmt.Printf("    fetched again on %d of %d re-asks\n", g.reasksFetched, g.reasks)
		}
		if g.graded > 0 {
			l := g.limit
			switch l.Kind {
			case probe.LimitNone:
				fmt.Printf("    spec limit for a shared cache: none stated (%s)\n", l.Why)
			default:
				fmt.Printf("    spec limit for a shared cache (%s): reuse unchecked for at most %s — %s\n", l.Kind, l.Max.Round(time.Second), l.Why)
			}
			fmt.Printf("    answered from its copy past that limit: %d of %d pages; longest reuse without checking %.1f min\n",
				g.past, g.graded, g.longest/60)
			if g.drift > 0 {
				fmt.Printf("    WARNING: %d page(s) were served headers other than this setting declares — see the DRIFT lines above\n", g.drift)
			}
		}
		if len(g.ttls) == 0 {
			fmt.Printf("    no page re-fetched inside the window (held=%d, unjudgeable=%d)\n", g.never, g.unjud)
			continue
		}
		sort.Float64s(g.ttls)
		sum := 0.0
		for _, v := range g.ttls {
			sum += v
		}
		fmt.Printf("    copy lifetime (min): min %.1f  median %.1f  max %.1f  mean %.1f  (n=%d re-fetched",
			g.ttls[0], median(g.ttls), g.ttls[len(g.ttls)-1], sum/float64(len(g.ttls)), len(g.ttls))
		if g.never > 0 {
			fmt.Printf(", %d still held at window end", g.never)
		}
		if g.unjud > 0 {
			fmt.Printf(", %d unjudgeable", g.unjud)
		}
		fmt.Println(")")
	}
	return 0
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// shortModel trims a vendor/model slug to its last segment for compact printing.
func shortModel(m string) string {
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		return m[i+1:]
	}
	return m
}
