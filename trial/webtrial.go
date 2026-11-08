package main

// The web-app console (`-webtrial`) and its dataset.
//
// Phase 1+ of the web-app study (doc/web-app-automation-plan.md). We do NOT drive
// a browser robot. A person is the browser; this console removes every error-prone
// step around the clicking.
//
// STORAGE: TIDY FILES, A DERIVED DATASET, QUERIED WITH DUCKDB.
//
//   logs/web-trials/
//     raw/<run-id>.jsonl   append-only, ONE FILE PER RUN. Holds the one thing we
//                          cannot recreate: the verbatim reply you pasted, plus
//                          the trial's id, run id, time, app, and unique address.
//                          A line here is an observation; it is never rewritten,
//                          and runs never pile into one file.
//     dataset.jsonl        DERIVED from all the raw files + the server log, and
//     dataset.csv          rewritten wholesale on every rebuild.
//
// Because every trial has its OWN unguessable address, the fetch count, the
// fetcher's identity, robots, and the code-in-answer check are all recomputable
// from the two logs. So the dataset is a projection, not a pile you can only
// append to: a bug in any derived field is fixed by rebuilding, not by hand.
//
// To QUERY it, point DuckDB at the files at analysis time (SQL straight over the
// JSONL/CSV, no import, no dependency baked into this binary):
//     duckdb -c "SELECT app, verdict, count(*) FROM 'logs/web-trials/dataset.jsonl' GROUP BY 1,2"
// The `run_id` column joins a run against its states or the raw request log.
//
// WHY THE REPLY IS CAPTURED IN AN EDITOR. A pasted chat reply can contain any
// text at all — blank lines, the word END, a lone dot — so no in-band terminator
// is safe (each one collided with real replies). The standard fix for capturing an
// arbitrary block of text is the one `git commit` uses: open the person's editor
// on a temp file, let them paste and save, and read the file. No terminator, no
// signal in the stream, nothing to collide with.
//
// WHY THE SERVER IS UNTOUCHED. servePage 404s any id containing a "/", so a true
// per-app path range (/page/chatgpt/<id>) is a server change and waits for the
// lock-free window after the retention run. Until then the app name is folded
// into the single id segment as "<app>-<nonce>", so the path still says who
// fetched, and no server change is needed.

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aisearch-probe/internal/probe"
)

// ksaOffset converts a UTC instant to Riyadh clock time. Logs stay UTC; every
// time shown to the person is KSA, per the standing rule.
const ksaOffset = 3 * time.Hour

// standardConfirmReply is the ONE fixed line the console gives, identical for
// every app, when an app asks permission before browsing and we are running the
// "when permitted" (confirmed) condition. It must never be improvised per app, or
// the confirmed results stop being comparable. See doc/web-app-automation-plan.md.
const standardConfirmReply = "Yes, please go ahead."

// webTrialConfig is everything one batch needs.
type webTrialConfig struct {
	app      string // which chat app; names the path prefix and the vendor column
	config   string // the app setup under test (recorded in the model column)
	n        int    // how many trials in this batch
	base     string // origin serving /page/<id>
	marker   string // the code that proves a real fetch came back in the answer
	phase    string // phase label for the dataset
	dir      string // logs/web-trials — holds raw/<run>.jsonl and the dataset
	probeLog string // the server log (ground truth), read at rebuild time
	reaskOf  string // if set, re-ask the SAME page as this earlier trial (holds-a-copy test)
	// coded: use the /cpage/ route, where each page carries its own unforgeable code,
	// so "served-from-store" is solid. codeSecret recomputes those codes to check an
	// answer; it is the driver's own copy of the server secret and is NEVER sent to
	// the app (not in the URL, not in the prompt).
	coded      bool
	codeSecret string
	// followups: warm one page, then re-ask it at scheduled offsets, each with a
	// session label, in one sitting. Form: "0m:same-session,5m:new-session".
	followups string
	// reaskBatch: re-ask every earlier warmed page whose config starts with this
	// label, in one sitting — the lifetime / holds-a-copy checkpoints.
	reaskBatch string
}

