package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aisearch-probe/internal/probe"
)

// THE WEB-APP SHARING RUN (-websharing).
//
// The API sharing test (sharing.go) for the consumer chat apps, where a person is
// the browser. Same question: does a page one ACCOUNT's chat downloaded get served
// to a DIFFERENT account's chat? Two logins per app, in separate browser profiles.
//
// It is the API run's steps 1-3 — same 2 KB depth pages, the SAME prompt text the
// API sends (buildPrompt), the same per-page codes and the same verdicts — so web
// and API results compare directly. No late re-ask: the API run already measures
// how long a copy lasts, and every extra step is more hand-pasting.
//
// Batched: account A warms every app's page first, then (each 5 min after its own
// warm) account B asks each app's shared page, then each app's control page. The
// waits overlap, so a sitting costs little more than the pasting.
//
// Storage: the replies go to the web-trial raw log (logs/web-trials/raw/<run>.jsonl,
// phase "sharing", same format as -webtrial), the steps to
// logs/sharing-redo/web-<run>.jsonl, and the standard records with surface web-app.

type webSharingConfig struct {
	base, secret   string
	apps           []string
	acctA, acctB   string
	gap            time.Duration
	webDir, outDir string
}

// sharingReaskDepth is the marker account A's same-account re-ask asks for: a
// middle one no other step on the shared page asks for.
const sharingReaskDepth = 25

// buildWebSharingPlan is, for one app: account A warms (the API run's step 1);
// account A asks the same page again in a new chat (the same-account re-ask —
// does the app keep a copy at all?); account B asks the same page and a fresh
// control page (the API run's steps 2 and 3). Each app gets its own pages.
func buildWebSharingPlan(base, runID, app, secret, acctA, acctB string) []sharingPlanStep {
	api := buildSharingPlan(base, runID+"-"+app, secret, Caller{Name: app}, Caller{Name: app}, acctA, acctB, 0, 0)
	warm, ask, ctl := api[0], api[1], api[2]
	reask := warm
	reask.label = "2: first account asks the shared page again, in a new chat — SAME ACCOUNT"
	reask.question = sharingAskQuestion(sharingReaskDepth)
	reask.wantDepth = sharingReaskDepth
	reask.want = probe.DepthCode(secret, reask.id, sharingReaskDepth, probe.DepthPageRevision)
	ask.label = "3: second account asks the shared page — THE SHARING MEASUREMENT"
	ctl.label = "4: second account asks a fresh page — the control"
	return []sharingPlanStep{warm, reask, ask, ctl}
}

// webSharingOrder is the running-order screen shown before anything is asked.
func webSharingOrder(apps []string, gap time.Duration) string {
	var b strings.Builder
	list := strings.Join(apps, ", ")
	fmt.Fprintf(&b, "\n\033[1mRUNNING ORDER — each part goes through ALL %d apps before the next part starts\033[0m\n", len(apps))
	fmt.Fprintf(&b, "  Part 1: account A, each app in turn (%s): new chat, paste, paste the reply back\n", list)
	fmt.Fprintf(&b, "  Part 2: account A again, each app in a NEW chat — each one %s after its part 1 (it beeps)\n", gap)
	b.WriteString("  Part 3: account B, each app in turn: the SAME page\n")
	b.WriteString("  Part 4: account B again, each app in turn: a FRESH page (the control)\n")
	b.WriteString("  Each step: NEW chat, web search on, paste, send, then paste the reply into the editor.\n")
	b.WriteString("  Reply too long for one paste? Save what you have; at \"complete?\" type m to paste the rest.\n")
	b.WriteString("  Notes are ONE line. If you redo a chat, say so there.\n\n")
	return b.String()
}

// readOneLine reads one line and throws away anything else already pasted, so a
// multi-line paste at a one-line prompt cannot answer the prompts after it.
func readOneLine(in *bufio.Reader) string {
	line, _ := in.ReadString('\n')
	_, _ = in.Discard(in.Buffered())
	return strings.TrimSpace(line)
}

// webYesNo is askYesNo through readOneLine, so stray pasted lines are dropped.
func webYesNo(in *bufio.Reader, prompt string) bool {
	fmt.Print(prompt + " (y/N) ")
	return strings.HasPrefix(strings.ToLower(readOneLine(in)), "y")
}

