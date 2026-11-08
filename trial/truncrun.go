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

// THE READ-DEPTH TEST runner (client side).
//
// Where does each AI fetcher CUT a page? A depth page (proxy/depthpage.go) carries
// unforgeable marker codes at known byte depths; the deepest code a model can quote
// is the deepest it truly read.
//
// One "scan" ask per page ("quote every TRUNC- code you can see"). The deepest
// returned code = the cut, in bytes, for that fetch. Swept across a size ladder so
// the cut is found whether it sits low or high. The same runner serves the
// document-type, dynamic-filtering and JavaScript tests, on their own pages.
//
// FACTS ONLY, like every runner: it emits the standard ResultRecord
// (experiment "truncprobe") with the raw answer, the page id as the join token, and
// the deepest depth actually quoted + the size carried in extra. The cut READING is
// derived from those; the runner does not declare a policy.
//
// The depth page needs the server secret (the codes are HMACs), and the runner needs
// the SAME secret to recompute the codes and read which were returned; it must
// match the server's.

// The scan asks for EVERY code: the measurement is how far down the model read, so
// it is an approved exception to the standard question.
const truncScanQuestion = "This page has several marker codes that begin with TRUNC- at different points from top to bottom. Quote every TRUNC- code you can see on the page, exactly as written."

// truncLadder is the page-size ladder, octave-spaced 64 KiB .. 4 MiB (matches the
// size sweep's range), so the cut can be located whether it sits low or high.
func truncLadder() []int {
	const KiB, MiB = 1 << 10, 1 << 20
	return []int{64 * KiB, 128 * KiB, 256 * KiB, 512 * KiB, 1 * MiB, 2 * MiB, 4 * MiB}
}

// parseSizes turns an explicit comma list of sizes into bytes. Each token is a
// plain byte count or carries a k/m suffix (KiB/MiB): "2k" = 2048, "4m" = 4 MiB.
// An explicit list is cleaner than capping a fixed ladder, and lets a run target
// the small end (e.g. measuring xAI's web_search extract window).
func parseSizes(s string) ([]int, error) {
	var out []int
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(strings.ToLower(tok))
		if tok == "" {
			continue
		}
		mult := 1
		if strings.HasSuffix(tok, "k") {
			mult, tok = 1<<10, strings.TrimSuffix(tok, "k")
		} else if strings.HasSuffix(tok, "m") {
			mult, tok = 1<<20, strings.TrimSuffix(tok, "m")
		}
		n, err := strconv.Atoi(strings.TrimSpace(tok))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("bad size %q in -readDepth-sizes", tok)
		}
		out = append(out, n*mult)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-readDepth-sizes is empty")
	}
	return out, nil
}

// capLadder drops rungs above maxBytes, so a run can skip the expensive multi-MB
// pages (fetched content is billed as input tokens — the big rungs drive the cost).
// A zero/negative cap leaves the ladder whole. It never returns empty: if the cap
// is below every rung, it keeps just the smallest, so a run still has something.
// Assumes sizes ascending (truncLadder is).
func capLadder(sizes []int, maxBytes int) []int {
	if maxBytes <= 0 {
		return sizes
	}
	var out []int
	for _, s := range sizes {
		if s <= maxBytes {
			out = append(out, s)
		}
	}
	if len(out) == 0 && len(sizes) > 0 {
		out = sizes[:1]
	}
	return out
}

// TruncAction is one scheduled ask about one depth page. Part A is a single "scan";
// Part B is a "warm" (top marker) and a "reask" (bottom marker) sharing a PageID.
type TruncAction struct {
	ID        string
	Provider  string
	Model     string
	PageID    string
	URL       string
	Bytes     int
	Part      string // "A" (locate the cut) or "B" (is the stored copy cut)
	Step      string // "scan" | "warm" | "reask"
	Seq       int    // 0 warm/scan, 1 reask
	TargetPct int    // depth this ask targets; -1 for a Part A scan (all markers)
	Question  string

	FiredUnix    float64
	ElapsedS     float64
	Answer       string
	Err          string
	DeepestDepth int // deepest depth quoted, from the answer; -1 if none
	Fetches      int
	Usage        probe.Usage // token cost of the call
}