// followupStep is one scheduled re-ask: how long after the warm, and a label the
// person follows (which chat/session to use) that is also recorded in config.
type followupStep struct {
	off   time.Duration
	label string
}

// webRawRecord is one line of an append-only raw run file: the trial's identity
// plus the verbatim reply. Everything else in the dataset is derived from this
// and the server log.
type webRawRecord struct {
	TrialID  string  `json:"trial_id"`
	RunID    string  `json:"run_id"`
	SentUTC  string  `json:"sent_utc"`
	SentUnix float64 `json:"sent_unix"`
	Phase    string  `json:"phase"`
	App      string  `json:"app"`
	Config   string  `json:"config"`
	PageURL  string  `json:"page_url"`
	Marker   string  `json:"marker"`
	Reply    string  `json:"reply"`

	// Ask-first handling. AskedBack: the app asked a question / for permission in
	// round 1 instead of acting. Confirmed: we then gave standardConfirmReply and
	// Reply2 is the app's reply to it ("when permitted" condition).
	AskedBack   bool   `json:"asked_back,omitempty"`
	Confirmed   bool   `json:"confirmed,omitempty"`
	ConfirmText string `json:"confirm_text,omitempty"`
	Reply2      string `json:"reply2,omitempty"`

	// ReaskOf, when set, is the trial_id of an earlier trial whose SAME page this
	// trial asked about again (the holds-a-copy test). The page URL is reused; the
	// trial id is new.
	ReaskOf string `json:"reask_of,omitempty"`

	// Notes is a free-text hook for anything unexpected the person saw.
	Notes string `json:"notes,omitempty"`

	// AnsweredUnix: when the reply was pasted back (-websharing only). Requests
	// after it cannot have produced the answer.
	AnsweredUnix float64 `json:"answered_unix,omitempty"`
}

