package main

import (
	"bufio"
	"strings"
	"testing"
	"time"

	"aisearch-probe/internal/probe"
)

// The web prompts must be word for word what the API sharing run sends, on the
// same kind of page and graded on the same per-page codes — or web and API
// results could not be compared.
func TestWebSharingPlanMatchesTheAPIRun(t *testing.T) {
	const secret = "test-secret"
	p := buildWebSharingPlan("https://h.test/", "run1", "chatgpt", secret, "account-a", "account-b")
	if len(p) != 4 {
		t.Fatalf("want 4 steps (A warms, A re-asks, B asks, B control), got %d", len(p))
	}
	warm, reask, ask, ctl := p[0], p[1], p[2], p[3]

	// Warm, B's ask and the control are the API run's three steps, word for word.
	api := buildSharingPlan("https://h.test/", "run1", secret, Caller{}, Caller{}, "", "", time.Minute, time.Minute)
	for i, pair := range [][2]sharingPlanStep{{warm, api[0]}, {ask, api[1]}, {ctl, api[2]}} {
		if pair[0].question != pair[1].question || pair[0].wantDepth != pair[1].wantDepth {
			t.Errorf("pair %d: question/depth differ from the API run", i)
		}
	}
	// A's re-ask is the same sentence, naming a marker nobody else asks for.
	if reask.question != sharingAskQuestion(sharingReaskDepth) || reask.url != warm.url || reask.user != "account-a" {
		t.Errorf("A's re-ask must be account A, same page, depth=%d: %+v", sharingReaskDepth, reask)
	}
	seen := map[int]bool{}
	for _, s := range []sharingPlanStep{warm, reask, ask} {
		if seen[s.wantDepth] {
			t.Errorf("depth %d asked twice on the shared page; a saved answer could explain a silence", s.wantDepth)
		}
		seen[s.wantDepth] = true
	}
	for i, s := range p {
		// The web prompt is exactly the API prompt (address + question), and never
		// asks for a "study reference code": these pages print none, and on
		// 2026-10-11 that line sent Mistral hunting for a "full" page.
		got := webSharingPrompt(s)
		if got != buildPrompt(s.url, s.question) {
			t.Errorf("step %d: web prompt must be the API prompt:\n%q", i+1, got)
		}
		if strings.Contains(strings.ToLower(got), "reference code") {
			t.Errorf("step %d: web prompt still asks for the reference code: %q", i+1, got)
		}
		if !strings.Contains(got, s.url) || !strings.Contains(got, s.question) {
			t.Errorf("step %d: web prompt lost the address or the question: %q", i+1, got)
		}
		if !strings.Contains(s.url, "/depth/html/") || !strings.HasSuffix(s.url, "/2048") {
			t.Errorf("step %d: not the 2 KB depth page: %s", i+1, s.url)
		}
		if s.want != probe.DepthCode(secret, s.id, s.wantDepth, probe.DepthPageRevision) || s.want == "" {
			t.Errorf("step %d: not graded on this page's own code", i+1)
		}
	}
	if warm.url != ask.url || ctl.url == ask.url {
		t.Error("A and B share one page; the control is a different page")
	}
	if warm.user != "account-a" || ask.user != "account-b" || ctl.user != "account-b" {
		t.Errorf("accounts: warm %q ask %q control %q", warm.user, ask.user, ctl.user)
	}
	// Each app gets its own pages, so two apps in one sitting cannot share a copy.
	other := buildWebSharingPlan("https://h.test/", "run1", "claude", secret, "account-a", "account-b")
	if other[0].url == warm.url || other[3].url == ctl.url {
		t.Error("two apps in one sitting must get different pages")
	}
}

// Every app gets all four steps (user, 2026-10-11): no step is skipped on what an
// earlier step showed, so every app's data has the same shape.
func TestWebSharingAlwaysRunsAllFourSteps(t *testing.T) {
	order := webSharingOrder([]string{"grok"}, time.Minute)
	if strings.Contains(order, "skipped") || strings.Contains(strings.ToLower(order), "only") {
		t.Errorf("the running order must not say any step is skipped:\n%s", order)
	}
	for _, app := range []string{"grok", "metaai", "claude"} {
		if n := len(buildWebSharingPlan("https://h.test", "r", app, "s", "a", "b")); n != 4 {
			t.Errorf("%s: %d steps, want 4", app, n)
		}
	}
}

// A one-line prompt must swallow any extra pasted lines, so they cannot answer
// the next prompts (2026-10-11: the rest of Qwen's reply, pasted at its notes
// prompt, answered Meta AI's prompts and became Meta AI's "reply").
func TestReadOneLineDiscardsTheRestOfAPaste(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("first line\nspill one\nspill two\n"))
	if got := readOneLine(in); got != "first line" {
		t.Errorf("got %q", got)
	}
	if rest, _ := in.ReadString('\n'); rest != "" {
		t.Errorf("leftover pasted text reached the next prompt: %q", rest)
	}
}

