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
//	./bin/probe-trial -url https://example.com/method.html
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
// -anthropic-model. Sonnet 5, never Haiku: Haiku cannot fetch a page (BULLETIN
// 2026-10-11, DECISIONS D9), so a Haiku default silently voided Anthropic results.
const defaultAnthropicModel = "claude-sonnet-5"

func main() {
	var (
		urlFlag     = flag.String("url", "", "Page URL to ask about (required)")
		markerFlag  = flag.String("marker", "RCP-2026-08", "String that only appears on the page; its presence in an answer proves a real fetch")
		dryRunFlag  = flag.Bool("dry-run", false, "Print the plan and exit without calling any API")
		envFileFlag = flag.String("env-file", "~/.config/probe-trial/env",
			"Credentials file to load (KEY=value lines). Read by this tool directly, because `source` on such a file does not export to child processes.")
		allModelsFlag = flag.Bool("all-models", false,
			"With -list-models, probe every candidate instead of stopping at the cheapest usable one")
		listModelsFlag = flag.Bool("list-models", false,
			"Ask each provider what models this account can see, and probe which cheap ones are actually callable right now")
		checkFlag = flag.Bool("check", false, "Verify every provider is usable against -url, then exit")

		pageCheck = flag.String("page-check", "",
			"Diagnostic: fetch this URL of OUR OWN page and report whether it is reachable and carries the page code, then exit. Makes no API call")
		resolveFlag = flag.String("resolve", "",
			"host:ip to use for OUR OWN probe address, like curl --resolve. Only needed where DNS-over-HTTPS is blocked too; the driver falls back to it automatically")
		sharingTest = flag.Bool("sharing-test", false,
			"Run the sharing experiment: does one caller's download serve another caller's question? -url is the origin; each run makes two fresh 2 KB /depthhtmlpage/ pages, matched on their own codes (needs -page-code-secret). Both callers get the IDENTICAL address — never use for retention")
		sharingBaseline = flag.Bool("sharing-baseline", false,
			"Sharing experiment: the within-account reference — -sharing-first and -sharing-second are the SAME caller, asked twice")
		sharingFirst = flag.String("sharing-first", "openai",
			"Sharing experiment: the account that asks first and causes the download")
		sharingSecond = flag.String("sharing-second", "or:openai",
			"Sharing experiment: the unrelated account that asks second")
		sharingFirstUser = flag.String("sharing-first-user", "",
			"Per-user isolation test: an end-user id sent (via OpenRouter's user field) on the FIRST caller's requests. With both -sharing-first/-second the SAME or:vendor route, this simulates two end-users of one account")
		sharingSecondUser = flag.String("sharing-second-user", "",
			"Per-user isolation test: the end-user id for the SECOND caller. Different from -sharing-first-user tests whether the provider isolates its cache per end-user")
		sharingGap = flag.Duration("sharing-gap", 5*time.Minute,
			"Sharing experiment: pause between the first caller's warm and the second caller's ask — well inside every vendor's copy lifetime (OpenAI's is the shortest, ~30 min)")
		sharingControlGap = flag.Duration("sharing-control-gap", 0,
			"Sharing experiment: pause before the control step (step 3); 0 = straight after step 2, as before. For Gemini's free tier, which answered 503 to a call 20 s after the previous one")
		sharingLongGap = flag.Duration("sharing-long-gap", 10*time.Minute,
			"Sharing experiment: pause before the final step, long enough to rule out both requests landing on one warm machine")
		sharingOut = flag.String("sharing-out", "logs/sharing.jsonl",
			"Sharing experiment: where to write the steps")
		pageCodeSecret = flag.String("page-code-secret", os.Getenv("PROBE_PAGE_CODE_SECRET"),
			"Secret to recompute /cpage/ codes (content-reuse experiment); must match the origin's -page-code-secret. Never sent in a URL or a prompt. Defaults to $PROBE_PAGE_CODE_SECRET")
		controlSecretFlag = flag.String("control-secret", os.Getenv("PROBE_CONTROL_SECRET"),
			"Secret for the server's robots control endpoint; must match the server's -robots-control-secret")
		onlyFlag = flag.String("only", "",
			"Comma-separated provider names to keep from the active set (e.g. \"or:anthropic\" or \"google\"); empty = all with a key. Lets one provider be run in isolation without touching the credentials file")
		correctionPlan = flag.Bool("correction-plan", false,
			"Correction run: write the state file for fresh changeable pages (/verhtmlpage/, /vertxtpage/), one per vendor x format x trial. Calls NO API. Needs -url (origin base) and -correction-reask; then run -correction-loop or -correction-tick")
		correctionTick = flag.Bool("correction-tick", false,
			"Correction run: fire every step now due (set A + warm-up, switch to B after the settle time, ask-agains) and exit. For cron")
		correctionLoop = flag.Bool("correction-loop", false,
			"Correction run: like -correction-tick, but keep ticking every minute until every page is done or void. For tmux")
		correctionReport = flag.Bool("correction-report", false,
			"Correction run: print each page's timeline and the outcome counts (needs -probe-log), then exit")
		correctionState = flag.String("correction-state", "logs/correction.state.json",
			"Correction run: path to the plan/state file")
		correctionFormats = flag.String("correction-formats", "html,txt",
			"Correction run: which formats of the changeable page (html, txt)")
		correctionTrials = flag.Int("correction-trials", 1,
			"Correction run: independent pages per vendor, format and check time IN THIS SITTING. Repeats (n>=3) normally come from separate sittings on different days/times, each its own -correction-state")
		correctionWarmGap = flag.Duration("correction-warm-gap", 90*time.Second,
			"Correction run: one warm-up per slot, this far apart, in shuffled vendor order (asks go out one at a time; the gap lets a slow ask finish so warm-ups never queue). 0 = all at once")
		correctionStartAt = flag.String("correction-start-at", "",
			"Correction run: when the first warm-up slot is, with its zone, e.g. 2026-10-15T10:00+03:00 (empty = now). Lets a sitting be planned and its loop started now, waiting until then")
		correctionJitter = flag.Bool("correction-jitter", true,
			"Correction run: draw each check's wait at random inside its own step (between the geometric midpoints with its neighbours), so checks never fall into step with a vendor's cycle")
		correctionSize = flag.Int("correction-size", 2048,
			"Correction run: page size in bytes")
		correctionSettle = flag.Duration("correction-settle", 5*time.Minute,
			"Correction run: wait this long after the warm-up answer comes back before switching the page to version 2 (a timer, never a count of fetches)")
		correctionReask = flag.String("correction-reask", "",
			"Correction run: ask-again offsets after the switch, per vendor: \"or:openai=20m,40m;anthropic=12h,24h;*=...\". No default — every vendor must be covered")
		correctionLocalOrigin = flag.String("correction-local-origin", "http://localhost:8899",
			"Correction run: the origin's LOCAL address, where the version control is called (bypasses Cloudflare). Providers still fetch the public -url")
		healPlan = flag.Bool("heal-plan", false,
			"Remembered-failure run: plan a sitting (no API call) — one fresh /healpage/ per vendor x check time x failure, written to -heal-state. Needs -url, -page-code-secret, -control-secret")
		healTick = flag.Bool("heal-tick", false,
			"Remembered-failure run: fire whatever is due once (create broken + first ask, fix 5 min after the answer, re-ask at its check)")
		healLoop = flag.Bool("heal-loop", false,
			"Remembered-failure run: like -heal-tick, sleeping until the next step is due (the site is checked only then), exiting once every page is re-asked (run in tmux)")
		healReport = flag.Bool("heal-report", false,
			"Remembered-failure run: print every page (none dropped) as fetch yes/no x code yes/no, with notes. Needs -probe-log")
		healReportJSON = flag.String("heal-report-json", "",
			"Remembered-failure run: with -heal-report, also write every page's result as JSON to this file (input for scripts/plot_heal.py)")
		healState = flag.String("heal-state", "logs/heal.state.json",
			"Remembered-failure run: the sitting's state file (one per sitting; its lock sits next to it)")
		healChecks = flag.String("heal-checks", defaultHealChecks,
			"Remembered-failure run: check times after the fix (a times-2 grid; each drawn inside its own step)")
		healFails = flag.String("heal-fails", "503,404,403",
			"Remembered-failure run: the failures each check gets a page for (the origin serves 503, 404, 403)")
		healTrials = flag.Int("heal-trials", 1,
			"Remembered-failure run: pages per vendor x check time x failure in this sitting")
		healSettle = flag.Duration("heal-settle", 5*time.Minute,
			"Remembered-failure run: wait after the first answer came back before fixing the page (a timer, never a fetch count)")
		healWarmGap = flag.Duration("heal-warm-gap", 90*time.Second,
			"Remembered-failure run: one first ask per slot, this far apart, in shuffled order")
		healStartAt = flag.String("heal-start-at", "",
			"Remembered-failure run: first slot, with its zone (e.g. 2026-10-23T10:00+03:00); empty = now")
		healJitter = flag.Bool("heal-jitter", true,
			"Remembered-failure run: draw each check's wait at random inside its step")
		healSize = flag.Int("heal-size", 2048,
			"Remembered-failure run: byte size of the fixed page (the 2 KB depth page)")
		healLocalOrigin = flag.String("heal-local-origin", "http://localhost:8899",
			"Remembered-failure run: the origin's LOCAL address, where the heal control is called (bypasses Cloudflare). Providers still fetch the public -url")
		healFetchVersion = flag.String("heal-anthropic-fetch-version", defaultHealFetchVersion,
			"Remembered-failure run: the Anthropic web fetch tool, pinned in the state at planning (default: without dynamic filtering)")
		ttlmap = flag.Bool("ttlmap", false,
			"TTL-map: warm N fresh /page/ pages, poll them on a fixed cadence, and record for each the gap of its first re-fetch — its copy lifetime. Run once per model (change -openai-model) to compare TTL across a vendor's models. Driver-only; needs -url and -probe-log")
		ttlmapReplicates = flag.Int("ttlmap-replicates", 8,
			"TTL-map: fresh pages per provider (the N of the TTL distribution)")
		ttlmapCadence = flag.Duration("ttlmap-cadence", 3*time.Minute,
			"TTL-map: how often to re-ask each page. Floored at 150s so a fetch attributes to the ask that caused it (the fetch-counting window)")
		ttlmapFirstPoll = flag.Duration("ttlmap-first-poll", 9*time.Minute,
			"TTL-map: gap of the first poll — start below the suspected lifetime so the first polls are still held, skipping wasted early asks")
		ttlmapWindow = flag.Duration("ttlmap-window", 45*time.Minute,
			"TTL-map: stop polling a page at this gap; a page still held here is reported as living past the window")
		ttlmapPollGaps = flag.String("ttlmap-poll-gaps", "",
			"TTL-map: explicit back-off poll schedule, e.g. \"15m,30m,1h,2h,4h,8h\" — overrides -ttlmap-first-poll/-cadence/-window. Dense early catches a short cache lifetime (OpenAI ~30m); sparse late just checkpoints a long-lived cache (Anthropic/xAI hours-days) so held pages are not re-asked every round. Each gap >= 150s, strictly ascending")
		ttlmapArms = flag.String("ttlmap-arms", "",
			"TTL-map: comma-separated arm labels to keep (e.g. \"cc-nostore-tag,cc-private-tag\"); empty = all. An unknown label is an error")
		ttlmapKeep = flag.Bool("ttlmap-keep-asking", false,
			"TTL-map: keep re-asking a page at every gap even after the vendor fetches it again (default: stop at the first fetch-again). The report then prints how many re-asks were fetched again")
		ttlmapCcdepth = flag.Bool("ttlmap-ccdepth", false,
			"TTL-map: the CACHE-HEADER TEST — 19 arms on /ccdepthpage/ (the shared depth HTML at -ttlmap-size), one per caching-header setting x version info, each graded against what RFC 9111 allows a shared cache for the headers we sent. Needs -page-code-secret and the /ccdepthpage/ route deployed. Pair with -ttlmap-poll-gaps: the limits under test are 0, 1 min, minutes and 1 h")
		ttlmapSize = flag.Int("ttlmap-size", 2048,
			"TTL-map: byte size of the fixed-size arms (-ttlmap-formats, -ttlmap-ccdepth); small on purpose, a held page re-feeds as input tokens on every poll")
		truncprobe = flag.Bool("truncprobe", false,
			"Truncation-depth probe: serve long pages with unforgeable markers at known depths and read the deepest one each model quotes — where its fetcher cut the page. Part A locates the cut per size; Part B (warm top, reask bottom) tests whether the stored copy is the stub. Needs -url, -probe-log and -page-code-secret; deploy the /depthpage/ route first")
		truncprobeGap = flag.Duration("truncprobe-gap", 3*time.Minute,
			"Truncation-depth probe: pause between warming the top marker and asking the bottom marker (Part B). Keep it well inside the copy lifetime so a blank bottom means truncation, not expiry")
		truncprobeMax = flag.Int("truncprobe-max", 0,
			"Truncation-depth probe: largest page size in bytes to test; rungs above it are dropped (0 = no cap, full 64KiB..4MiB ladder). The multi-MB rungs drive nearly all the cost, so cap this (e.g. 2097152 for 2 MiB) to keep a run cheap. If a fetcher reads past the cap the result is just \">cap\".")
		truncprobeSizes = flag.String("truncprobe-sizes", "",
			"Truncation-depth probe: exact page sizes to test instead of the default ladder, e.g. \"2k,4k,8k,16k,32k,64k\" (k=KiB, m=MiB). Use for the small-doc end (measuring a search extract window). Overrides -truncprobe-max. Smallest usable size is ~2k (5 markers must fit).")
		truncprobeRoute = flag.String("truncprobe-route", "/depthpage/",
			"Truncation-depth probe: which generated-page route to hit. Default /depthpage/ (HTML). Format variants for the object-size-by-format study: /depthtxtpage/, /depthpdfpage/, /depthdocxpage/ (same markers, other format). Must be deployed.")
		truncprobeParta = flag.Bool("truncprobe-parta", false,
			"Truncation-depth probe: run ONLY Part A (locate the cut / read depth per size), skip the costly Part B warm+reask. Use for the cheap object-size-by-format smoke.")
		truncprobeReps = flag.Int("truncprobe-reps", 1,
			"Truncation-depth probe: independent pages per (provider, size), so a noisy tool (e.g. OpenAI web_search) is sampled n=reps instead of once. Each rep is a fresh page.")
		faults = flag.Bool("faults", false,
			"Run the failure-retry experiment: ask each provider about the /fault/ paths and count, from -probe-log, how many times its fetcher retried each failure. Needs -url, -probe-log and -page-code-secret (the content-ending faults carry depth markers)")
		faultsWindow = flag.Duration("faults-window", 90*time.Second,
			"How long to watch our log after each fault question. Must exceed the slow fault (~25s) and the Retry-After delay (10s) so a late retry is not missed")
		faultsOut = flag.String("faults-out", "logs/faults.jsonl",
			"Failure-retry experiment: where to write per-provider-per-fault results")
		faultsOnly = flag.String("faults-only", "",
			"Failure-retry experiment: comma-separated fault names to ask about instead of the usual battery (e.g. \"flaky/0\" = a page that works at once, the success baseline). Empty = all")
		faultsRunID = flag.String("run-id", "",
			"Per-run tag folded into every fault URL so a repeat run gets fresh addresses (no cache collision, flaky counter resets). Empty = auto (UTC datetime + random)")
		cacheFile = flag.String("check-cache", "logs/route-cache.json",
			"Where preflight route validations are remembered, so a re-run does not pay for the same check again. TRIALS ARE NEVER CACHED.")
		cacheTTL   = flag.Duration("check-cache-ttl", 6*time.Hour, "How long a passing route validation stays valid")
		freshCheck = flag.Bool("fresh-check", false, "Ignore remembered validations and re-test every route with a real call")
		probeLog   = flag.String("probe-log", "", "Probe log to read during -check, to confirm WHO fetched the page")
		forceFlag  = flag.Bool("force-search", true, "Require the provider to call its search/fetch tool instead of answering from memory")

		// Cheap by default, for the reason in openrouter.go: the task is easy and
		// the study needs many runs. Dashes here, not dots — this is Anthropic's
		// own API id style, unlike the OpenRouter slug.
		anthropicModel        = flag.String("anthropic-model", defaultAnthropicModel, "Anthropic model id")
		anthropicFetchVersion = flag.String("anthropic-fetch-version", "", "Pin the Anthropic web_fetch tool version for the dynamic-filtering A/B (e.g. web_fetch_20250910 = no dynamic filtering, web_fetch_20260209 = has it). Empty = default (web_fetch_20260209). Investigation only.")
		rawOut                = flag.String("raw-out", "logs/raw-replies.jsonl", "Keep every vendor reply, whole, one JSON line per request (time, vendor address, model, page address, round). On by default so later analysis can take anything a runner did not keep (Anthropic's per-fetch error_code, OpenRouter annotations). Empty = off. Append-only")
		anthropicDumpRaw      = flag.String("anthropic-dump-raw", "", "Append each raw Anthropic response to this file (for the dynamic-filtering investigation: inspect usage.server_tool_use / code-execution). Empty = off. Investigation only.")
		openaiModel           = flag.String("openai-model", "gpt-5-mini", "OpenAI model id")
		perplexityModel       = flag.String("perplexity-model", "openai/gpt-5.6-sol",
			"Model for Perplexity's Agent API (it routes to third-party models; the fetch_url tool is Perplexity's own)")
		googleModel = flag.String("google-model", "gemini-3.1-flash-lite", "Gemini model id (both the Developer API and Vertex AI)")
		// grok-4.3 by default to MATCH the OpenRouter x-ai route (x-ai/grok-4.3), so
		// the direct-vs-OpenRouter sharing test compares like with like, and so the
		// default is the model the real run uses — no "remember to switch it back".
		// The cheaper build models lack the web_search tool, so a check on one of
		// those cannot validate the search wiring; override deliberately if needed.
		xaiModel          = flag.String("xai-model", "grok-4.3", "xAI (Grok) model id for the direct x-ai route")
		googleRegion      = flag.String("google-region", "global", "Vertex AI region, e.g. global or us-central1")
		allowTunnelGoogle = flag.Bool("allow-tunnel-google", false,
			"Run Google even on a tunnel hostname. For testing whether a named tunnel on a real domain clears Google's restriction; not for measurement runs")
		googleSearchToo = flag.Bool("google-also-search", false,
			"Add googleSearch alongside urlContext for Gemini. Off by default: search can answer from an index snapshot without fetching, making a missing request ambiguous")
		orModels = flag.String("openrouter-models", strings.Join(defaultOpenRouterModels, ","),
			"Comma-separated OpenRouter model slugs; one provider entry per slug, named or:<vendor>")

		// Web-app console (Phase 1): a person drives the browser, this reads the log.
		// Needs no -url, no provider keys, and no run lock — it only mints addresses
		// and watches the server log. See trial/webtrial.go.
		webBase        = flag.String("webtrial-base", "https://probe.attentionhub.org", "Origin that serves /page/<id>")
		webDir         = flag.String("webtrial-dir", "logs/web-trials", "Directory holding the per-run raw logs (raw/<run>.jsonl) and the derived dataset (dataset.jsonl/.csv)")
		websharingFlag = flag.Bool("websharing", false,
			"Web-app sharing run: does a page one ACCOUNT's chat downloaded get served to a DIFFERENT account's chat? Same pages, prompts and verdicts as -sharing-test; a person pastes. Needs the page-code secret. See trial/websharing.go")
		websharingApps = flag.String("websharing-apps", "claude,chatgpt,gemini,grok,deepseek,mistral,qwen,metaai", "Web-app sharing: apps in this sitting, comma-separated")
		websharingA    = flag.String("websharing-a", "account-a", "Web-app sharing: label for account A (reads first)")
		websharingB    = flag.String("websharing-b", "account-b", "Web-app sharing: label for account B (asks second, and the control)")
		websharingGap  = flag.Duration("websharing-gap", 5*time.Minute, "Web-app sharing: wait from account A's ask to account B's ask, per app")
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
	// Same for the control secret (-robots-*, -correction-*): without this an
	// env-file-only PROBE_CONTROL_SECRET never reaches -control-secret.
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

	// -dry-run and -check are contradictory: -check's whole job is to make a real
	// API call to verify a route, so a "dry check" is meaningless. Surface the
	// contradiction instead of silently doing one or the other.
	if *dryRunFlag && *checkFlag {
		fmt.Fprintln(os.Stderr, "error: -check makes real API calls to verify routes; it cannot be combined with -dry-run. Drop one.")
		os.Exit(1)
	}

	// Set before every mode branch. It used to be assigned only inside the
	// -check branch, which runs last, so the sharing experiment saw an empty
	// path, could not check that its pages were untouched, and spent two API
	// calls on a run that could conclude nothing.
	probeLogPath = *probeLog

	// Pin the Anthropic web_fetch version for the whole run (dynamic-filtering A/B).
	if *anthropicFetchVersion != "" {
		anthropicFetchVersionOverride = anthropicFetchVersion
	}
	anthropicRawDumpPath = *anthropicDumpRaw
	rawOutPath = *rawOut

	if *resolveFlag != "" {
		if err := addResolveOverride(*resolveFlag); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}

	// A diagnostic that needs no -url and makes no API call: it answers "is our
	// own page reachable from here?", which on a network that cannot resolve
	// tunnel hostnames is the first thing to check.
	if *pageCheck != "" {
		l := checkPageLive(context.Background(), *pageCheck, *markerFlag)
		fmt.Printf("%s\n  %s\n", *pageCheck, l)
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
			base: strings.TrimRight(*webBase, "/"), secret: *pageCodeSecret, apps: apps,
			acctA: *websharingA, acctB: *websharingB, gap: *websharingGap,
			webDir: *webDir, outDir: "logs/sharing-redo",
		}))
	}

	if *urlFlag == "" && !(*correctionTick || *correctionLoop || *correctionReport) {
		// The correction run's tick/loop/report read the origin from their state file.
		fmt.Fprintln(os.Stderr, "error: -url is required")
		flag.Usage()
		os.Exit(1)
	}

	vertexRegion = *googleRegion
	googleAlsoSearch = *googleSearchToo
	allowGoogleOnTunnel = *allowTunnelGoogle
	var orList []string
	for _, m := range strings.Split(*orModels, ",") {
		if m = strings.TrimSpace(m); m != "" {
			orList = append(orList, m)
		}
	}
	provs := callers(*anthropicModel, *openaiModel, *perplexityModel, *googleModel, *xaiModel, orList)
	// Google is parked on tunnel hostnames — see parkGoogle for why this is
	// enforced in code rather than left as a thing to remember.
	parkGoogle(provs, *urlFlag)
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

	// -only keeps just the named providers, so one can be run in isolation (a
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
			fmt.Fprintf(os.Stderr, "error: -only %q matched none of the active providers\n", keep)
			os.Exit(1)
		}
		active = filtered
	}

	// BACKSTOP: under -dry-run, no code path may spend money — no matter which
	// experiment branch forgot to implement its own planner. Money is only ever
	// spent inside Caller.Ask, so replacing every active provider's Ask with a
	// refusal here makes a paid call under -dry-run structurally impossible. This
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
			fmt.Fprintln(os.Stderr, "error: -probe-log is required; which version each fetch got is read from it")
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
			fmt.Fprintln(os.Stderr, "error: -probe-log is required; what each request got is read from it")
			os.Exit(1)
		}
		recs, _, lerr := probe.LoadRecords(probeLogPath, "", time.Time{})
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", lerr)
			os.Exit(1)
		}
		if *healReportJSON != "" {
			if werr := writeHealResults(*healReportJSON, st, recs, *pageCodeSecret); werr != nil {
				fmt.Fprintf(os.Stderr, "error: -heal-report-json: %v\n", werr)
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
			fmt.Fprintln(os.Stderr, "error: -dry-run applies to -heal-plan; a tick only fires real steps")
			os.Exit(1)
		}
		os.Exit(runHealTick(active, *healState, *pageCodeSecret, *controlSecretFlag, *healLoop))
	}

	if *correctionTick || *correctionLoop {
		if *dryRunFlag {
			fmt.Fprintln(os.Stderr, "error: -dry-run applies to -correction-plan; a tick only fires real steps")
			os.Exit(1)
		}
		os.Exit(runCorrectionTick(active, *correctionState, *pageCodeSecret, *controlSecretFlag, *correctionLoop))
	}

	if *ttlmap {
		if len(active) == 0 {
			fmt.Fprintln(os.Stderr, "error: no usable providers")
			os.Exit(1)
		}
		// An explicit back-off schedule replaces the fixed first-poll/cadence/window
		// ladder, so those checks only apply when no schedule is given.
		var explicitGaps []time.Duration
		if *ttlmapPollGaps != "" {
			var gerr error
			explicitGaps, gerr = parseDurations(*ttlmapPollGaps)
			if gerr != nil {
				fmt.Fprintf(os.Stderr, "error: -ttlmap-poll-gaps: %v\n", gerr)
				os.Exit(1)
			}
		}
		if len(explicitGaps) == 0 {
			if *ttlmapCadence < 150*time.Second {
				fmt.Fprintln(os.Stderr, "error: -ttlmap-cadence must be at least 150s, or a fetch cannot be pinned to the ask that caused it")
				os.Exit(1)
			}
			if *ttlmapFirstPoll > *ttlmapWindow {
				fmt.Fprintln(os.Stderr, "error: -ttlmap-first-poll must not exceed -ttlmap-window")
				os.Exit(1)
			}
		}
		if *ttlmapReplicates < 1 {
			fmt.Fprintln(os.Stderr, "error: -ttlmap-replicates must be at least 1")
			os.Exit(1)
		}
		// Only the cache-header test is kept; the plain TTL map and its size and
		// format sweeps are not part of this release.
		if !*ttlmapCcdepth {
			fmt.Fprintln(os.Stderr, "error: -ttlmap needs -ttlmap-ccdepth (the cache-header test)")
			os.Exit(1)
		}
		arms := ccDepthArms(*ttlmapSize)
		if sel, serr := selectArms(arms, *ttlmapArms); serr != nil {
			fmt.Fprintf(os.Stderr, "error: -ttlmap-arms: %v\n", serr)
			os.Exit(1)
		} else {
			arms = sel
		}
		ttlmapKeepAsking = *ttlmapKeep
		// Own lock, not the shared run lock: this touches only generated page
		// addresses. A dry run makes no address and no call, so it
		// needs no lock.
		if *dryRunFlag {
			os.Exit(runTTLMap(*urlFlag, active, arms, *ttlmapReplicates, *ttlmapFirstPoll, *ttlmapCadence, *ttlmapWindow, explicitGaps, *pageCodeSecret, *markerFlag, *forceFlag, true))
		}
		lock, lerr := acquireRunLock(ttlmapLockPath, "TTL-map experiment", []string{"generated /page/, /depth*page/ and /ccdepthpage/ addresses (own lock; does not block dev runs)"})
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", lerr)
			os.Exit(1)
		}
		code := runTTLMap(*urlFlag, active, arms, *ttlmapReplicates, *ttlmapFirstPoll, *ttlmapCadence, *ttlmapWindow, explicitGaps, *pageCodeSecret, *markerFlag, *forceFlag, false)
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
		// Own lock, not the shared run lock: this touches only generated /depthpage/
		// addresses. A dry run needs no lock — it makes no address and no call.
		if *dryRunFlag {
			os.Exit(runTruncProbe(*urlFlag, active, *pageCodeSecret, *truncprobeGap, *truncprobeMax, sizesOverride, *truncprobeReps, *truncprobeRoute, *truncprobeParta, *forceFlag, true))
		}
		lock, lerr := acquireRunLock(truncLockPath, "truncation-depth probe", []string{"generated /depthpage/ addresses (own lock; does not block dev runs)"})
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", lerr)
			os.Exit(1)
		}
		code := runTruncProbe(*urlFlag, active, *pageCodeSecret, *truncprobeGap, *truncprobeMax, sizesOverride, *truncprobeReps, *truncprobeRoute, *truncprobeParta, *forceFlag, false)
		lock.Release()
		os.Exit(code)
	}

	if *faults {
		if *faultsWindow < 45*time.Second {
			fmt.Fprintln(os.Stderr, "error: -faults-window must be at least 45s; the slow fault alone holds ~25s")
			os.Exit(1)
		}
		if len(active) == 0 {
			fmt.Fprintln(os.Stderr, "error: no usable providers")
			os.Exit(1)
		}
		lock, lerr := acquireRunLock(runLockPath, "failure-retry experiment", []string{"probe-site/ (measured pages)"})
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

	// The provider check only needs -url. Build (and validate) the arms after
	// it, so a check does not demand -url-repeat.
	if *sharingTest {
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
			fmt.Fprintln(os.Stderr, "  a model not in the active list needs -openrouter-models / -<vendor>-model.")
			os.Exit(1)
		}
		if err := validateSharingPair(a, b, *sharingFirstUser, *sharingSecondUser, *sharingBaseline); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		// Its own lock per comparison: the pages are generated per run (none under
		// probe-site/), so different comparisons can run side by side.
		lock, lerr := acquireRunLock(sharingLockPath(a, b, *sharingFirstUser, *sharingSecondUser), "sharing experiment",
			[]string{"generated /depthhtmlpage/sh-<run>/ addresses"})
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
		if runCheck(active, *urlFlag, *markerFlag, *forceFlag) {
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
// point that makes a paid call under -dry-run impossible regardless of which
// experiment path runs. See the backstop note at the call site.
func refuseUnderDryRun(provs []Caller) {
	for i := range provs {
		provs[i].Ask = func(ctx context.Context, key, model, url, question string, force bool) (string, probe.Usage, error) {
			return "", probe.Usage{}, fmt.Errorf("dry-run: refused a real API call (%s)", url)
		}
	}
}
