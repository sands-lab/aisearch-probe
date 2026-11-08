// probe-trial: drives AI assistants at a page on a schedule, and records
// exactly when each request was sent.
//
// The point is correlation. The probe log says a fetch arrived; this log says a
// query was submitted three seconds earlier. Together they distinguish a fetch
// caused by your query from a background crawl that happened to land nearby.
//
// It also separates two different caches that look identical in server logs.
// Sending the SAME question twice cannot tell you whether the assistant kept
// your page or merely replayed its own previous answer. So the schedule rotates
// through DIFFERENT questions about the same page: a replayed answer cannot
// survive a changed question, but a retained page can.
//
// Build:
//
//	go build -o bin/probe-trial ./trial
//
// Usage:
//
//	export PERPLEXITY_API_KEY=...          # any subset; missing keys are skipped
//	./bin/probe-trial -siteUrl https://example.com/method.html
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aisearch-probe/internal/probe"
)

// defaultAnthropicModel is what a direct Anthropic run gets without
// -anthropicModel. Sonnet 5, never Haiku: Haiku cannot fetch a page (BULLETIN
// 2026-10-11, DECISIONS D9), so a Haiku default silently voided Anthropic results.
const defaultAnthropicModel = "claude-sonnet-5"

func main() {
	var (
		urlFlag     = flag.String("siteUrl", "", "Page URL to ask about (required)")
		dryRunFlag  = flag.Bool("dryRun", false, "Print the plan and exit without calling any API")
		envFileFlag = flag.String("keyFile", "~/.config/probe-trial/env",
			"Credentials file to load (KEY=value lines). Read by this tool directly, because `source` on such a file does not export to child processes.")
		allModelsFlag = flag.Bool("listModelsAll", false,
			"With -listModels, probe every candidate instead of stopping at the cheapest usable one")
		listModelsFlag = flag.Bool("listModels", false,
			"Ask each provider what models this account can see, and probe which cheap ones are actually callable right now")
		checkFlag = flag.Bool("checkVendors", false, "Verify every provider is usable against -siteUrl, then exit")

		pageCheck = flag.Bool("checkPage", false,
			"Fetch a fresh 2 KB depth page from -siteUrl and report whether it is reachable and carries the code -pageCodeSecret predicts, then exit. Makes no API call")
		resolveFlag = flag.String("resolve", "",
			"host:ip to use for OUR OWN probe address, like curl --resolve. Only needed where DNS-over-HTTPS is blocked too; the driver falls back to it automatically")
		sharingTest = flag.Bool("sharingApi-run", false,
			"Run the sharing experiment: does one caller's download serve another caller's question? -siteUrl is the origin; each run makes two fresh 2 KB /depth/html/ pages, matched on their own codes (needs -pageCodeSecret). Both callers get the IDENTICAL address — never use for retention")
		sharingBaseline = flag.Bool("sharingApi-run-sameCaller", false,
			"Run the sharing test with one caller asked twice (the reference): -sharingApi-firstCaller and -sharingApi-secondCaller name the SAME caller")
		sharingFirst = flag.String("sharingApi-firstCaller", "openai",
			"Sharing experiment: the account that asks first and causes the download")
		sharingSecond = flag.String("sharingApi-secondCaller", "or:openai",
			"Sharing experiment: the unrelated account that asks second")
		sharingFirstUser = flag.String("sharingApi-firstUser", "",
			"Per-user isolation test: an end-user id sent (via OpenRouter's user field) on the FIRST caller's requests. With both -sharingApi-firstCaller/-second the SAME or:vendor route, this simulates two end-users of one account")
		sharingSecondUser = flag.String("sharingApi-secondUser", "",
			"Per-user isolation test: the end-user id for the SECOND caller. Different from -sharingApi-firstUser tests whether the provider isolates its cache per end-user")
		sharingGap = flag.Duration("sharingApi-secondAskGap", 5*time.Minute,
			"Sharing experiment: pause between the first caller's warm and the second caller's ask — well inside every vendor's copy lifetime (OpenAI's is the shortest, ~30 min)")
		sharingControlGap = flag.Duration("sharingApi-controlAskGap", 0,
			"Sharing experiment: pause before the control step (step 3); 0 = straight after step 2, as before. For Gemini's free tier, which answered 503 to a call 20 s after the previous one")
		sharingLongGap = flag.Duration("sharingApi-lastAskGap", 10*time.Minute,
			"Sharing experiment: pause before the final step, long enough to rule out both requests landing on one warm machine")
		sharingOut = flag.String("sharingApi-outFile", "logs/sharing.jsonl",
			"Sharing experiment: where to write the steps")
		pageCodeSecret = flag.String("pageCodeSecret", os.Getenv("PROBE_PAGE_CODE_SECRET"),
			"Secret to recompute the codes printed on depth pages; must match the server's -pageCodeSecret. Never sent in a URL or a prompt. Defaults to $PROBE_PAGE_CODE_SECRET")
		controlSecretFlag = flag.String("controlSecret", os.Getenv("PROBE_CONTROL_SECRET"),
			"Password for the server's page controls (correction, heal); must match the server's -controlSecret. Defaults to $PROBE_CONTROL_SECRET")
		onlyFlag = flag.String("vendors", "",
			"Comma-separated provider names to keep from the active set (e.g. \"or:anthropic\" or \"google\"); empty = all with a key. Lets one provider be run in isolation without touching the credentials file")
		correctionPlan = flag.Bool("correction-plan", false,
			"Correction run: write the state file for fresh changeable pages (/correction/html/, /correction/txt/), one per vendor x format x trial. Calls NO API. Needs -siteUrl (origin base) and -correction-askAgainTimes; then run -correction-loop or -correction-step")
		correctionTick = flag.Bool("correction-step", false,
			"Correction run: fire every step now due (set A + warm-up, switch to B after the settle time, ask-agains) and exit. For cron")
		correctionLoop = flag.Bool("correction-loop", false,
			"Correction run: like -correction-step, but keep ticking every minute until every page is done or void. For tmux")
		correctionReport = flag.Bool("correction-report", false,
			"Correction run: print each page's timeline and the outcome counts (needs -serverLog), then exit")
		correctionState = flag.String("correction-stateFile", "logs/correction.state.json",
			"Correction run: path to the plan/state file")
		correctionFormats = flag.String("correction-formats", "html,txt",
			"Correction run: which formats of the changeable page (html, txt)")
		correctionTrials = flag.Int("correction-repeats", 1,
			"Correction run: independent pages per vendor, format and check time IN THIS SITTING. Repeats (n>=3) normally come from separate sittings on different days/times, each its own -correction-stateFile")
		correctionWarmGap = flag.Duration("correction-firstAskGap", 90*time.Second,
			"Correction run: one warm-up per slot, this far apart, in shuffled vendor order (asks go out one at a time; the gap lets a slow ask finish so warm-ups never queue). 0 = all at once")
		correctionStartAt = flag.String("correction-startAt", "",
			"Correction run: when the first warm-up slot is, with its zone, e.g. 2026-10-15T10:00+03:00 (empty = now). Lets a sitting be planned and its loop started now, waiting until then")
		correctionJitter = flag.Bool("correction-jitter", true,
			"Correction run: draw each check's wait at random inside its own step (between the geometric midpoints with its neighbours), so checks never fall into step with a vendor's cycle")
		correctionSize = flag.Int("correction-pageSize", 2048,
			"Correction run: page size in bytes")
		correctionSettle = flag.Duration("correction-switchDelay", 5*time.Minute,
			"Correction run: wait this long after the warm-up answer comes back before switching the page to version 2 (a timer, never a count of fetches)")
		correctionReask = flag.String("correction-askAgainTimes", "",
			"Correction run: ask-again offsets after the switch, per vendor: \"or:openai=20m,40m;anthropic=12h,24h;*=...\". No default — every vendor must be covered")
		correctionLocalOrigin = flag.String("correction-controlUrl", "http://localhost:8899",
			"Correction run: the origin's LOCAL address, where the version control is called (bypasses Cloudflare). Providers still fetch the public -siteUrl")
		healPlan = flag.Bool("heal-plan", false,
			"Remembered-failure run: plan a sitting (no API call) — one fresh /heal/ per vendor x check time x failure, written to -heal-stateFile. Needs -siteUrl, -pageCodeSecret, -controlSecret")
		healTick = flag.Bool("heal-step", false,
			"Remembered-failure run: fire whatever is due once (create broken + first ask, fix 5 min after the answer, re-ask at its check)")
		healLoop = flag.Bool("heal-loop", false,
			"Remembered-failure run: like -heal-step, sleeping until the next step is due (the site is checked only then), exiting once every page is re-asked (run in tmux)")
		healReport = flag.Bool("heal-report", false,
			"Remembered-failure run: print every page (none dropped) as fetch yes/no x code yes/no, with notes. Needs -serverLog")
		healReportJSON = flag.String("heal-reportFile", "",
			"Remembered-failure run: with -heal-report, also write every page's result as JSON to this file (input for scripts/heal_plot.py)")
		healState = flag.String("heal-stateFile", "logs/heal.state.json",
			"Remembered-failure run: the sitting's state file (one per sitting; its lock sits next to it)")
		healChecks = flag.String("heal-askAgainTimes", defaultHealChecks,
			"Remembered-failure run: check times after the fix (a times-2 grid; each drawn inside its own step)")
		healFails = flag.String("heal-failCodes", "503,404,403",
			"Remembered-failure run: the failures each check gets a page for (the origin serves 503, 404, 403)")
		healTrials = flag.Int("heal-repeats", 1,
			"Remembered-failure run: pages per vendor x check time x failure in this sitting")
		healSettle = flag.Duration("heal-fixDelay", 5*time.Minute,
			"Remembered-failure run: wait after the first answer came back before fixing the page (a timer, never a fetch count)")
		healWarmGap = flag.Duration("heal-firstAskGap", 90*time.Second,
			"Remembered-failure run: one first ask per slot, this far apart, in shuffled order")
		healStartAt = flag.String("heal-startAt", "",
			"Remembered-failure run: first slot, with its zone (e.g. 2026-10-23T10:00+03:00); empty = now")
		healJitter = flag.Bool("heal-jitter", true,
			"Remembered-failure run: draw each check's wait at random inside its step")
		healSize = flag.Int("heal-pageSize", 2048,
			"Remembered-failure run: byte size of the fixed page (the 2 KB depth page)")
		healLocalOrigin = flag.String("heal-controlUrl", "http://localhost:8899",
			"Remembered-failure run: the origin's LOCAL address, where the heal control is called (bypasses Cloudflare). Providers still fetch the public -siteUrl")
		healFetchVersion = flag.String("heal-anthropicFetchTool", defaultHealFetchVersion,
			"Remembered-failure run: the Anthropic web fetch tool, pinned in the state at planning (default: without dynamic filtering)")
		ttlmapReplicates = flag.Int("cacheHeaders-repeats", 8,
			"Cache-header test: fresh pages per vendor and setting")
		ttlmapPollGaps = flag.String("cacheHeaders-askAgainTimes", "2m30s,5m,10m,20m,40m,80m",
			"Cache-header test: when to ask again after the first ask, e.g. \"2m30s,5m,10m,20m,40m,80m\". Each gap >= 150s, strictly ascending")
		ttlmapArms = flag.String("cacheHeaders-settings", "",
			"Cache-header test: comma-separated setting labels to run (e.g. \"cc-nostore-tag,cc-private-tag\"); empty = all 19. An unknown label is an error")
		ttlmapKeep = flag.Bool("cacheHeaders-askAfterRefetch", false,
			"Cache-header test: keep asking a page at every gap even after the vendor downloads it again (default: stop at the first download-again)")
		ttlmapCcdepth = flag.Bool("cacheHeaders-run", false,
			"Run the cache-header test: 19 pages on /cacheHeaders/ (the depth HTML page at -cacheHeaders-pageSize), one per caching-header setting with and without version info, each graded against what RFC 9111 allows a shared cache for the headers sent. Needs -siteUrl, -serverLog and -pageCodeSecret")
		ttlmapSize = flag.Int("cacheHeaders-pageSize", 2048,
			"Cache-header test: page size in bytes; small on purpose, a kept page is billed as input tokens on every ask")
		truncprobe = flag.Bool("readDepth-run", false,
			"Read-depth test: ask about long pages with unforgeable codes at known depths and read the deepest one each model quotes, which is where its fetcher cut the page. Also runs the document-type, dynamic-filtering and JavaScript tests on their own pages (-readDepth-page). Needs -siteUrl, -serverLog and -pageCodeSecret")
		truncprobeMax = flag.Int("readDepth-maxSize", 0,
			"Read-depth test: largest page size in bytes to test; rungs above it are dropped (0 = no cap, full 64KiB..4MiB ladder). The multi-MB rungs drive nearly all the cost, so cap this (e.g. 2097152 for 2 MiB) to keep a run cheap. If a fetcher reads past the cap the result is just \">cap\".")
		truncprobeSizes = flag.String("readDepth-sizes", "",
			"Read-depth test: exact page sizes to test instead of the default ladder, e.g. \"2k,4k,8k,16k,32k,64k\" (k=KiB, m=MiB). Use for the small-doc end (measuring a search extract window). Overrides -readDepth-maxSize. Smallest usable size is ~2k (5 markers must fit).")
		truncprobeRoute = flag.String("readDepth-page", "/depth/html/",
			"Read-depth test: which depth page kind to ask about, e.g. /depth/html/ (default), /depth/pdf/, /depth/jsInline/")
		truncprobeReps = flag.Int("readDepth-repeats", 1,
			"Read-depth test: independent pages per (provider, size), so a noisy tool (e.g. OpenAI web_search) is sampled n=reps instead of once. Each rep is a fresh page.")
		faults = flag.Bool("fault-run", false,
			"Run the failure-retry experiment: ask each provider about the /fault/ paths and count, from -serverLog, how many times its fetcher retried each failure. Needs -siteUrl, -serverLog and -pageCodeSecret (the content-ending faults carry depth markers)")
		faultsWindow = flag.Duration("fault-retryWait", 90*time.Second,
			"How long to watch our log after each fault question. Must exceed the slow fault (~25s) and the Retry-After delay (10s) so a late retry is not missed")
		faultsOut = flag.String("fault-outFile", "logs/faults.jsonl",
			"Failure-retry experiment: where to write per-provider-per-fault results")
		faultsOnly = flag.String("fault-kinds", "",
			"Failure-retry experiment: comma-separated fault names to ask about instead of the usual battery (e.g. \"flaky/0\" = a page that works at once, the success baseline). Empty = all")
		faultsRunID = flag.String("fault-runTag", "",
			"Per-run tag folded into every fault URL so a repeat run gets fresh addresses (no cache collision, flaky counter resets). Empty = auto (UTC datetime + random)")
		cacheFile = flag.String("checkRecordFile", "logs/route-cache.json",
			"Where preflight route validations are remembered, so a re-run does not pay for the same check again. TRIALS ARE NEVER CACHED.")
		cacheTTL   = flag.Duration("checkRecordAge", 6*time.Hour, "How long a passing route validation stays valid")
		freshCheck = flag.Bool("checkAgain", false, "Ignore remembered validations and re-test every route with a real call")
		probeLog   = flag.String("serverLog", "", "Probe log to read during -checkVendors, to confirm WHO fetched the page")
		forceFlag  = flag.Bool("forceFetch", true, "Require the provider to call its search/fetch tool instead of answering from memory")

		// Cheap by default, for the reason in openrouter.go: the task is easy and
		// the study needs many runs. Dashes here, not dots — this is Anthropic's
		// own API id style, unlike the OpenRouter slug.
		anthropicModel        = flag.String("anthropicModel", defaultAnthropicModel, "Anthropic model id")
		anthropicFetchVersion = flag.String("anthropicFetchTool", "", "Pin the Anthropic web_fetch tool version for the dynamic-filtering A/B (e.g. web_fetch_20250910 = no dynamic filtering, web_fetch_20260209 = has it). Empty = default (web_fetch_20260209). Investigation only.")
		rawOut                = flag.String("replyLog", "logs/raw-replies.jsonl", "Keep every vendor reply, whole, one JSON line per request (time, vendor address, model, page address, round). On by default so later analysis can take anything a runner did not keep (Anthropic's per-fetch error_code, OpenRouter annotations). Empty = off. Append-only")
		openaiModel           = flag.String("openaiModel", "gpt-5-mini", "OpenAI model id")
		googleModel           = flag.String("googleModel", "gemini-3.1-flash-lite", "Gemini model id (both the Developer API and Vertex AI)")
		// grok-4.3 by default to MATCH the OpenRouter x-ai route (x-ai/grok-4.3), so
		// the direct-vs-OpenRouter sharing test compares like with like, and so the
		// default is the model the real run uses — no "remember to switch it back".
		// The cheaper build models lack the web_search tool, so a check on one of
		// those cannot validate the search wiring; override deliberately if needed.
		xaiModel = flag.String("xaiModel", "grok-4.3", "xAI (Grok) model id for the direct x-ai route")
		orModels = flag.String("openrouterModels", strings.Join(defaultOpenRouterModels, ","),
			"Comma-separated OpenRouter model slugs; one provider entry per slug, named or:<vendor>")

		// Web-app sharing helper: a person pastes the prompts into each chat app; this
		// mints the pages and reads the server log. Needs no provider keys.
		websharingFlag = flag.Bool("sharingWeb-run", false,
			"Web-app sharing run: does a page one ACCOUNT's chat downloaded get served to a DIFFERENT account's chat? Same pages, prompts and verdicts as -sharingApi-run; a person pastes. Needs the page-code secret. See trial/websharing.go")
		websharingApps = flag.String("sharingWeb-apps", "claude,chatgpt,gemini,grok,deepseek,mistral,qwen,metaai", "Web-app sharing: apps in this sitting, comma-separated")
		websharingA    = flag.String("sharingWeb-firstAccount", "account-a", "Web-app sharing: label for account A (reads first)")
		websharingB    = flag.String("sharingWeb-secondAccount", "account-b", "Web-app sharing: label for account B (asks second, and the control)")
		websharingGap  = flag.Duration("sharingWeb-secondAskGap", 5*time.Minute, "Web-app sharing: wait from account A's ask to account B's ask, per app")
	)
	flag.Parse()

	// Load the credentials file ourselves; an explicit export still wins.
	if names, err := loadEnvFile(*envFileFlag); err == nil && len(names) > 0 {
		fmt.Fprintf(os.Stderr, "[trial] loaded %d credential(s) from %s: %s\n",
			len(names), *envFileFlag, strings.Join(names, ", "))
	}

	// Backfill the page-code secret from the env AFTER the file is loaded: these
	// flags' os.Getenv defaults were read before loadEnvFile ran, so an
	// env-file-only secret would otherwise be empty. An explicit flag still wins.
	*pageCodeSecret = orEnv(*pageCodeSecret, "PROBE_PAGE_CODE_SECRET")
	// Same for the control secret (-correction-*, -heal-*): without this an
	// env-file-only PROBE_CONTROL_SECRET never reaches -controlSecret.
	*controlSecretFlag = orEnv(*controlSecretFlag, "PROBE_CONTROL_SECRET")

	// Discovery needs no URL and makes no measurement.
	if *listModelsFlag {
		probeAllModels = *allModelsFlag
		if len(credentialSummary()) == 0 {
			printNoCredentials(*envFileFlag)
			os.Exit(1)
		}
		runListModels()
		return
	}

	// -dryRun and -checkVendors are contradictory: -checkVendors's whole job is to make a real
	// API call to verify a route, so a "dry check" is meaningless. Surface the
	// contradiction instead of silently doing one or the other.
	if *dryRunFlag && *checkFlag {
		fmt.Fprintln(os.Stderr, "error: -checkVendors makes real API calls to verify routes; it cannot be combined with -dryRun. Drop one.")
		os.Exit(1)
	}

	// Set before every mode branch. It used to be assigned only inside the
	// -checkVendors branch, which runs last, so the sharing experiment saw an empty
	// path, could not check that its pages were untouched, and spent two API
	// calls on a run that could conclude nothing.
	probeLogPath = *probeLog

	// Pin the Anthropic web_fetch version for the whole run (dynamic-filtering A/B).
	if *anthropicFetchVersion != "" {
		anthropicFetchVersionOverride = anthropicFetchVersion
	}
	rawOutPath = *rawOut

	if *resolveFlag != "" {
		if err := addResolveOverride(*resolveFlag); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}

	// A diagnostic that makes no API call: is our own server reachable from here,
	// and does a fresh depth page carry the code this secret predicts?
	if *pageCheck {
		pageURL, code, err := checkPageTarget(*urlFlag, *pageCodeSecret)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		l := checkPageLive(context.Background(), pageURL, code)
		fmt.Printf("%s\n  %s\n", pageURL, l)
		if l.OK {
			os.Exit(0)
		}
		os.Exit(1)
	}

	// The web-app sharing helper needs no provider keys and no run lock: a person
	// pastes the prompts, and it only mints addresses and watches the server log.
	if *websharingFlag {
		apps, err := parseWebApps(*websharingApps)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[websharing] %v\n", err)
			os.Exit(1)
		}
		probeLogPath = *probeLog
		os.Exit(runWebSharing(webSharingConfig{
			base: strings.TrimRight(*urlFlag, "/"), secret: *pageCodeSecret, apps: apps,
			acctA: *websharingA, acctB: *websharingB, gap: *websharingGap,
			webDir: "logs/sharingWeb", outDir: "logs/sharingWeb",
		}))
	}

	if *urlFlag == "" && !(*correctionTick || *correctionLoop || *correctionReport) {
		// The correction run's tick/loop/report read the origin from their state file.
		fmt.Fprintln(os.Stderr, "error: -siteUrl is required")
		flag.Usage()
		os.Exit(1)
	}

	var orList []string
	for _, m := range strings.Split(*orModels, ",") {
		if m = strings.TrimSpace(m); m != "" {
			orList = append(orList, m)
		}
	}
	provs := callers(*anthropicModel, *openaiModel, *googleModel, *xaiModel, orList)
	var active []Caller
	for _, p := range provs {
		if p.Enabled {
			active = append(active, p)
		} else if os.Getenv(p.EnvKey) == "" {
			what := "is not set"
			if p.IsFile {
				what = "is not set (it should hold the path to a service-account JSON file)"
			}
			fmt.Fprintf(os.Stderr, "[trial] skipping %s — %s %s\n", p.Name, p.EnvKey, what)
		}
	}
	if len(active) == 0 {
		fmt.Fprintln(os.Stderr, "error: no provider API keys found in the environment")
		os.Exit(1)
	}

	// -vendors keeps just the named providers, so one can be run in isolation (a
	// single-provider repeat, or a leg that needs different timing) without
	// editing the credentials file to hide the others.
	if keep := strings.TrimSpace(*onlyFlag); keep != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(keep, ",") {
			if n = strings.TrimSpace(n); n != "" {
				want[n] = true
			}
		}
		var filtered []Caller
		for _, p := range active {
			if want[p.Name] {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) == 0 {
			fmt.Fprintf(os.Stderr, "error: -vendors %q matched none of the active providers\n", keep)
			os.Exit(1)
		}
		active = filtered
	}
	warnGoogle(active)

	// BACKSTOP: under -dryRun, no code path may spend money — no matter which
	// experiment branch forgot to implement its own planner. Money is only ever
	// spent inside Caller.Ask, so replacing every active provider's Ask with a
	// refusal here makes a paid call under -dryRun structurally impossible. This
	// is defense in depth behind the per-mode dry-run handling, not a substitute
	// for it: a mode that prints its plan and exits still should. It exists
	// because a real fault run was launched by accident when a "dry" invocation
	// slipped past a branch that never checked the flag (2026-09-15).
	if *dryRunFlag {
		refuseUnderDryRun(active)
	}

	if *correctionReport {
		st, rerr := loadCorrectionState(*correctionState)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", rerr)
			os.Exit(1)
		}
		if probeLogPath == "" {
			fmt.Fprintln(os.Stderr, "error: -serverLog is required; which version each fetch got is read from it")
			os.Exit(1)
		}
		recs, _, lerr := probe.LoadRecords(probeLogPath, "", time.Time{})
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", lerr)
			os.Exit(1)
		}
		os.Exit(reportCorrection(st, recs))
	}

	if *correctionPlan {
		var formats []string
		for _, f := range strings.Split(*correctionFormats, ",") {
			if f = strings.TrimSpace(f); f != "" {
				formats = append(formats, f)
			}
		}
		os.Exit(planCorrection(*urlFlag, *correctionLocalOrigin, active, formats, *correctionTrials, *correctionSize,
			*correctionSettle, *correctionWarmGap, *correctionStartAt, *correctionJitter, *correctionReask, *forceFlag,
			*correctionState, *pageCodeSecret, *controlSecretFlag, *dryRunFlag))
	}

	if *healReport {
		st, rerr := loadHealState(*healState)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", rerr)
			os.Exit(1)
		}
		if probeLogPath == "" {
			fmt.Fprintln(os.Stderr, "error: -serverLog is required; what each request got is read from it")
			os.Exit(1)
		}
		recs, _, lerr := probe.LoadRecords(probeLogPath, "", time.Time{})
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", lerr)
			os.Exit(1)
		}
		if *healReportJSON != "" {
			if werr := writeHealResults(*healReportJSON, st, recs, *pageCodeSecret); werr != nil {
				fmt.Fprintf(os.Stderr, "error: -heal-reportFile: %v\n", werr)
				os.Exit(1)
			}
		}
		os.Exit(reportHeal(st, recs, *pageCodeSecret))
	}

	if *healPlan {
		os.Exit(planHeal(healPlanArgs{
			Base: *urlFlag, LocalOrigin: *healLocalOrigin, Provs: active,
			Checks: *healChecks, Fails: *healFails, Trials: *healTrials, Size: *healSize,
			Settle: *healSettle, WarmGap: *healWarmGap, StartAt: *healStartAt, Jitter: *healJitter, Force: *forceFlag,
			StatePath: *healState, Secret: *pageCodeSecret, ControlSecret: *controlSecretFlag,
			FetchVersion: *healFetchVersion, DryRun: *dryRunFlag,
		}))
	}

	if *healTick || *healLoop {
		if *dryRunFlag {
			fmt.Fprintln(os.Stderr, "error: -dryRun applies to -heal-plan; a tick only fires real steps")
			os.Exit(1)
		}
		os.Exit(runHealTick(active, *healState, *pageCodeSecret, *controlSecretFlag, *healLoop))
	}

	if *correctionTick || *correctionLoop {
		if *dryRunFlag {
			fmt.Fprintln(os.Stderr, "error: -dryRun applies to -correction-plan; a tick only fires real steps")
			os.Exit(1)
		}
		os.Exit(runCorrectionTick(active, *correctionState, *pageCodeSecret, *controlSecretFlag, *correctionLoop))
	}

	if *ttlmapCcdepth {
		if len(active) == 0 {
			fmt.Fprintln(os.Stderr, "error: no usable providers")
			os.Exit(1)
		}
		explicitGaps, gerr := parseDurations(*ttlmapPollGaps)
		if gerr != nil || len(explicitGaps) == 0 {
			fmt.Fprintf(os.Stderr, "error: -cacheHeaders-askAgainTimes: %v\n", gerr)
			os.Exit(1)
		}
		if *ttlmapReplicates < 1 {
			fmt.Fprintln(os.Stderr, "error: -cacheHeaders-repeats must be at least 1")
			os.Exit(1)
		}
		arms := ccDepthArms(*ttlmapSize)
		if sel, serr := selectArms(arms, *ttlmapArms); serr != nil {
			fmt.Fprintf(os.Stderr, "error: -cacheHeaders-settings: %v\n", serr)
			os.Exit(1)
		} else {
			arms = sel
		}
		ttlmapKeepAsking = *ttlmapKeep
		// Own lock, not the shared run lock: this touches only generated page
		// addresses. A dry run makes no address and no call, so it
		// needs no lock.
		if *dryRunFlag {
			os.Exit(runTTLMap(*urlFlag, active, arms, *ttlmapReplicates, 0, 0, 0, explicitGaps, *pageCodeSecret, "", *forceFlag, true))
		}
		lock, lerr := acquireRunLock(ttlmapLockPath, "cache-header test", []string{"generated /cacheHeaders/ addresses (own lock)"})
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", lerr)
			os.Exit(1)
		}
		code := runTTLMap(*urlFlag, active, arms, *ttlmapReplicates, 0, 0, 0, explicitGaps, *pageCodeSecret, "", *forceFlag, false)
		lock.Release()
		os.Exit(code)
	}

	if *truncprobe {
		if len(active) == 0 {
			fmt.Fprintln(os.Stderr, "error: no usable providers")
			os.Exit(1)
		}
		var sizesOverride []int
		if *truncprobeSizes != "" {
			var perr error
			sizesOverride, perr = parseSizes(*truncprobeSizes)
			if perr != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", perr)
				os.Exit(1)
			}
		}
		// Own lock, not the shared run lock: this touches only generated /depth/
		// addresses. A dry run needs no lock — it makes no address and no call.
		if *dryRunFlag {
			os.Exit(runTruncProbe(*urlFlag, active, *pageCodeSecret, *truncprobeMax, sizesOverride, *truncprobeReps, *truncprobeRoute, *forceFlag, true))
		}
		lock, lerr := acquireRunLock(truncLockPath, "read-depth test", []string{"generated /depth/ addresses (own lock)"})
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", lerr)
			os.Exit(1)
		}
		code := runTruncProbe(*urlFlag, active, *pageCodeSecret, *truncprobeMax, sizesOverride, *truncprobeReps, *truncprobeRoute, *forceFlag, false)
		lock.Release()
		os.Exit(code)
	}

	if *faults {
		if *faultsWindow < 45*time.Second {
			fmt.Fprintln(os.Stderr, "error: -fault-retryWait must be at least 45s; the slow fault alone holds ~25s")
			os.Exit(1)
		}
		if len(active) == 0 {
			fmt.Fprintln(os.Stderr, "error: no usable providers")
			os.Exit(1)
		}
		lock, lerr := acquireRunLock(runLockPath, "failure-retry experiment", []string{"site/ (measured pages)"})
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", lerr)
			os.Exit(1)
		}
		chosen, err := pickFaults(*faultsOnly)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		code := runFaults(*urlFlag, active, chosen, *faultsWindow, *forceFlag, *faultsOut, *faultsRunID, *pageCodeSecret)
		lock.Release()
		os.Exit(code)
	}

	// The provider check only needs -siteUrl. Build (and validate) the arms after
	// it, so a check does not demand -url-repeat.
	if *sharingTest || *sharingBaseline {
		byName := map[string]Caller{}
		for _, p := range active {
			byName[p.Name] = p
			byName[p.Tag()] = p // also address by vendor+model, so two models of one vendor (cross-model test) can be picked, e.g. or:openai-gpt-5-mini
		}
		a, okA := byName[*sharingFirst]
		b, okB := byName[*sharingSecond]
		if !okA || !okB {
			fmt.Fprintf(os.Stderr, "error: need both %q and %q usable; active accounts are: %s\n",
				*sharingFirst, *sharingSecond, providerNames(active))
			fmt.Fprintln(os.Stderr, "  Address a caller by route (openai, or:openai) or by Tag (or:openai-gpt-5-mini);")
			fmt.Fprintln(os.Stderr, "  a model not in the active list needs -openrouterModels / -<vendor>-model.")
			os.Exit(1)
		}
		if err := validateSharingPair(a, b, *sharingFirstUser, *sharingSecondUser, *sharingBaseline); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		// Its own lock per comparison: the pages are generated per run (none under
		// site/), so different comparisons can run side by side.
		lock, lerr := acquireRunLock(sharingLockPath(a, b, *sharingFirstUser, *sharingSecondUser), "sharing experiment",
			[]string{"generated /depth/html/sh-<run>/ addresses"})
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", lerr)
			os.Exit(1)
		}
		// Release explicitly: os.Exit does NOT run deferred calls, so a deferred
		// Release leaked the lock on every exit and blocked the next run. Found
		// 2026-09-03 when a refused sharing run left its lock behind.
		code := runSharing(*urlFlag, *pageCodeSecret, a, b,
			*sharingGap, *sharingLongGap, *sharingControlGap, *forceFlag, *sharingOut, *sharingFirstUser, *sharingSecondUser, *dryRunFlag)
		lock.Release()
		os.Exit(code)
	}

	if *checkFlag {
		if !*freshCheck {
			checkCache = openRouteCache(*cacheFile, *cacheTTL)
		} else {
			// Still open it so passes are recorded, but with a zero TTL nothing
			// is read back.
			checkCache = openRouteCache(*cacheFile, 0)
		}
		if runCheck(active, *urlFlag, *pageCodeSecret, *forceFlag) {
			return
		}
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "error: no experiment chosen; see -help for the run switches")
	flag.Usage()
	os.Exit(1)
}