// runWebTrial drives one batch of hand-run web-app trials. Returns an exit code.
func runWebTrial(cfg webTrialConfig) int {
	cfg.base = strings.TrimRight(cfg.base, "/")
	if cfg.probeLog == "" {
		cfg.probeLog = "logs/probe.jsonl"
	}
	if cfg.n < 1 {
		cfg.n = 1
	}
	rawDir := filepath.Join(cfg.dir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "[webtrial] cannot make %s: %v\n", rawDir, err)
		return 1
	}
	runID := runNonce()
	rawPath := filepath.Join(rawDir, runID+".jsonl")
	in := bufio.NewReader(os.Stdin)

	if cfg.followups != "" && cfg.reaskOf != "" {
		fmt.Fprintln(os.Stderr, "[webtrial] -webtrial-followups and -webtrial-reask cannot be combined")
		return 1
	}
	var steps []followupStep
	if cfg.followups != "" {
		var err error
		steps, err = parseFollowups(cfg.followups)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[webtrial] -webtrial-followups: %v\n", err)
			return 1
		}
	}

	// A re-ask reuses an earlier trial's page instead of minting a new one — the
	// holds-a-copy test. Look it up once, up front, so a bad id fails before any
	// browser work.
	var reuseURL, reuseMarker string
	if cfg.reaskOf != "" {
		orig, err := findRawByTrialID(rawDir, cfg.reaskOf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[webtrial] -webtrial-reask %s: %v\n", cfg.reaskOf, err)
			return 1
		}
		reuseURL, reuseMarker = orig.PageURL, orig.Marker
		fmt.Printf("re-asking the page from trial %s (ask it in a FRESH chat): %s\n", cfg.reaskOf, reuseURL)
	}

	// Batch re-ask loads every earlier warmed page matching the config label.
	var allRaws []webRawRecord
	if cfg.reaskBatch != "" {
		if cfg.followups != "" || cfg.reaskOf != "" {
			fmt.Fprintln(os.Stderr, "[webtrial] -webtrial-reask-batch cannot combine with -webtrial-followups or -webtrial-reask")
			return 1
		}
		var err error
		allRaws, err = loadAllRaw(rawDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[webtrial] reading raw logs: %v\n", err)
			return 1
		}
	}

	// Reachability: if we cannot fetch our own page, a "no fetch" tells us nothing
	// (see trial/liveness.go). Warn, but let the person decide.
	if cfg.coded && cfg.codeSecret == "" {
		fmt.Fprintln(os.Stderr, "[webtrial] -webtrial-coded needs the code secret (set PROBE_PAGE_CODE_SECRET or -webtrial-code-secret)")
		return 1
	}

	golden := cfg.base + "/page/golden-fixed-id"
	live := checkPageLive(context.Background(), golden, cfg.marker)
	host := strings.TrimPrefix(strings.TrimPrefix(cfg.base, "https://"), "http://")
	if live.OK {
		fmt.Printf("setup: %s reachable ✓\n", host)
	} else {
		fmt.Printf("setup: %s NOT reachable — %s\n", host, live.String())
		fmt.Print("a 'no fetch' will be meaningless if the page is down. press ENTER to go on anyway, or Ctrl-C to stop: ")
		_, _ = in.ReadString('\n')
	}

	// In coded mode, also confirm the /cpage/ route itself is live (it only exists
	// once the origin is restarted with a page-code secret), so a "route not
	// deployed" state is caught here rather than read as every trial missing.
	if cfg.coded {
		_, curl, cmark := mintCoded(cfg.base, "probe", cfg.codeSecret)
		if cl := checkPageLive(context.Background(), curl, cmark); cl.OK {
			fmt.Println("setup: /cpage/ coded route reachable ✓ (per-page codes)")
		} else {
			fmt.Printf("setup: /cpage/ coded route NOT reachable — is it deployed? (%s)\n", cl.String())
			fmt.Print("press ENTER to go on anyway, or Ctrl-C to stop: ")
			_, _ = in.ReadString('\n')
		}
	}

	fmt.Printf("run %s — set %s to: browse ON", runID, cfg.app)
	if cfg.config != "" {
		fmt.Printf(", %s", cfg.config)
	}
	fmt.Print("  (press ENTER when ready) ")
	_, _ = in.ReadString('\n')

	written := 0
	appendOrFail := func(rec webRawRecord) bool {
		if err := appendRaw(rawPath, rec); err != nil {
			fmt.Fprintf(os.Stderr, "  ERROR: could not write the raw log %s: %v\n", rawPath, err)
			return false
		}
		written++
		return true
	}

	if cfg.reaskBatch != "" {
		// Batch re-ask: revisit every earlier warmed page whose config starts with
		// the label — the lifetime / holds-a-copy checkpoints, in one sitting.
		warms := selectWarms(allRaws, cfg.reaskBatch)
		if len(warms) == 0 {
			fmt.Fprintf(os.Stderr, "[webtrial] no warmed pages found with config starting %q\n", cfg.reaskBatch)
			return 1
		}
		fmt.Printf("\nre-asking %d warmed page(s) matching %q — open a FRESH chat for each\n", len(warms), cfg.reaskBatch)
		for i, w := range warms {
			fmt.Printf("\n[%d/%d] === re-ask %s in a FRESH %s chat (same page) ===\n", i+1, len(warms), w.App, w.App)
			c := cfg
			c.app = w.App
			rec, ok := oneWebTrial(c, runID, in, w.PageURL, w.Marker, w.TrialID, cfg.config)
			if !ok || !appendOrFail(rec) {
				break
			}
		}
	} else if len(steps) > 0 {
		// Lifecycle run: warm one page, then re-ask it at the scheduled offsets.
		fmt.Println("\nWarm (round 1)")
		warm, ok := oneWebTrial(cfg, runID, in, "", "", "", joinCfg(cfg.config, "warm"))
		if !ok || !appendOrFail(warm) {
			return 1
		}
		// Each step's offset is the gap to wait SINCE THE PREVIOUS step finished, not
		// since the warm — so the time you spend pasting a step never eats into the
		// next gap. The actual send time is still recorded, so the measured gap is
		// exact regardless of how long a paste took.
		last := nowUnix()
		for _, st := range steps {
			waitAndAlert(last+st.off.Seconds(), st.label)
			fmt.Printf("\n==== re-ask NOW in %s (same page) ====\n", st.label)
			rec, ok := oneWebTrial(cfg, runID, in, warm.PageURL, warm.Marker, warm.TrialID, joinCfg(cfg.config, st.label))
			if !ok || !appendOrFail(rec) {
				break
			}
			last = nowUnix()
		}
	} else {
		for i := 1; i <= cfg.n; i++ {
			fmt.Printf("\nTrial %d/%d\n", i, cfg.n)
			rec, ok := oneWebTrial(cfg, runID, in, reuseURL, reuseMarker, cfg.reaskOf, "")
			if !ok {
				fmt.Fprintln(os.Stderr, "  trial aborted")
				break
			}
			if !appendOrFail(rec) {
				return 1
			}
		}
	}

	if written == 0 {
		return 1
	}
	// Rebuild the dataset from every raw run file + the server log, so the trials
	// just run are scored against what the server actually saw.
	rows, err := rebuildDataset(cfg.dir, cfg.probeLog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[webtrial] rebuild failed: %v\n", err)
		return 1
	}
	// Show only THIS run's rows — the whole dataset is noise after every trial. The
	// full table is in dataset.csv / -webtrial-rebuild.
	mine := rowsForRun(rows, runID)
	fmt.Printf("\n[webtrial] %d trial(s) this run; %d total in the dataset (full table: %s)\n\n",
		len(mine), len(rows), filepath.Join(cfg.dir, "dataset.csv"))
	probe.RenderTrialTable(os.Stdout, mine)
	return 0
}