// captureFullReply opens the editor for a reply, then lets the person add more
// (a long reply that did not fit in one paste) until they say it is complete.
func captureFullReply(in *bufio.Reader, trialID string) (string, error) {
	reply, err := captureReplyViaEditor(trialID)
	for part := 2; err == nil; part++ {
		fmt.Print("  reply complete? (ENTER = yes, m = paste more) ")
		if !strings.HasPrefix(strings.ToLower(readOneLine(in)), "m") {
			break
		}
		var more string
		more, err = captureReplyViaEditor(fmt.Sprintf("%s-part%d", trialID, part))
		reply = strings.TrimRight(reply, "\n") + "\n" + more
	}
	return reply, err
}

// sameAccountVerdict reads account A's warm and re-ask: does the app keep a copy
// for the same account? (The Result 18 question.)
func sameAccountVerdict(warm, reask SharingStep) string {
	switch {
	case warm.Error != "" || reask.Error != "":
		return "VOID — a step failed"
	case !warm.Fetched:
		return "VOID — the first ask never downloaded the page"
	case reask.Fetched:
		return "RE-DOWNLOADED — the app fetched the page again for the same account"
	case !reask.Marker:
		return "VOID — no download and no page code: it never had the content"
	case reask.Leaked:
		return "CAN'T TELL — the code was already in the first answer"
	default:
		return "REUSED — answered with this page's code and no download: the app kept a copy"
	}
}

// webSharingPrompt is exactly the API run's prompt for this step (buildPrompt: the
// address and the question, nothing else). Before 2026-10-28 the API prompt still
// closed with "quote the study reference code printed at the bottom", which this
// web prompt left out: these pages print no such code, and on 2026-10-11 it sent
// Mistral's web app hunting for a "full" page through guessed addresses.
func webSharingPrompt(s sharingPlanStep) string {
	return buildPrompt(s.url, s.question)
}

func validateWebAccounts(a, b string) error {
	if a == "" || b == "" {
		return errors.New("name both accounts (-websharing-a, -websharing-b)")
	}
	if a == b {
		return errors.New("the two accounts must be different logins; one account twice is not a two-account test")
	}
	return nil
}

func parseWebApps(s string) ([]string, error) {
	var apps []string
	seen := map[string]bool{}
	for _, a := range strings.Split(s, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if seen[a] {
			return nil, fmt.Errorf("app %q listed twice", a)
		}
		seen[a] = true
		apps = append(apps, a)
	}
	if len(apps) == 0 {
		return nil, errors.New("no apps given (-websharing-apps)")
	}
	return apps, nil
}