// containsFold reports whether needle appears in haystack, ignoring case.
// An empty needle never matches — no marker means no evidence, not proof.
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// pathOf returns the path with any query stripped, matching how the probe
// derives page_key — so a cache-busting query does not break attribution.
func pathOf(rawURL string) string {
	i := strings.Index(rawURL, "://")
	if i < 0 {
		return rawURL
	}
	rest := rawURL[i+3:]
	path := "/"
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		path = rest[j:]
	}
	if k := strings.IndexByte(path, '?'); k >= 0 {
		path = path[:k]
	}
	return path
}

func providerNames(ps []Caller) string {
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}

func parseSchedule(s string) ([]time.Duration, error) {
	var out []time.Duration
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		d, err := time.ParseDuration(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not a duration (try 0s, 2m, 1h, 24h)", part)
		}
		if d < 0 {
			return nil, fmt.Errorf("%q is negative", part)
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no offsets given")
	}
	return out, nil
}

type trialLog struct {
	f *os.File
}

func newTrialLog(path string) (*trialLog, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &trialLog{f: f}, nil
}

// WriteAny appends any record as one JSON line.
// experiment, whose rows have a different shape and go to their own file — mixing
// them into trials.jsonl would give the correlation report half-populated Trial
// records to misread.
func (l *trialLog) WriteAny(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return l.f.Sync()
}

func (l *trialLog) Close() error { return l.f.Close() }

// refuseUnderDryRun replaces every provider's Ask with one that refuses to make a
// call. The money is only ever spent inside Ask, so this is the single choke
// point that makes a paid call under -dryRun impossible regardless of which
// experiment path runs. See the backstop note at the call site.
func refuseUnderDryRun(provs []Caller) {
	for i := range provs {
		provs[i].Ask = func(ctx context.Context, key, model, url, question string, force bool) (string, probe.Usage, error) {
			return "", probe.Usage{}, fmt.Errorf("dry-run: refused a real API call (%s)", url)
		}
	}
}