// rowsForRun keeps just the rows written by one run, so the after-run table shows
// what you just did, not the entire history.
func rowsForRun(rows []probe.TrialRow, runID string) []probe.TrialRow {
	var out []probe.TrialRow
	for _, r := range rows {
		if r.RunID == runID {
			out = append(out, r)
		}
	}
	return out
}

// runWebRebuild regenerates the dataset from the raw logs and prints it, without
// running a trial. Used by -webtrial-rebuild.
func runWebRebuild(dir, probeLog string) int {
	rows, err := rebuildDataset(dir, probeLog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[webtrial] rebuild failed: %v\n", err)
		return 1
	}
	fmt.Printf("[webtrial] dataset rebuilt (%d rows) from %s\n\n", len(rows), dir)
	probe.RenderTrialTable(os.Stdout, rows)
	return 0
}

// oneWebTrial runs a single trial and returns the raw record to append. When
// reuseURL is set (a re-ask), the trial points at that existing page instead of
// minting a fresh one; the trial id is still new. reaskOf and configOverride, when
// non-empty, set those fields (used by the -reask and -followups paths).
func oneWebTrial(cfg webTrialConfig, runID string, in *bufio.Reader, reuseURL, reuseMarker, reaskOf, configOverride string) (webRawRecord, bool) {
	id := cfg.app + "-" + runNonce()
	pageURL := cfg.base + "/page/" + id
	marker := cfg.marker
	switch {
	case reuseURL != "":
		// A re-ask reuses the earlier page and its stored code.
		pageURL, marker = reuseURL, reuseMarker
	case cfg.coded:
		// A fresh coded page: its own unforgeable code becomes this trial's marker.
		id, pageURL, marker = mintCoded(cfg.base, cfg.app, cfg.codeSecret)
	}
	config := cfg.config
	if configOverride != "" {
		config = configOverride
	}
	sentAt := time.Now().UTC()

	fmt.Println("  paste this into the app and send:")
	fmt.Println("  " + strings.Repeat("-", 58))
	fmt.Printf("  What does the page at %s say? Quote its reference code.\n", pageURL)
	fmt.Println("  " + strings.Repeat("-", 58))
	fmt.Print("  send it, wait for the reply, then press ENTER to open your editor to paste it… ")
	_, _ = in.ReadString('\n')

	reply, err := captureReplyViaEditor(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  editor failed: %v\n", err)
		return webRawRecord{}, false
	}
	if strings.TrimSpace(reply) == "" {
		fmt.Println("  (warning: the pasted reply was empty)")
	}

	rec := webRawRecord{
		TrialID:  id,
		RunID:    runID,
		SentUTC:  sentAt.Format(time.RFC3339),
		SentUnix: float64(sentAt.UnixNano()) / 1e9,
		Phase:    cfg.phase,
		App:      cfg.app,
		Config:   config,
		PageURL:  pageURL,
		Marker:   marker,
		Reply:    reply,
		ReaskOf:  reaskOf,
	}

	// Ask-first handling, decided per instance (NOT a pre-set batch condition): web
	// apps are observational, not a controlled experiment, so we react to what an app
	// actually does and record it. If it asked a question / for permission instead of
	// acting, that is its own recorded outcome. Whether to then grant permission is
	// your call right here, for this one case; the confirmation, when given, is the
	// one fixed line for every app so those instances stay comparable.
	if askYesNo(in, "  did the app ask a question / for permission instead of answering?") {
		rec.AskedBack = true
		if askYesNo(in, "  reply with the standard confirmation ("+standardConfirmReply+") and continue?") {
			fmt.Println("  paste this EXACT line into the SAME chat and send:")
			fmt.Println("  " + strings.Repeat("-", 58))
			fmt.Println("  " + standardConfirmReply)
			fmt.Println("  " + strings.Repeat("-", 58))
			fmt.Print("  then wait for the reply and press ENTER to open your editor for it… ")
			_, _ = in.ReadString('\n')
			reply2, err := captureReplyViaEditor(id + "-r2")
			if err != nil {
				fmt.Fprintf(os.Stderr, "  editor failed: %v\n", err)
				return webRawRecord{}, false
			}
			rec.Confirmed = true
			rec.ConfirmText = standardConfirmReply
			rec.Reply2 = reply2
		} else {
			fmt.Println("  recorded: app asked first, not confirmed")
		}
	}

	// A free-text hook for anything unexpected — the point is to record odd behavior
	// now and find it later, since we cannot control what an app does.
	fmt.Print("  note anything unexpected (ENTER to skip): ")
	note, _ := in.ReadString('\n')
	rec.Notes = strings.TrimSpace(note)

	return rec, true
}

