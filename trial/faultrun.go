package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"aisearch-probe/internal/probe"
)

// THE FAILURE-RETRY EXPERIMENT, driver side.
//
// The server side (proxy/faults.go) serves pages that misbehave on purpose.
// This asks each provider to fetch each one, waits, and then counts — from our
// own request log — how many times the provider's fetcher actually arrived, how
// long the retries were spread over, and whether it fetched anything we did not
// name. That count is the finding; the model's answer is almost beside the point
// here, because the failure means it usually cannot answer at all.
//
// It is the mirror of the caching experiments: those read meaning from the
// ABSENCE of a fetch, this reads it from a SURPLUS of fetches.
//
// WHY THE VERDICT IS ABOUT CORRECTNESS, NOT JUST COUNT. Each fault is labelled
// with whether a well-behaved client should retry it (faultExpectations below).
// Retrying a 404 is waste; ignoring Retry-After on a 503 is guessing at a delay
// the server already gave. So a bare count is turned into "as expected" or a
// named departure from it.
//
// A FRESH ADDRESS PER PROVIDER. Each provider gets its own ?probe= token and,
// for the flaky fault, its own attempt counter follows that token — so one
// provider's retries are never counted against another. A provider that strips
// the token still arrives on the fault path within the window and is counted by
// time, reported as untokenised (see attribution note in probeurl.go).

// FaultSpec is one misbehaving path and what a good client should do with it.
type FaultSpec struct {
	Name        string // path under /fault/, e.g. "500" or "flaky/2"
	Kind        string // permanent | transient | timing | body | transport | recovery
	ShouldRetry bool
	Expectation string // what a well-behaved fetcher would do
}

// faultBattery is the default set, matching proxy/faults.go. flaky/2 is included
// last because it is the only stateful one and the only one that can succeed.
var faultBattery = []FaultSpec{
	{"404", "permanent", false, "should NOT retry — the page is gone"},
	{"500", "transient", true, "may retry a few times, then give up"},
	{"503", "transient", true, "should retry, and wait the Retry-After: 10 it was given"},
	{"slow", "timing", false, "should wait for the slow 200, not refetch"},
	{"empty", "body", false, "should accept the empty 200 as success, not retry"},
	{"reset", "transport", true, "may retry a dropped connection"},
	{"429", "transient", true, "should retry, and wait the Retry-After: 10 it was given"},
	{"429date", "transient", true, "should retry, and wait the Retry-After given as an HTTP-date (~10s)"},
	{"403", "permanent", false, "should NOT retry — and should not come back as someone else"},
	{"loop/a", "redirect", false, "should follow a few hops of the a↔b loop, then give up"},
	{"cut", "truncated", true, "should notice the body is shorter than promised: retry or report failure"},
	{"flaky/2", "recovery", true, "should retry past 2 failures and get the page"},
}

// faultLoopHopCap mirrors loopHopCap in proxy/faults.go: past it the server
// answers 508, so a hop count at or above it means OUR cap stopped the fetcher,
// not the fetcher itself.
const faultLoopHopCap = 100

// faultDepthID is the id the server keyed the depth markers on: the ?probe=
// VALUE, while the runner's token is the whole "probe=…" pair.
func faultDepthID(token string) string {
	return strings.TrimPrefix(token, "probe=")
}

// pickFaults is the -fault-kinds selection: a comma-separated list of fault
// names, or "" for the usual battery. A battery name gives the battery's own
// entry; "flaky/N" (any N >= 0) gives a recovery fault, so flaky/0 — a page that
// works at once — can serve as the success baseline asked with the fault
// question. Anything else is an error: a typo must never run something unexpected.
func pickFaults(names string) ([]FaultSpec, error) {
	if strings.TrimSpace(names) == "" {
		return faultBattery, nil
	}
	var out []FaultSpec
	for _, n := range strings.Split(names, ",") {
		n = strings.TrimSpace(n)
		found := false
		for _, f := range faultBattery {
			if f.Name == n {
				out = append(out, f)
				found = true
				break
			}
		}
		if found {
			continue
		}
		if k, ok := strings.CutPrefix(n, "flaky/"); ok {
			if c, err := strconv.Atoi(k); err == nil && c >= 0 {
				out = append(out, FaultSpec{n, "recovery", true,
					fmt.Sprintf("should retry past %d failure(s) and get the page", c)})
				continue
			}
		}
		return nil, fmt.Errorf("-fault-kinds: unknown fault %q", n)
	}
	return out, nil
}