func TestSameAccountVerdict(t *testing.T) {
	warm := SharingStep{Fetched: true, Marker: true}
	cases := []struct {
		name        string
		warm, reask SharingStep
		want, notIn string
	}{
		{"reused", warm, SharingStep{Fetched: false, Marker: true}, "REUSED", "RE-DOWNLOADED"},
		{"re-downloaded", warm, SharingStep{Fetched: true, Marker: true}, "RE-DOWNLOADED", "REUSED"},
		{"code already in the warm answer", warm, SharingStep{Fetched: false, Marker: true, Leaked: true}, "CAN'T TELL", "REUSED"},
		{"no download and no code", warm, SharingStep{Fetched: false, Marker: false}, "VOID", "REUSED"},
		{"warm never downloaded", SharingStep{Fetched: false, Marker: true}, SharingStep{Fetched: false, Marker: true}, "VOID", "REUSED"},
		{"a call failed", warm, SharingStep{Error: "x"}, "VOID", "REUSED"},
	}
	for _, c := range cases {
		got := sameAccountVerdict(c.warm, c.reask)
		if !strings.Contains(got, c.want) || strings.Contains(got, c.notIn) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWebSharingRefusesOneAccountTwice(t *testing.T) {
	if err := validateWebAccounts("account-a", "account-a"); err == nil {
		t.Error("the same account twice is not a two-account test")
	}
	if err := validateWebAccounts("account-a", ""); err == nil {
		t.Error("both accounts must be named")
	}
	if err := validateWebAccounts("account-a", "account-b"); err != nil {
		t.Errorf("two named accounts must pass: %v", err)
	}
}

func TestParseWebApps(t *testing.T) {
	apps, err := parseWebApps(" chatgpt, claude ,grok ")
	if err != nil || strings.Join(apps, ",") != "chatgpt,claude,grok" {
		t.Errorf("got %v %v", apps, err)
	}
	if _, err := parseWebApps("chatgpt,chatgpt"); err == nil {
		t.Error("an app listed twice must be refused")
	}
	if _, err := parseWebApps(""); err == nil {
		t.Error("no apps must be refused")
	}
}

// The web run has no late re-ask, so the report must give a verdict on three steps.
func TestReportSharingThreeSteps(t *testing.T) {
	out, code := capture([]SharingStep{mk(1, "chatgpt", true, true), mk(2, "chatgpt", false, true), mk(3, "chatgpt", true, true)})
	if code != 0 || !strings.Contains(out, "SHARED —") {
		t.Errorf("three clean steps must read SHARED; got %d:\n%s", code, out)
	}
	if strings.Contains(out, "long gap") {
		t.Errorf("no step 4 was run, so nothing may be said about it:\n%s", out)
	}
	out, code = capture([]SharingStep{mk(1, "chatgpt", true, true), mk(2, "chatgpt", false, true)})
	if code == 0 || !strings.Contains(out, "incomplete") {
		t.Error("two steps is incomplete")
	}
}

// A request for ANY address under the page's id is a download of that page: the
// other sizes carry the same codes (Mistral fetched /4096 … /1048576 on
// 2026-10-11). The control page has a different id and must not match.
func TestSamePageCountsEverySizeOfThePage(t *testing.T) {
	const u = "https://h.test/depth/html/sh-run1-mistral/2048"
	for _, p := range []string{"/depth/html/sh-run1-mistral/2048", "/depth/html/sh-run1-mistral/4096",
		"/depth/html/sh-run1-mistral/2048.html", "/depth/html/sh-run1-mistral/", "/depth/html/sh-run1-mistral"} {
		if !samePage(p, u) {
			t.Errorf("%s must count as the same page", p)
		}
	}
	for _, p := range []string{"/depth/html/sh-run1-mistral-ctl/2048", "/depth/html/sh-run1-claude/2048", "/favicon.ico"} {
		if samePage(p, u) {
			t.Errorf("%s must NOT count as the same page", p)
		}
	}
}

// Requests are split at the moment the reply was pasted: only those BEFORE it
// could have given the answer; those after (Grok's web app fetched after
// answering on 2026-10-11) are reported apart. Other pages, other hosts and our
// own probes never count; any size of the page does.
func TestSplitFetchesAtTheAnswer(t *testing.T) {
	const u = "https://h.test/depth/html/sh-r-grok/2048"
	rec := func(ts float64, path, bot string) probe.Record {
		return probe.Record{TSUnix: ts, TS: "2026-10-11T00:00:00Z", URL: "https://h.test" + path, PageKey: path, Bot: bot, StatusCode: 200}
	}
	recs := []probe.Record{
		rec(99, "/depth/html/sh-r-grok/2048", "unknown"),         // before the prompt: not this step
		rec(105, "/depth/html/sh-r-grok/2048", "unknown"),        // before the answer
		rec(106, "/depth/html/sh-r-grok/4096", "unknown"),        // another size: still this page
		rec(107, "/depth/html/sh-r-grok/2048", "probe-liveness"), // our own probe
		rec(108, "/depth/html/sh-r-grok-ctl/2048", "unknown"),    // the control page
		rec(130, "/depth/html/sh-r-grok/2048", "unknown"),        // after the answer
		rec(300, "/depth/html/sh-r-grok/2048", "unknown"),        // after the window
	}
	before, after := splitFetches(recs, u, 100, 120, 200)
	if len(before) != 2 || len(after) != 1 {
		t.Errorf("before %v, after %v; want 2 and 1", before, after)
	}
}

// The running order must be shown up front and say the parts go across all apps.
func TestWebSharingOrderExplainsTheBatching(t *testing.T) {
	s := webSharingOrder([]string{"claude", "chatgpt"}, 5*time.Minute)
	for _, want := range []string{"ALL 2 apps", "Part 1", "Part 2", "Part 3", "Part 4", "claude, chatgpt", "5m0s", "NEW chat"} {
		if !strings.Contains(s, want) {
			t.Errorf("running order lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "\033[3") || strings.Contains(s, "\033[1;3") {
		t.Error("no foreground colours: they are unreadable on a white terminal")
	}
}