// askYesNo prints a prompt and reads a yes/no answer. Anything starting with y/Y
// is yes; everything else (including a bare ENTER) is no, so a slip defaults to
// the conservative "did not ask".
func askYesNo(in *bufio.Reader, prompt string) bool {
	fmt.Print(prompt + " (y/N) ")
	line, _ := in.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return strings.HasPrefix(line, "y")
}

// mintCoded builds a fresh coded page for one trial: a /cpage/ URL whose id the
// server can parse (cr<NN>-<app>-<nonce>, index 0 — the caching test asks by
// address, so the subject is unused) and the page's own reference code, recomputed
// from the driver's secret. The secret is used only here to compute the marker; it
// never goes into the URL or the prompt.
func mintCoded(base, app, secret string) (id, pageURL, marker string) {
	id = "cr0-" + app + "-" + runNonce()
	pageURL = strings.TrimRight(base, "/") + "/cpage/" + id
	marker = probe.CpageCode(secret, id, probe.CpageFieldRef, 1)
	return id, pageURL, marker
}

// selectWarms picks the warmed pages a batch re-ask should revisit: config starts
// with prefix, it is a warm (not itself a re-ask), and each page is taken once.
func selectWarms(raws []webRawRecord, prefix string) []webRawRecord {
	var out []webRawRecord
	seen := map[string]bool{}
	for _, r := range raws {
		if r.ReaskOf != "" || !strings.HasPrefix(r.Config, prefix) {
			continue
		}
		if seen[r.PageURL] {
			continue
		}
		seen[r.PageURL] = true
		out = append(out, r)
	}
	return out
}

// parseFollowups parses "0m:same-session,5m:new-session" into ordered steps.
// Each item is an offset (a Go duration, from the warm time) and a session label.
func parseFollowups(s string) ([]followupStep, error) {
	var out []followupStep
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.IndexByte(part, ':')
		if i < 0 {
			return nil, fmt.Errorf("bad step %q (want offset:label, e.g. 5m:new-session)", part)
		}
		off, err := time.ParseDuration(strings.TrimSpace(part[:i]))
		if err != nil {
			return nil, fmt.Errorf("bad offset in %q: %w", part, err)
		}
		label := strings.TrimSpace(part[i+1:])
		if label == "" {
			return nil, fmt.Errorf("empty label in %q", part)
		}
		out = append(out, followupStep{off: off, label: label})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no steps parsed")
	}
	return out, nil
}