// buildTruncActions is the pure plan (no network): Part A one scan per
// (provider, size); Part B a warm+reask pair per (provider, size) on its OWN page.
// The size is in the path and the page id is unique per (provider, run, part, size).
// routePrefix is the generated-page route to hit (default "/depth/html/"); the format
// variants (/depth/txt/, /depth/pdf/, /depth/docx/) serve the same markers
// in another format. partAOnly builds only the "scan" (cut-locating) ask and skips
// the Part B warm+reask — the cheap read-depth smoke that avoids Part B's cost.
func buildTruncActions(provs []Caller, base, runID string, sizes []int, reps int, routePrefix string) []*TruncAction {
	if reps < 1 {
		reps = 1
	}
	if routePrefix == "" {
		routePrefix = "/depth/html/"
	}
	mkURL := func(id string, n int) string {
		return strings.TrimRight(base, "/") + routePrefix + id + "/" + strconv.Itoa(n)
	}
	var out []*TruncAction
	for _, p := range provs {
		for _, n := range sizes {
			for r := 1; r <= reps; r++ {
				// Each rep gets fresh, independent pages so a noisy tool is sampled.
				aID := fmt.Sprintf("%s-%s-a-%s-r%d", p.Tag(), runID, sizeLabel(n), r)
				out = append(out, &TruncAction{
					ID: aID + "#0", Provider: p.Name, Model: p.Model, PageID: aID, URL: mkURL(aID, n),
					Bytes: n, Part: "A", Step: "scan", Seq: 0, TargetPct: -1, Question: truncScanQuestion,
				})
			}
		}
	}
	return out
}

// truncEmit writes one fired action's standard record. The deepest depth quoted is
// recomputed from the stored answer (a fact, not a verdict) and carried in extra
// with the size and the part; the page id is the token analysis joins on.
func truncEmit(runID string, a *TruncAction, secret string, force bool) {
	deepest := probe.DeepestDepth(a.Answer, secret, a.PageID, probe.DepthPageRevision)
	m := resultMeta{
		RunID: runID, Experiment: "truncprobe",
		EpisodeID: runID + "-" + a.PageID, Step: a.Step,
		Surface: "api", Vendor: a.Provider, Model: a.Model, ToolForced: force, Marker: probe.DepthMarkerPrefix,
	}
	extra := map[string]any{
		"size_bytes":       float64(a.Bytes),
		"part":             a.Part,
		"deepest_depth":    float64(deepest),
		"seq":              float64(a.Seq),
		"fetches_snapshot": float64(a.Fetches),
	}
	if a.TargetPct >= 0 {
		extra["target_pct"] = float64(a.TargetPct)
	}
	// Self-identify the dynamic-filtering A/B cells: record the pinned Anthropic web_fetch
	// version when one is set (A = web_fetch_20250910 no-DF, B = web_fetch_20260209 DF).
	// ONLY on the DIRECT anthropic caller — the override does not affect the OpenRouter
	// path (C), so labelling it there would be wrong. See docs/experiments/dynFilter.md.
	if anthropicFetchVersionOverride != nil && a.Provider == "anthropic" {
		extra["web_fetch_version"] = *anthropicFetchVersionOverride
	}
	if deepest >= 0 {
		extra["deepest_offset"] = float64(probe.DepthOffset(deepest, a.Bytes))
	}
	rec := resultFromAsk(m, a.ID, askOutcome{
		URL: a.URL, Token: a.PageID, Question: a.Question, Answer: a.Answer, ErrStr: a.Err,
		MarkerPresent: deepest >= 0, SentUnix: a.FiredUnix, ElapsedS: a.ElapsedS, FetchesSnapshot: a.Fetches,
		InputTokens: a.Usage.InputTokens, OutputTokens: a.Usage.OutputTokens, Rounds: a.Usage.Rounds,
	}, extra)
	if err := emitResults(m.Experiment, runID, []probe.ResultRecord{rec}); err != nil {
		fmt.Fprintf(os.Stderr, "  result record write failed (truncprobe %s): %v\n", a.ID, err)
	}
}