// FaultResult is what one provider did with one fault.
type FaultResult struct {
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	Fault       string  `json:"fault"`
	Kind        string  `json:"kind"`
	RunID       string  `json:"run_id"`
	ShouldRetry bool    `json:"should_retry"`
	URL         string  `json:"url"`
	Token       string  `json:"token"`
	SentAt      string  `json:"sent_at"`
	SentUnix    float64 `json:"sent_unix"`
	// Hits is every arrival we saw, in order, as "time bot status bytes url".
	Hits        []string `json:"hits"`
	Fetches     int      `json:"fetches"`       // total arrivals on the fault path
	OffPathHits int      `json:"off_path_hits"` // arrivals for a path we did not name
	SpanSeconds float64  `json:"span_seconds"`  // first to last arrival
	Untokenised int      `json:"untokenised"`   // arrivals with our token stripped
	// DeepestDepth is the deepest depth marker (0/25/50/75/95) the answer quoted,
	// -1 for none. On /fault/cut it says how much of the page the model got.
	DeepestDepth int `json:"deepest_depth"`
	// UserAgents and ClientIPs are the distinct identities that arrived on this
	// fault, in first-seen order. On 403 a second user agent means the fetcher
	// came back as someone else. IPs alone vary across a provider's pool.
	UserAgents []string `json:"user_agents,omitempty"`
	ClientIPs  []string `json:"client_ips,omitempty"`
	Answer     string   `json:"answer"`
	Error      string   `json:"error,omitempty"`
	Verdict    string   `json:"verdict"`
	// Usage carries the call's token cost to the standard record. Not serialised
	// to the bespoke fault file (json:"-"), so that file's schema is unchanged.
	Usage probe.Usage `json:"-"`
}