// waitAndAlert sleeps until untilUnix, then rings the terminal bell several times
// so the person notices without watching the terminal (the bell reaches the local
// terminal even over ssh). Being late is fine: the next ask records its own real
// time, so the measured gap is exact regardless.
func waitAndAlert(untilUnix float64, label string) {
	if remain := untilUnix - nowUnix(); remain > 0 {
		fmt.Printf("\n  waiting %s for the next step (%s) — you'll hear a beep…\n",
			time.Duration(remain*float64(time.Second)).Round(time.Second), label)
	}
	for {
		remain := untilUnix - nowUnix()
		if remain <= 0 {
			break
		}
		time.Sleep(time.Duration(math.Min(remain, 5) * float64(time.Second)))
	}
	for i := 0; i < 6; i++ {
		fmt.Print("\a")
		time.Sleep(250 * time.Millisecond)
	}
}

func nowUnix() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// joinCfg combines the app-setup label with a lifecycle/session label, keeping
// both dimensions in the config column (e.g. "free-loggedin/new-session").
func joinCfg(base, extra string) string {
	switch {
	case base == "":
		return extra
	case extra == "":
		return base
	default:
		return base + "/" + extra
	}
}

// captureReplyViaEditor opens the person's editor on an empty temp file and reads
// back whatever they saved. Empty file, no instruction lines: a chat reply can
// start with '#' (a markdown heading), so we must not strip any lines. The temp
// file is removed afterwards.
func captureReplyViaEditor(trialID string) (string, error) {
	f, err := os.CreateTemp("", "webtrial-"+safeName(trialID)+"-*.txt")
	if err != nil {
		return "", err
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	name, args := editorCommand()
	args = append(args, path)
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("running %s: %w", name, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// editorCommand picks the editor: $VISUAL, then $EDITOR, then the first installed
// of vim, vi, nano. An editor set with flags (e.g. "code -w") is honoured by
// splitting on spaces.
func editorCommand() (string, []string) {
	for _, env := range []string{os.Getenv("VISUAL"), os.Getenv("EDITOR")} {
		if fields := strings.Fields(env); len(fields) > 0 {
			return fields[0], fields[1:]
		}
	}
	for _, cand := range []string{"vim", "vi", "nano"} {
		if _, err := exec.LookPath(cand); err == nil {
			return cand, nil
		}
	}
	return "vi", nil // last resort; almost always present
}

// safeName keeps a temp-file fragment to characters that are safe in a filename.
func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		default:
			return '_'
		}
	}, s)
}

// webVerdict collapses one web-app trial into a fixed word. A fetch in the log
// wins outright. Otherwise, an app that asked first and was NOT confirmed is its
// own outcome ("asked-permission") — not "no-evidence", which would wrongly lump
// "politely asked" with "browsed nothing and had nothing". A confirmed trial that
// still did not fetch falls through to the code/no-evidence cases: it didn't
// browse even when permitted.
func webVerdict(pageFetches int, codeInAnswer, askedBack, confirmed bool) string {
	switch {
	case pageFetches > 0:
		return "fetched"
	case askedBack && !confirmed:
		return "asked-permission"
	case codeInAnswer:
		return "served-from-store"
	default:
		return "no-evidence"
	}
}