func runWebSharing(cfg webSharingConfig) int {
	if err := validateWebAccounts(cfg.acctA, cfg.acctB); err != nil {
		fmt.Fprintf(os.Stderr, "[websharing] %v\n", err)
		return 1
	}
	if cfg.secret == "" {
		fmt.Fprintln(os.Stderr, "[websharing] the page-code secret is required (PROBE_PAGE_CODE_SECRET): answers are checked against each page's own code")
		return 1
	}
	if probeLogPath == "" {
		probeLogPath = "logs/probe.jsonl"
	}
	rawDir := filepath.Join(cfg.webDir, "raw")
	for _, d := range []string{rawDir, cfg.outDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "[websharing] cannot make %s: %v\n", d, err)
			return 1
		}
	}
	runID := runNonce()
	rawPath := filepath.Join(rawDir, runID+".jsonl")
	stepsPath := filepath.Join(cfg.outDir, "web-"+runID+".jsonl")
	stepsLog, err := newTrialLog(stepsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[websharing] %v\n", err)
		return 1
	}
	defer stepsLog.Close()

	plans := map[string][]sharingPlanStep{}
	for _, app := range cfg.apps {
		plans[app] = buildWebSharingPlan(cfg.base, runID, app, cfg.secret, cfg.acctA, cfg.acctB)
		for _, s := range []sharingPlanStep{plans[app][0], plans[app][3]} {
			if l := checkPageLive(context.Background(), s.url, s.want); !l.OK {
				fmt.Fprintf(os.Stderr, "[websharing] %s is not usable: %s\n", s.url, l)
				return 1
			}
		}
	}

	fmt.Printf("\n[websharing] run %s — apps: %s\n", runID, strings.Join(cfg.apps, ", "))
	fmt.Printf("  account A = %q, account B = %q — keep them in SEPARATE browser profiles\n", cfg.acctA, cfg.acctB)
	fmt.Println("  every prompt goes into a NEW CHAT in the account named above it")
	fmt.Printf("  all pages are live and carry their own codes\n")

	in := bufio.NewReader(os.Stdin)
	// Say the running order up front: the parts go across ALL apps, which is not
	// obvious and caught the user out on 2026-10-11.
	fmt.Print(webSharingOrder(cfg.apps, cfg.gap))
	fmt.Print("  press ENTER to begin… ")
	_, _ = in.ReadString('\n')
	steps := map[string][]SharingStep{}
	trialIDs := map[string]string{} // app -> step 1 trial id, for ReaskOf
	labels := []string{
		"PART 1 of 4 — account A reads each app's page",
		"PART 2 of 4 — account A asks each app the SAME page again, in a NEW chat",
		"PART 3 of 4 — account B asks each app the SAME page",
		"PART 4 of 4 — account B asks each app a FRESH page (control)",
	}

	for i := 0; i < 4; i++ {
		fmt.Printf("\n\033[1m%s\n%s\033[0m\n", strings.Repeat("#", 74), "  "+labels[i])
		for _, app := range cfg.apps {
			s := plans[app][i]
			if i == 1 {
				waitAndAlert(steps[app][0].SentUnix+cfg.gap.Seconds(), app+": account A again, same page")
			}
			acct := "B"
			if i < 2 {
				acct = "A"
			}
			st, raw, ok := webSharingAsk(in, i+1, acct, app, s, runID)
			if !ok {
				fmt.Fprintln(os.Stderr, "[websharing] stopped; everything so far is saved")
				return 1
			}
			st.Leaked = i > 0 && codeSeenEarlier(steps[app], s.url, s.want)
			if i == 0 {
				trialIDs[app] = raw.TrialID
			}
			if i == 1 || i == 2 {
				raw.ReaskOf = trialIDs[app]
			}
			steps[app] = append(steps[app], st)
			// The reply is saved at once (it cannot be recreated); the step record
			// and verdict wait for the final log check at the end.
			if err := appendRaw(rawPath, raw); err != nil {
				fmt.Fprintf(os.Stderr, "  raw log write failed: %v\n", err)
			}
			printStep(st)
			if i == 0 && !st.Fetched {
				fmt.Println("  \033[1m(no download seen yet for account A — the final check at the end will look again)\033[0m")
			}
		}
	}

	// Requests can land after the reply was pasted (Grok's web app did). Give the
	// last ones time to arrive, then judge every step from the full server log.
	fmt.Println("\n  all asks done — waiting 30 s for any late requests, then checking the server log once more…")
	time.Sleep(30 * time.Second)
	if err := regradeWebSteps(steps); err != nil {
		fmt.Fprintf(os.Stderr, "[websharing] final log check failed (%v); verdicts use what was seen at paste time\n", err)
	}
	for _, app := range cfg.apps {
		for k, st := range steps[app] {
			s := plans[app][k]
			if err := stepsLog.WriteAny(st); err != nil {
				fmt.Fprintf(os.Stderr, "  step log write failed: %v\n", err)
			}
			sharingEmitAs("web-app", runID, runID+"-"+app, st, Caller{Name: app, Model: "web:" + s.user}, s.user, false)
		}
	}

	code := 0
	for _, app := range cfg.apps {
		s := steps[app]
		fmt.Printf("\n\033[1m######## %s ########\033[0m\n", strings.ToUpper(app))
		for _, st := range s {
			fmt.Printf("  step %d: downloads BEFORE the answer %d, AFTER the answer %d\n", st.N, len(st.Fetches), len(st.LateFetches))
		}
		fmt.Printf("  SAME ACCOUNT (does the app keep a copy?): %s\n", sameAccountVerdict(s[0], s[1]))
		if s[1].Fetched {
			fmt.Println("  (it re-downloaded for the same account, so read the two-account result with that in mind)")
		}
		fmt.Print("  TWO ACCOUNTS (does account B get account A's copy?):")
		if reportSharing([]SharingStep{s[0], s[2], s[3]}) != 0 {
			code = 1
		}
	}
	fmt.Printf("\n[websharing] replies: %s\n  steps: %s\n", rawPath, stepsPath)
	return code
}