// runFaults asks every active provider about every fault and writes one
// FaultResult per pairing. Returns a process exit code.
func runFaults(base string, provs []Caller, faults []FaultSpec,
	window time.Duration, force bool, outPath, runID, secret string) int {

	if base == "" {
		fmt.Fprintln(os.Stderr, "error: -siteUrl is required — the base address whose /fault/ paths are asked about")
		return 1
	}
	if probeLogPath == "" {
		// The whole measurement is what arrives at our server; without the log
		// there is nothing to read and the run would only spend money.
		fmt.Fprintln(os.Stderr, "error: -serverLog is required; the retry count is read from it")
		return 1
	}
	if secret == "" {
		// The content-ending faults serve the depth page; its markers are HMACs,
		// and the runner needs the same secret to read how deep the model got.
		fmt.Fprintln(os.Stderr, "error: -pageCodeSecret (PROBE_PAGE_CODE_SECRET) is required; the fault pages carry depth markers the runner must recompute")
		return 1
	}
	host := hostOfURL(base)
	if host == "" {
		fmt.Fprintf(os.Stderr, "error: -siteUrl %q has no host\n", base)
		return 1
	}
	// A fresh nonce per run, so this run's URLs collide with no previous run's
	// cached copy or stateful flaky counter. See runNonce (probeurl.go).
	if runID == "" {
		runID = runNonce()
	}
	fmt.Printf("[faults] run id: %s (each provider's URLs carry it, so a repeat run is clean)\n", runID)

	if err := faultPreflight(base, secret, runID); err != nil {
		fmt.Fprintf(os.Stderr, "error: preflight failed — %v\n", err)
		return 1
	}
	fmt.Println("[faults] preflight ok — the origin serves depth-bodied faults under our secret")

	log, err := newTrialLog(outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer log.Close()

	var results []FaultResult
	for _, p := range provs {
		fmt.Printf("\n\033[1m=== %s (%s)\033[0m\n", p.Name, p.Model)
		for _, fault := range faults {
			r := askFault(base, host, p, fault, window, force, runID, secret)
			results = append(results, r)
			if err := log.WriteAny(r); err != nil {
				fmt.Fprintf(os.Stderr, "  log write failed: %v\n", err)
			}
			faultEmit(r, force)
			printFaultResult(r)
		}
	}

	fmt.Printf("\n[faults] results written to %s\n", outPath)
	return reportFaults(results)
}

// faultMarker names what marker_present tests on a fault record: any of this
// caller's TRUNC- depth codes quoted (DeepestDepth >= 0). Not the shared
// RCP-2026-08 line, which the pages still carry for the negative-caching runner
// but which proves nothing about THIS fetch.
const faultMarker = "TRUNC- depth code"

// faultEmit writes one fault ask's standard record.
func faultEmit(r FaultResult, force bool) {
	rec := faultRecord(r, force)
	if err := emitResults(rec.Experiment, r.RunID, []probe.ResultRecord{rec}); err != nil {
		fmt.Fprintf(os.Stderr, "  result record write failed (%s %s): %v\n", r.Provider, r.Fault, err)
	}
}

// faultRecord builds one fault ask's standard record. A fault is a single ask,
// so its episode is itself. The detailed arrival breakdown (hits, off-path,
// untokenised) stays in the bespoke FaultResult file; the record keeps the
// answer, the token to re-derive fetches, the call's cost (tokens and rounds),
// and a labelled snapshot of the counts.
func faultRecord(r FaultResult, force bool) probe.ResultRecord {
	m := resultMeta{
		RunID: r.RunID, Experiment: "faults",
		EpisodeID: r.RunID + "-" + r.Provider + "-" + r.Fault, Step: "",
		Surface: "api", Vendor: r.Provider, Model: r.Model, ToolForced: force, Marker: faultMarker,
	}
	return resultFromAsk(m, m.EpisodeID, askOutcome{
		URL: r.URL, Token: r.Token, Question: faultQuestion(FaultSpec{Name: r.Fault}),
		Answer: r.Answer, ErrStr: r.Error, MarkerPresent: r.DeepestDepth >= 0,
		SentUnix: r.SentUnix, FetchesSnapshot: r.Fetches,
		InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens, Rounds: r.Usage.Rounds,
	}, map[string]any{
		"fault":                r.Fault,
		"kind":                 r.Kind,
		"should_retry":         r.ShouldRetry,
		"fetches_snapshot":     float64(r.Fetches),
		"off_path_snapshot":    float64(r.OffPathHits),
		"span_seconds":         r.SpanSeconds,
		"untokenised_snapshot": float64(r.Untokenised),
		"deepest_depth":        float64(r.DeepestDepth),
		"user_agents":          strings.Join(r.UserAgents, " | "),
		"client_ips":           strings.Join(r.ClientIPs, " | "),
	})
}

// faultPreflight fetches one depth-bodied fault (flaky/0 succeeds at once) under
// the instrument user-agent, and checks the runner can read every marker with
// its OWN secret. That proves, before any money is spent, that the deployed
// origin has the new fault code and the same page-code secret. A mismatch would
// otherwise score every answer "no marker" and read as "the model never got it".
func faultPreflight(base, secret, runID string) error {
	id := sanitizeTokenField("preflight-" + runID)
	u := strings.TrimRight(base, "/") + "/fault/flaky/0?probe=" + id
	body, err := fetchPageBody(u)
	if err != nil {
		return fmt.Errorf("GET %s: %v (is the new fault code deployed?)", u, err)
	}
	if d := probe.DeepestDepth(body, secret, id, probe.DepthPageRevision); d != probe.DepthPercents[len(probe.DepthPercents)-1] {
		return fmt.Errorf("read deepest marker %d from %s, want %d — the origin is on old fault code, or its page-code secret differs from ours",
			d, u, probe.DepthPercents[len(probe.DepthPercents)-1])
	}
	return nil
}

// askFault asks one provider about one fault path, waits, and counts arrivals.
func askFault(base, host string, p Caller, fault FaultSpec, window time.Duration, force bool, runID, secret string) FaultResult {
	// The fault lives in the path; the token rides in the query for attribution.
	// The token is provider+model+run so a repeat run gets fresh URLs (no cache
	// collision) and the flaky counter, keyed on the token, resets per run.
	faultPath := "/fault/" + fault.Name
	url, token := probeURLFor(strings.TrimRight(base, "/")+faultPath, p.Tag()+"-"+runID)

	fmt.Printf("\n  \033[1m%s\033[0m — %s\n", fault.Name, fault.Expectation)
	fmt.Printf("    address : %s\n", url)

	r := FaultResult{
		Provider: p.Name, Model: p.Model, Fault: fault.Name, Kind: fault.Kind,
		RunID: runID, ShouldRetry: fault.ShouldRetry, URL: url, Token: token,
	}

	sent := time.Now().UTC()
	r.SentAt = sent.Format(time.RFC3339Nano)
	r.SentUnix = float64(sent.UnixNano()) / 1e9

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	answer, usage, aerr := p.Ask(ctx, os.Getenv(p.EnvKey), p.Model, url, faultQuestion(fault), force)
	r.Answer = truncate(answer, 2000)
	r.Usage = usage
	if aerr != nil {
		// A tool-call error is expected here — the page failed — so it is not
		// fatal to the measurement. Record it and still read the log: the
		// fetches happened regardless of what the model finally reported.
		r.Error = aerr.Error()
	}

	// Wait out the window before counting, so a slow retry is not missed. The
	// slow fault holds the socket ~25s and a 503 asks for a 10s wait, so the
	// window must comfortably exceed both.
	time.Sleep(window)

	recs, _, lerr := probe.LoadRecords(probeLogPath, "", time.Time{})
	if lerr != nil {
		r.Error = strings.TrimSpace(r.Error + " ; probe log unreadable: " + lerr.Error())
		r.Verdict = "void — log unreadable"
		return r
	}

	secs := window.Seconds()
	var first, last float64
	for _, rec := range recs {
		if rec.TSUnix < r.SentUnix || rec.TSUnix > r.SentUnix+secs {
			continue
		}
		if strings.HasPrefix(rec.Bot, "probe-") {
			continue // our own liveness/self-check requests are the instrument
		}
		if hostOfURL(rec.URL) != host {
			continue
		}
		onThisFault := strings.Contains(rec.URL, token) ||
			(strings.Contains(rec.URL, faultPath) && !strings.Contains(rec.URL, "probe="))
		onAnyFault := strings.Contains(rec.PageKey, "/fault/")

		switch {
		case onThisFault:
			r.Fetches++
			if !strings.Contains(rec.URL, token) {
				r.Untokenised++
			}
			r.UserAgents = appendDistinct(r.UserAgents, rec.UserAgent)
			r.ClientIPs = appendDistinct(r.ClientIPs, rec.ClientIP)
			if first == 0 {
				first = rec.TSUnix
			}
			last = rec.TSUnix
			r.Hits = append(r.Hits, fmt.Sprintf("%s %s %d %dB %s",
				rec.TS[11:19], rec.Bot, rec.StatusCode, rec.BytesSent, pathOf(rec.URL)))
		case !onAnyFault && strings.Contains(rec.URL, token):
			// Our token on a NON-fault path: the fetcher wandered to a page we
			// did not name (e.g. the site root), which is the amplification
			// behaviour worth recording separately.
			r.OffPathHits++
			r.Hits = append(r.Hits, fmt.Sprintf("%s %s %d %dB %s (off-path)",
				rec.TS[11:19], rec.Bot, rec.StatusCode, rec.BytesSent, pathOf(rec.URL)))
		}
	}
	if r.Fetches > 0 {
		r.SpanSeconds = last - first
	}
	// The server keyed the markers on our token's value, or on "untokenised"
	// when the fetcher stripped it; take the deeper reading of the two.
	r.DeepestDepth = probe.DeepestDepth(r.Answer, secret, faultDepthID(token), probe.DepthPageRevision)
	if d := probe.DeepestDepth(r.Answer, secret, "untokenised", probe.DepthPageRevision); d > r.DeepestDepth {
		r.DeepestDepth = d
	}
	r.Verdict = faultVerdict(fault, r)
	return r
}

// appendDistinct appends v unless it is empty or already present, keeping
// first-seen order.
func appendDistinct(list []string, v string) []string {
	if v == "" {
		return list
	}
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// faultQuestion asks something the page would answer if it worked, so a fetcher
// that DOES get through (flaky, slow, heal) can be seen to have. The TRUNC-
// sentence is the page-size experiment's own (truncScanQuestion) with "has"
// softened to "may have": most fault pages carry no codes, and promising codes
// that are not there could make a model call its fetch tool again — inflating
// the very fetch count this experiment measures. It does not ask for the shared
// RCP-2026-08 code: that string is the same on every page and every run, so a
// model could repeat it without this fetch; the TRUNC- codes are unforgeable and
// fresh per caller, and the 0% one sits at the top of the page.
func faultQuestion(f FaultSpec) string {
	return "What does this page say? This page may have marker codes that begin with TRUNC- " +
		"at different points from top to bottom. Quote every TRUNC- code you can see on the page, exactly as written."
}

// faultVerdict turns the count into a plain reading against what a good client
// should have done. It never claims more than the numbers show.
func faultVerdict(f FaultSpec, r FaultResult) string {
	if r.Fetches == 0 && r.Error != "" {
		return "void — nothing reached us and the call errored; can't tell a refusal from a silent drop"
	}
	if r.Fetches == 0 {
		return "no fetch reached our server at all"
	}
	retried := r.Fetches > 1
	extra := ""
	if r.OffPathHits > 0 {
		extra = fmt.Sprintf("; also fetched %d page(s) we did not name (amplification)", r.OffPathHits)
	}
	if f.Kind == "permanent" && len(r.UserAgents) > 1 {
		// The bot-block question is "did it come back as someone else AFTER the
		// refusal". Several agents alone cannot show that: run 1 (2026-10-08) saw
		// xAI use the same two browser agents on every fault, its requests sent
		// together before any 403 could arrive. So report the count, not a motive.
		extra += fmt.Sprintf("; arrived under %d user agents (%s) — compare with its other faults before reading this as dodging the block",
			len(r.UserAgents), strings.Join(r.UserAgents, " | "))
	}
	switch {
	case f.Kind == "timing" && retried:
		// The page answers only after slowFaultDelay (~25 s). A repeat before
		// that is the fetcher giving up and asking again, or several sent at
		// once — not a retry of a failure it should have left alone.
		return fmt.Sprintf("%d fetches over %.0fs before the slow answer — timed out and asked again, or sent several at once%s",
			r.Fetches, r.SpanSeconds, extra)
	case f.Kind == "truncated":
		// Only meaningful once the live tunnel check shows a short body really
		// reaches the fetcher (cloudflared may turn the cut into a 502).
		const caveat = " — valid only if the tunnel check showed a partial body arriving"
		switch {
		case r.DeepestDepth < 0:
			return fmt.Sprintf("model quoted no depth marker after %d fetch(es)%s", r.Fetches, extra)
		case r.DeepestDepth >= probe.DepthPercents[len(probe.DepthPercents)-1]:
			return fmt.Sprintf("model quoted the deepest marker — it had the whole page despite the cut (%d fetches)%s", r.Fetches, extra)
		default:
			return fmt.Sprintf("model quoted markers only to %d%% — it was handed the partial page (%d fetch(es)); read the answer for whether it said so%s%s",
				r.DeepestDepth, r.Fetches, extra, caveat)
		}
	case f.Kind == "redirect":
		// Arrivals on a loop are hops, not retries.
		if r.Fetches >= faultLoopHopCap {
			return fmt.Sprintf("followed %d hops until the server's hop cap stopped it%s", r.Fetches, extra)
		}
		return fmt.Sprintf("followed %d hops, then stopped%s", r.Fetches, extra)
	case f.Kind == "recovery":
		// Read from the depth codes only: "recovered" in prose, or the shared
		// RCP code, proves nothing about this fetch.
		if r.DeepestDepth >= 0 {
			return fmt.Sprintf("recovered after %d fetches%s — retried past the failures", r.Fetches, extra)
		}
		return fmt.Sprintf("gave up after %d fetch(es); did not retry enough to recover%s", r.Fetches, extra)
	case retried && !f.ShouldRetry:
		return fmt.Sprintf("retried %d times a failure it SHOULD NOT have (%s)%s", r.Fetches, f.Kind, extra)
	case retried && f.ShouldRetry:
		return fmt.Sprintf("retried %d times over %.0fs%s", r.Fetches, r.SpanSeconds, extra)
	case !retried:
		return fmt.Sprintf("fetched once, did not retry%s", extra)
	}
	return fmt.Sprintf("%d fetch(es)%s", r.Fetches, extra)
}

func printFaultResult(r FaultResult) {
	if r.Error != "" {
		fmt.Printf("    tool result: %s\n", truncate(r.Error, 120))
	}
	fmt.Printf("    fetches reaching us: %d", r.Fetches)
	if r.OffPathHits > 0 {
		fmt.Printf("  (+%d off-path)", r.OffPathHits)
	}
	if r.Untokenised > 0 {
		fmt.Printf("  [%d untokenised]", r.Untokenised)
	}
	fmt.Println()
	for _, h := range r.Hits {
		fmt.Printf("      %s\n", h)
	}
	fmt.Printf("    \033[1m→\033[0m %s\n", r.Verdict)
}

// faultDepartures picks the results worth naming: retries of a failure a good
// client leaves alone, and fetches of pages we never named. A redirect loop's
// arrivals are hops, and a slow page's repeats are timeouts — neither is a retry
// of a failure, so neither is listed as one.
func faultDepartures(results []FaultResult) (wrong, amp []FaultResult) {
	for _, r := range results {
		if r.Fetches > 1 && !r.ShouldRetry && r.Kind != "redirect" && r.Kind != "timing" {
			wrong = append(wrong, r)
		}
		if r.OffPathHits > 0 {
			amp = append(amp, r)
		}
	}
	return wrong, amp
}

// reportFaults prints the grid: provider down the side, fault across, retry
// count in each cell — then the departures from correct behaviour.
func reportFaults(results []FaultResult) int {
	if len(results) == 0 {
		fmt.Println("\nno faults were run — nothing to report")
		return 1
	}

	provs := []string{}
	faults := []string{}
	cell := map[string]FaultResult{}
	seenP := map[string]bool{}
	seenF := map[string]bool{}
	for _, r := range results {
		key := r.Provider + "|" + r.Model
		if !seenP[key] {
			seenP[key] = true
			provs = append(provs, key)
		}
		if !seenF[r.Fault] {
			seenF[r.Fault] = true
			faults = append(faults, r.Fault)
		}
		cell[key+"|"+r.Fault] = r
	}
	sort.Strings(provs)

	fmt.Println("\n" + strings.Repeat("=", 78))
	fmt.Println("  FETCHES REACHING OUR SERVER — how many times each fault was fetched")
	fmt.Println(strings.Repeat("=", 78))
	fmt.Printf("  %-26s", "provider")
	for _, f := range faults {
		fmt.Printf(" %-8s", f)
	}
	fmt.Println()
	for _, pk := range provs {
		name := pk
		if i := strings.Index(pk, "|"); i >= 0 {
			name = pk[:i]
		}
		fmt.Printf("  %-26s", truncate(name, 26))
		for _, f := range faults {
			r := cell[pk+"|"+f]
			mark := fmt.Sprintf("%d", r.Fetches)
			if r.OffPathHits > 0 {
				mark += fmt.Sprintf("+%d", r.OffPathHits)
			}
			fmt.Printf(" %-8s", mark)
		}
		fmt.Println()
	}
	fmt.Println("\n  (a cell of 1 = fetched once, no retry; N = N arrivals; +M = M off-path)")

	wrong, amp := faultDepartures(results)

	fmt.Println("\n" + strings.Repeat("=", 78))
	fmt.Println("  THE READING")
	fmt.Println(strings.Repeat("=", 78))

	if len(wrong) > 0 {
		fmt.Println("\n  RETRIED A FAILURE IT SHOULD HAVE LEFT ALONE:")
		for _, r := range wrong {
			fmt.Printf("    %s — /fault/%s (%s), %d fetches\n", r.Provider, r.Fault, r.Kind, r.Fetches)
		}
	}
	if len(amp) > 0 {
		fmt.Println("\n  FETCHED PAGES IT WAS NOT GIVEN (reflection/amplification):")
		for _, r := range amp {
			fmt.Printf("    %s — asked about /fault/%s, also hit %d other page(s)\n", r.Provider, r.Fault, r.OffPathHits)
		}
	}
	if len(wrong) == 0 && len(amp) == 0 {
		fmt.Println("\n  No provider retried a permanent failure or fetched an unnamed page.")
	}
	return 0
}