// rebuildDataset derives the whole dataset from every raw run file and the server
// log, and writes it as JSONL and CSV under dir. Regenerable by construction: run
// it any time to re-score every trial against the latest server log and logic.
func rebuildDataset(dir, probeLog string) ([]probe.TrialRow, error) {
	if probeLog == "" {
		probeLog = "logs/probe.jsonl"
	}
	raws, err := loadAllRaw(filepath.Join(dir, "raw"))
	if err != nil {
		return nil, err
	}
	records, _, err := probe.LoadRecords(probeLog, "", time.Time{})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", probeLog, err)
	}
	// A page may be asked more than once (a re-ask/holds-a-copy test). Each ask on a
	// page owns the fetches from its own send time up to the NEXT ask on that same
	// page — so the first ask gets the warm fetch and a later re-ask gets only the
	// new one. Precompute, per page, the sorted list of ask times to find each ask's
	// upper bound.
	askTimes := map[string][]float64{}
	for _, rw := range raws {
		k := pageKeyOf(rw.PageURL)
		askTimes[k] = append(askTimes[k], rw.SentUnix)
	}
	for k := range askTimes {
		sort.Float64s(askTimes[k])
	}
	rows := make([]probe.TrialRow, 0, len(raws))
	for _, rw := range raws {
		upper := nextAskAfter(askTimes[pageKeyOf(rw.PageURL)], rw.SentUnix)
		rows = append(rows, deriveOne(rw, records, upper))
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].SentUnix < rows[j].SentUnix })

	if err := writeJSONL(filepath.Join(dir, "dataset.jsonl"), rows); err != nil {
		return nil, err
	}
	if err := writeCSV(filepath.Join(dir, "dataset.csv"), rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// deriveOne builds one dataset row from a raw record and the server log. It counts
// the fetches on this trial's page from this ask's send time up to `upper` (the
// next ask on the same page, or +Inf). The address is one-of-a-kind, so a hit on
// it belongs to this page; the time window then assigns it to the right ask when a
// page is re-asked. A fetch cannot precede the send time (we stamp the send before
// showing the prompt), so the warm fetch lands in the first ask's window.
func deriveOne(raw webRawRecord, records []probe.Record, upper float64) probe.TrialRow {
	key := pageKeyOf(raw.PageURL)
	var hits []probe.Record
	for _, r := range records {
		if r.PageKey == key && r.TSUnix >= raw.SentUnix && r.TSUnix < upper {
			hits = append(hits, r)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].TSUnix < hits[j].TSUnix })

	// The code can come back in either round (a confirmed trial has a second reply).
	code := containsFold(raw.Reply, raw.Marker) || containsFold(raw.Reply2, raw.Marker)
	row := probe.TrialRow{
		TrialID:       raw.TrialID,
		RunID:         raw.RunID,
		SentUTC:       raw.SentUTC,
		SentUnix:      raw.SentUnix,
		Phase:         raw.Phase,
		Surface:       "web-app",
		Vendor:        raw.App,
		Model:         raw.Config,
		ToolForced:    false,
		PageURL:       raw.PageURL,
		RobotsFetched: robotsSeen(hits, records),
		PageFetches:   len(hits),
		CodeInAnswer:  code,
		AskedBack:     raw.AskedBack,
		Confirmed:     raw.Confirmed,
		ReaskOf:       raw.ReaskOf,
		Notes:         raw.Notes,
		Verdict:       webVerdict(len(hits), code, raw.AskedBack, raw.Confirmed),
	}
	if len(hits) > 0 {
		h := hits[0]
		row.FetcherName = h.UserAgent
		row.FetcherIP = h.ClientIP
		row.AskedIfChanged = h.AskedIfChanged()
		row.Status = h.StatusCode
		row.PageBytes = h.BytesSent
	}
	return row
}

// robotsSeen reports whether the fetcher that took the page also pulled
// /robots.txt. It matches on the fetcher's IP or classified name, because a
// fetcher may spread its reads across a pool of addresses. With no page fetch
// there is no identity to attach a stray robots read to, so false.
func robotsSeen(pageHits, all []probe.Record) bool {
	if len(pageHits) == 0 {
		return false
	}
	ips := map[string]bool{}
	bots := map[string]bool{}
	for _, h := range pageHits {
		if h.ClientIP != "" {
			ips[h.ClientIP] = true
		}
		if h.Bot != "" && h.Bot != "unknown" {
			bots[h.Bot] = true
		}
	}
	for _, r := range all {
		if r.PageKey != "/robots.txt" {
			continue
		}
		if ips[r.ClientIP] || bots[r.Bot] {
			return true
		}
	}
	return false
}

// nextAskAfter returns the smallest ask time strictly greater than t in the sorted
// slice, or +Inf if there is none — the upper bound of this ask's fetch window.
func nextAskAfter(sortedAsks []float64, t float64) float64 {
	for _, a := range sortedAsks {
		if a > t {
			return a
		}
	}
	return math.Inf(1)
}