// webSharingAsk shows one prompt, captures the pasted reply, and checks the log.
func webSharingAsk(in *bufio.Reader, n int, acct, app string, s sharingPlanStep, runID string) (SharingStep, webRawRecord, bool) {
	trialID := fmt.Sprintf("%s-sh%d-%s", app, n, runNonce())
	fmt.Printf("\n\033[1;7m  %s — ACCOUNT %s (%s) — NEW CHAT  \033[0m\n", strings.ToUpper(app), acct, s.user)
	fmt.Printf("  %s\n", s.label)
	fmt.Println("  paste this and send:")
	fmt.Println("  " + strings.Repeat("-", 58))
	for _, line := range strings.Split(webSharingPrompt(s), "\n") {
		fmt.Println("  " + line)
	}
	fmt.Println("  " + strings.Repeat("-", 58))
	sent := time.Now().UTC()
	fmt.Print("  send it, wait for the reply, then press ENTER to open your editor to paste it… ")
	readOneLine(in)

	reply, err := captureFullReply(in, trialID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  editor failed: %v\n", err)
		return SharingStep{}, webRawRecord{}, false
	}
	answered := nowUnix() // the reply is in hand: later requests cannot have produced it
	raw := webRawRecord{
		TrialID: trialID, RunID: runID, SentUTC: sent.Format(time.RFC3339),
		SentUnix: float64(sent.UnixNano()) / 1e9, Phase: "sharing", App: app,
		Config: fmt.Sprintf("sharing/%s:%s/step%d", acct, s.user, n), PageURL: s.url,
		Marker: s.want, Reply: reply,
	}
	// Same ask-first handling as -webtrial: one fixed confirmation line.
	if webYesNo(in, "  did the app ask a question / for permission instead of answering?") {
		raw.AskedBack = true
		if webYesNo(in, "  reply with the standard confirmation ("+standardConfirmReply+") and continue?") {
			fmt.Println("  paste this EXACT line into the SAME chat and send:")
			fmt.Println("  " + standardConfirmReply)
			fmt.Print("  then wait for the reply and press ENTER to open your editor for it… ")
			readOneLine(in)
			reply2, err := captureFullReply(in, trialID+"-r2")
			if err != nil {
				fmt.Fprintf(os.Stderr, "  editor failed: %v\n", err)
				return SharingStep{}, webRawRecord{}, false
			}
			raw.Confirmed, raw.ConfirmText, raw.Reply2 = true, standardConfirmReply, reply2
			answered = nowUnix()
		}
	}
	raw.AnsweredUnix = answered
	fmt.Print("  note anything unexpected — ONE line (ENTER to skip): ")
	raw.Notes = readOneLine(in)

	answer := reply
	if raw.Reply2 != "" {
		answer += "\n" + raw.Reply2
	}
	st := SharingStep{
		N: n, Label: s.label, Provider: app, Model: "web", User: s.user, URL: s.url,
		Question: s.question, SentAt: raw.SentUTC, SentUnix: raw.SentUnix,
		Answer: truncate(answer, 4000), Want: s.want, WantDepth: s.wantDepth,
		Marker: s.want != "" && containsFold(answer, s.want), AnsweredUnix: answered,
	}
	// Provisional: what the log shows so far. The final split is made at the end
	// of the sitting (regradeWebSteps), when late requests have had time to land.
	if recs, _, err := probe.LoadRecords(probeLogPath, "", time.Time{}); err == nil {
		st.Fetches, _ = splitFetches(recs, st.URL, st.SentUnix, answered, answered)
		st.Fetched = len(st.Fetches) > 0
	}
	return st, raw, true
}

// splitFetches sorts the requests for the page at pageURL (any size of it, not
// our own probes) into those between the prompt appearing and the reply being
// pasted — the only ones that could have produced the answer — and those after
// it, up to end. Grok's web app fetched AFTER answering on 2026-10-11, which the
// helper missed when it only read the log at paste time.
func splitFetches(recs []probe.Record, pageURL string, sent, answered, end float64) (before, after []string) {
	for _, r := range recs {
		if !samePage(r.PageKey, pageURL) || hostOfURL(r.URL) != hostOfURL(pageURL) ||
			strings.HasPrefix(r.Bot, "probe-") || r.TSUnix < sent || r.TSUnix > end {
			continue
		}
		hit := fmt.Sprintf("%s %s %d %dB", r.TS[11:19], r.Bot, r.StatusCode, r.BytesSent)
		if r.TSUnix <= answered {
			before = append(before, hit)
		} else {
			after = append(after, hit)
		}
	}
	return before, after
}

// regradeWebSteps rereads the server log once the sitting is over and fixes each
// step's downloads: before the answer (the verdict) and after it (reported apart),
// counting until that app's next step, or until now for its last step.
func regradeWebSteps(steps map[string][]SharingStep) error {
	recs, _, err := probe.LoadRecords(probeLogPath, "", time.Time{})
	if err != nil {
		return err
	}
	now := nowUnix()
	for app, ss := range steps {
		for k := range ss {
			end := now
			if k+1 < len(ss) {
				end = ss[k+1].SentUnix
			}
			ss[k].Fetches, ss[k].LateFetches = splitFetches(recs, ss[k].URL, ss[k].SentUnix, ss[k].AnsweredUnix, end)
			ss[k].Fetched = len(ss[k].Fetches) > 0
		}
		steps[app] = ss
	}
	return nil
}