// runTruncProbe is the live path: one scan per page, then the report.
func runTruncProbe(base string, provs []Caller, secret string, maxBytes int, sizesOverride []int, reps int, routePrefix string, force, dryRun bool) int {
	if base == "" {
		fmt.Fprintln(os.Stderr, "error: -siteUrl is required (origin base)")
		return 1
	}
	if probeLogPath == "" {
		fmt.Fprintln(os.Stderr, "error: -serverLog is required; the result is read from what reaches our server")
		return 1
	}
	host := hostOfURL(base)
	if host == "" {
		fmt.Fprintf(os.Stderr, "error: -siteUrl %q has no host\n", base)
		return 1
	}
	runID := runNonce()
	// An explicit -readDepth-sizes list wins; otherwise the default ladder, capped.
	sizes := capLadder(truncLadder(), maxBytes)
	if len(sizesOverride) > 0 {
		sizes = sizesOverride
	}
	acts := buildTruncActions(provs, base, runID, sizes, reps, routePrefix)

	provNames := make([]string, 0, len(provs))
	for _, p := range provs {
		provNames = append(provNames, p.Tag())
	}
	fmt.Printf("[readDepth] run %s: providers [%s], %d sizes\n",
		runID, strings.Join(provNames, ", "), len(sizes))

	if dryRun {
		fmt.Println("[readDepth] DRY RUN — nothing asked. Sizes:")
		for _, n := range sizes {
			fmt.Printf("  %-8s  one scan per vendor and repeat\n", sizeLabel(n))
		}
		return 0
	}

	// A live run needs the secret to recompute the codes and read which were returned.
	if secret == "" {
		fmt.Fprintln(os.Stderr, "error: -pageCodeSecret (PROBE_PAGE_CODE_SECRET) is required; the depth page codes are HMACs and the runner must recompute them")
		return 1
	}

	byName := map[string]Caller{}
	for _, p := range provs {
		byName[p.Name] = p
	}

	fmt.Printf("[readDepth] about to make ~%d API calls across %d provider(s) — Ctrl-C within 5s to abort\n", len(acts), len(provs))
	time.Sleep(5 * time.Second)

	fmt.Println("[readDepth] locating the cut (scan each size) …")
	for _, a := range acts {
		fireTruncAction(a, byName[a.Provider], secret, force)
		truncEmit(runID, a, secret, force)
		fmt.Printf("  %-8s %-13s deepest marker: %s\n", sizeLabel(a.Bytes), a.Provider, depthLabel(a.DeepestDepth))
	}

	return reportTrunc(acts)
}

// fireTruncAction asks the provider and reads the answer, the deepest marker quoted,
// and whether a fetch reached our origin.
func fireTruncAction(a *TruncAction, p Caller, secret string, force bool) {
	sent := time.Now().UTC()
	a.FiredUnix = unixf(sent)
	// 15 min, not 5: a server-tool turn that pauses is resumed over several POSTs,
	// so the whole ask can take longer than one request (see askVendor's pause loop
	// and anthropicVendor.Continue).
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	ans, usage, err := p.Ask(ctx, os.Getenv(p.EnvKey), p.Model, a.URL, a.Question, force)
	cancel()
	a.Usage = usage
	a.ElapsedS = time.Since(sent).Seconds()
	// Keep the answer generously long: a scan may quote several codes, and cutting
	// it short could drop a deep code and misread the cut.
	a.Answer = logAnswer(ans, usage, 2000)
	a.DeepestDepth = probe.DeepestDepth(a.Answer, secret, a.PageID, probe.DepthPageRevision)
	warnIfAnomalous(fmt.Sprintf("truncprobe %s [%s] %dB", a.Part, a.Provider, a.Bytes), usage)
	if err != nil {
		a.Err = err.Error()
	}
	time.Sleep(20 * time.Second) // let a fetch land
	recs, _, _ := probe.LoadRecords(probeLogPath, "", time.Time{})
	a.Fetches = countHits(recs, a.PageID, hostOfURL(a.URL), a.FiredUnix, 150)
	// WATCH (pause_turn): one task should fetch
	// once. A resumed turn should reuse the round-1 fetch, not re-fetch — more than
	// one hit means a genuine within-call duplicate fetch, not caching.
	if a.Fetches > 1 {
		fmt.Fprintf(os.Stderr, "  [watch] %s logged %d fetches (want 1) — a resumed turn may have re-fetched; not a cache reading\n", a.PageID, a.Fetches)
	}
}

// reportTrunc prints the cut per size.
// Shape only; name the mechanism, do not over-claim.
func reportTrunc(acts []*TruncAction) int {
	fmt.Println("\n" + strings.Repeat("=", 74))
	fmt.Println("  READ DEPTH — how far down the page does each fetcher read?")
	fmt.Println(strings.Repeat("=", 74))
	fmt.Println("  Deepest marker quoted = the cut for a live fetch.")
	fmt.Println()

	sort.SliceStable(acts, func(i, j int) bool {
		if acts[i].Bytes != acts[j].Bytes {
			return acts[i].Bytes < acts[j].Bytes
		}
		return acts[i].ID < acts[j].ID
	})
	fmt.Println("  Cut per size:")
	for _, a := range acts {
		if a.Part != "A" {
			continue
		}
		fmt.Printf("    %-8s %-13s deepest=%s (fetches=%d)\n", sizeLabel(a.Bytes), a.Provider, depthLabel(a.DeepestDepth), a.Fetches)
	}
	return 0
}

// depthLabel renders a depth reading: a percent, or "none" when nothing was quoted.
func depthLabel(pct int) string {
	if pct < 0 {
		return "none"
	}
	return strconv.Itoa(pct) + "%"
}