// findRawByTrialID looks up one earlier trial's raw record by its trial id, across
// all run files, so a re-ask can reuse its page.
func findRawByTrialID(rawDir, trialID string) (webRawRecord, error) {
	raws, err := loadAllRaw(rawDir)
	if err != nil {
		return webRawRecord{}, err
	}
	for _, r := range raws {
		if r.TrialID == trialID {
			return r, nil
		}
	}
	return webRawRecord{}, fmt.Errorf("no trial with that id under %s", rawDir)
}

// pageKeyOf strips a page URL down to its path (the identity of "a page"), the
// same shape the server log stores in page_key.
func pageKeyOf(pageURL string) string {
	if u, err := url.Parse(pageURL); err == nil {
		return u.Path
	}
	return pageURL
}

// appendRaw appends one raw record to a run file. Append-only: the raw log is the
// observation and must survive a crash mid-batch.
func appendRaw(path string, rec webRawRecord) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(rec)
}

// loadAllRaw reads every raw run file under rawDir, oldest run first (the run id
// begins with a timestamp, so a filename sort is chronological). A missing dir is
// an empty dataset, not an error.
func loadAllRaw(rawDir string) ([]webRawRecord, error) {
	paths, err := filepath.Glob(filepath.Join(rawDir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []webRawRecord
	for _, p := range paths {
		recs, err := loadRawFile(p)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		out = append(out, recs...)
	}
	return out, nil
}

// loadRawFile reads one append-only run file.
func loadRawFile(path string) ([]webRawRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []webRawRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // replies can be long
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r webRawRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// writeJSONL writes the dataset as newline-delimited JSON via a temp file and a
// rename, so a crashed rebuild never leaves a half-written dataset.
func writeJSONL(path string, rows []probe.TrialRow) error {
	return writeAtomic(path, func(f *os.File) error {
		return probe.WriteTrialRowsJSONL(f, rows)
	})
}

// writeCSV writes the dataset as CSV for a spreadsheet or a quick eyeball. Every
// column is spelled out; a KSA clock column sits beside the UTC one.
func writeCSV(path string, rows []probe.TrialRow) error {
	return writeAtomic(path, func(f *os.File) error {
		w := csv.NewWriter(f)
		header := []string{
			"trial_id", "run_id", "sent_utc", "sent_ksa", "phase", "app", "config",
			"tool_forced", "robots_fetched", "page_fetches", "code_in_answer",
			"asked_back", "confirmed", "reask_of", "verdict", "fetcher_name",
			"fetcher_ip", "status", "page_url", "notes",
		}
		if err := w.Write(header); err != nil {
			return err
		}
		for _, r := range rows {
			rec := []string{
				r.TrialID,
				r.RunID,
				r.SentUTC,
				ksaString(r.SentUnix),
				r.Phase,
				r.Vendor,
				r.Model,
				boolStr(r.ToolForced),
				boolStr(r.RobotsFetched),
				fmt.Sprintf("%d", r.PageFetches),
				boolStr(r.CodeInAnswer),
				boolStr(r.AskedBack),
				boolStr(r.Confirmed),
				r.ReaskOf,
				r.Verdict,
				r.FetcherName,
				r.FetcherIP,
				fmt.Sprintf("%d", r.Status),
				r.PageURL,
				r.Notes,
			}
			if err := w.Write(rec); err != nil {
				return err
			}
		}
		w.Flush()
		return w.Error()
	})
}

// writeAtomic writes via a temp file in the same directory, then renames over the
// target — so a reader never sees a partial dataset.
func writeAtomic(path string, write func(*os.File) error) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".webtrial-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := write(tmp); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// ksaString renders a Unix time as Riyadh wall-clock, for the CSV.
func ksaString(sentUnix float64) string {
	if sentUnix <= 0 {
		return ""
	}
	sec := int64(sentUnix)
	t := time.Unix(sec, int64((sentUnix-float64(sec))*1e9)).UTC().Add(ksaOffset)
	return t.Format("2006-01-02 15:04:05")
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
