package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aisearch-probe/internal/probe"
)

// capture runs reportSharing and returns what it printed plus the exit code.
func capture(steps []SharingStep) (string, int) {
	var b strings.Builder
	old := stdout
	stdout = &b
	defer func() { stdout = old }()
	code := reportSharing(steps)
	return b.String(), code
}

func mk(n int, prov string, fetched, marker bool) SharingStep {
	return SharingStep{N: n, Provider: prov, Fetched: fetched, Marker: marker,
		Answer: "three categories: first sight, return unstamped, judgeable"}
}

// The verdict that would be a real finding, and the three that must never be
// mistaken for it.
func TestSharingVerdicts(t *testing.T) {
	cases := []struct {
		name            string
		s1, s2, s3, s4  SharingStep
		wantPhrase      string
		wantNotPhrase   string
		wantNonZeroExit bool
	}{
		{
			name:       "shared and durable — the finding",
			s1:         mk(1, "openai", true, true),
			s2:         mk(2, "or:openai", false, true),
			s3:         mk(3, "or:openai", true, true),
			s4:         mk(4, "or:openai", false, true),
			wantPhrase: "SHARED",
		},
		{
			name:          "second account downloaded — not shared",
			s1:            mk(1, "openai", true, true),
			s2:            mk(2, "or:openai", true, true),
			s3:            mk(3, "or:openai", true, true),
			s4:            mk(4, "or:openai", true, true),
			wantPhrase:    "NOT SHARED",
			wantNotPhrase: "SHARED — the second account answered",
		},
		{
			// The dangerous one: silence plus no page content is not sharing.
			name: "silent but never had the page — void, not a finding",
			s1:   mk(1, "openai", true, true),
			s2: SharingStep{N: 2, Provider: "or:openai", Fetched: false, Marker: false,
				Answer: "I could not access that page."},
			s3:              mk(3, "or:openai", true, true),
			s4:              mk(4, "or:openai", false, false),
			wantPhrase:      "VOID",
			wantNotPhrase:   "SHARED —",
			wantNonZeroExit: true,
		},
		{
			// The control is what separates "reused" from "wasn't fetching".
			name:            "control failed — void",
			s1:              mk(1, "openai", true, true),
			s2:              mk(2, "or:openai", false, true),
			s3:              mk(3, "or:openai", false, true),
			s4:              mk(4, "or:openai", false, true),
			wantPhrase:      "control failed",
			wantNotPhrase:   "SHARED —",
			wantNonZeroExit: true,
		},
		{
			name:            "first account never downloaded — void",
			s1:              mk(1, "openai", false, true),
			s2:              mk(2, "or:openai", false, true),
			s3:              mk(3, "or:openai", true, true),
			s4:              mk(4, "or:openai", false, true),
			wantPhrase:      "never downloaded",
			wantNotPhrase:   "SHARED —",
			wantNonZeroExit: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, code := capture([]SharingStep{c.s1, c.s2, c.s3, c.s4})
			if !strings.Contains(out, c.wantPhrase) {
				t.Errorf("expected %q in the report; got:\n%s", c.wantPhrase, out)
			}
			if c.wantNotPhrase != "" && strings.Contains(out, c.wantNotPhrase) {
				t.Errorf("report must NOT contain %q; got:\n%s", c.wantNotPhrase, out)
			}
			if (code != 0) != c.wantNonZeroExit {
				t.Errorf("exit code %d, wantNonZero=%v", code, c.wantNonZeroExit)
			}
		})
	}
}

// A page that has already been fetched makes the experiment meaningless, so it
// must be refused — but our own instrument's requests must not count as pollution.
func TestPriorFetchesIgnoresOurOwnInstrument(t *testing.T) {
	recs := []probe.Record{
		{PageKey: "/method.html", URL: "https://h.test/method.html", Bot: "probe-liveness"},
		{PageKey: "/method.html", URL: "https://h.test/method.html", Bot: "probe-selfcheck"},
	}
	if n, _ := priorFetches(recs, "https://h.test/method.html"); n != 0 {
		t.Errorf("our own probe requests must not count as pollution, got %d", n)
	}

	recs = append(recs, probe.Record{
		PageKey: "/method.html", URL: "https://h.test/method.html", Bot: "openai-query"})
	n, who := priorFetches(recs, "https://h.test/method.html")
	if n != 1 || !strings.Contains(who, "openai-query") {
		t.Errorf("a real earlier fetch must be reported, got %d from %q", n, who)
	}

	// A different host is a different page as far as any cache is concerned.
	if n, _ := priorFetches(recs, "https://other.test/method.html"); n != 0 {
		t.Errorf("a fetch on another address must not count, got %d", n)
	}
}

// The dialler must substitute only the ADDRESS, leaving the Host header and
// certificate checking alone — the same contract as curl --resolve. Tested with
// a hostname that does not exist anywhere, pinned at a local server: if the
// substitution works the request arrives; if it leaked into a real lookup it
// cannot.
func TestResolveOverridePinsOnlyTheAddress(t *testing.T) {
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		fmt.Fprint(w, "served")
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	const fake = "does-not-exist-anywhere.invalid"
	resolveOverrides = map[string]string{}
	if err := addResolveOverride(fake + ":127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	defer func() { resolveOverrides = map[string]string{} }()

	url := "http://" + fake + ":" + port + "/x"
	resp, err := probeHTTPClient(10 * time.Second).Get(url)
	if err != nil {
		t.Fatalf("the pinned address was not used: %v", err)
	}
	resp.Body.Close()

	// The server must still have been told the original name: a cache keys on
	// the hostname, so rewriting it would change what we are measuring.
	if !strings.HasPrefix(gotHost, fake) {
		t.Errorf("Host header was rewritten to %q; it must stay %q", gotHost, fake)
	}
}

func TestResolveOverrideRejectsNonsense(t *testing.T) {
	for _, bad := range []string{"", "hostonly", "host:notanip", ":1.2.3.4"} {
		if err := addResolveOverride(bad); err == nil {
			t.Errorf("addResolveOverride(%q) should have failed", bad)
		}
	}
}

// All three questions are the cache-control run's sentence frame, each naming
// one marker's location — the warm one the bottom (95%) marker, the second
// caller's the middle ones. Naming the location stops the warm answer listing
// every code (the vague "study reference code" made Claude list all five on the
// 2026-10-11 smoke run, so the try could not be read).
func TestSharingQuestionsMatchTheStandardWording(t *testing.T) {
	deepest := probe.DepthPercents[len(probe.DepthPercents)-1]
	if sharingWarmQuestion != sharingAskQuestion(deepest) {
		t.Fatalf("warm question must name the bottom marker's location: %q", sharingWarmQuestion)
	}
	if !strings.Contains(sharingWarmQuestion, "depth=95%") {
		t.Errorf("warm question must name depth=95%%: %q", sharingWarmQuestion)
	}
	ask, late := sharingAskQuestion(sharingAskDepth), sharingAskQuestion(sharingLateDepth)
	const head, tail = "Fetch this document and tell me its id and ", " in it. Quote them exactly."
	for _, q := range []string{ttlmapQuestion, sharingWarmQuestion, ask, late} {
		if !strings.HasPrefix(q, head) || !strings.HasSuffix(q, tail) {
			t.Errorf("question leaves the standard frame: %q", q)
		}
	}
	if ask == sharingWarmQuestion || late == sharingWarmQuestion || ask == late {
		t.Error("each step must ask a question no earlier step asked, or a saved answer could explain a silence")
	}
	// The asked-for depths must be middle markers: the standard answer usually
	// quotes the top and the bottom code already (measured on the 2026-10-07 run).
	for _, d := range []int{sharingAskDepth, sharingLateDepth} {
		if d == probe.DepthPercents[0] || d == probe.DepthPercents[len(probe.DepthPercents)-1] {
			t.Errorf("depth %d is the top or bottom marker; the warm answer usually already quotes it", d)
		}
	}
}

// Every step is matched against THIS page's own recomputed code — never a shared
// string — and the control step is always there, on a different fresh page.
func TestSharingPlanUsesPerPageCodesAndAlwaysHasAControl(t *testing.T) {
	const secret = "test-secret"
	a := Caller{Name: "openai", Model: "gpt-5-mini"}
	b := Caller{Name: "or:openai", Model: "openai/gpt-5-mini"}
	plan := buildSharingPlan("https://h.test/", "run1", secret, a, b, "", "", 5*time.Minute, 10*time.Minute)
	if len(plan) != 4 {
		t.Fatalf("want 4 steps (warm, ask, control, late), got %d", len(plan))
	}
	warm, ask, ctl, late := plan[0], plan[1], plan[2], plan[3]

	for _, s := range plan {
		if !strings.HasPrefix(s.url, "https://h.test/depthhtmlpage/") || !strings.HasSuffix(s.url, "/2048") {
			t.Errorf("%s: not the 2 KB depth page: %s", s.label, s.url)
		}
		want := probe.DepthCode(secret, s.id, s.wantDepth, probe.DepthPageRevision)
		if s.want == "" || s.want != want {
			t.Errorf("%s: matches %q, want this page's own code %q", s.label, s.want, want)
		}
		if strings.Contains(s.want, "RCP") {
			t.Errorf("%s: matches the shared RCP marker", s.label)
		}
	}
	if warm.url != ask.url || ask.url != late.url {
		t.Error("warm, ask and late must all be the SAME shared page")
	}
	if ctl.url == ask.url || ctl.id == ask.id {
		t.Error("the control must be a different, untouched page")
	}
	if warm.caller.Tag() != a.Tag() || ask.caller.Tag() != b.Tag() || ctl.caller.Tag() != b.Tag() || late.caller.Tag() != b.Tag() {
		t.Error("caller A warms; caller B asks, controls and re-asks")
	}
	if ctl.question != ask.question {
		t.Error("the control asks the same question as the measurement, so only the page differs")
	}
	if warm.wantDepth != probe.DepthPercents[len(probe.DepthPercents)-1] {
		t.Error("the warm step is graded like the cache-control run: on the deepest code")
	}
	if ask.wait != 5*time.Minute || late.wait != 10*time.Minute {
		t.Errorf("waits: ask %s late %s", ask.wait, late.wait)
	}
	// A second run gets brand-new pages.
	again := buildSharingPlan("https://h.test", "run2", secret, a, b, "", "", time.Minute, time.Minute)
	if again[0].url == warm.url || again[2].url == ctl.url {
		t.Error("a new run id must give new pages")
	}
}

// If the code B was asked for already appeared in an earlier answer about the same
// page, a saved answer could explain B's silence — that try cannot count.
func TestCodeSeenEarlier(t *testing.T) {
	prev := []SharingStep{
		{URL: "https://h.test/depthhtmlpage/x/2048", Answer: "id: TRUNC-aaaa, ref: TRUNC-bbbb"},
		{URL: "https://h.test/depthhtmlpage/ctl/2048", Answer: "TRUNC-cccc"},
	}
	if !codeSeenEarlier(prev, "https://h.test/depthhtmlpage/x/2048", "trunc-AAAA") {
		t.Error("a code already quoted for this page must be caught (case-insensitive)")
	}
	if codeSeenEarlier(prev, "https://h.test/depthhtmlpage/x/2048", "TRUNC-cccc") {
		t.Error("a code quoted for a DIFFERENT page does not count")
	}
	if codeSeenEarlier(prev, "https://h.test/depthhtmlpage/x/2048", "") {
		t.Error("an empty code must never match")
	}
}

func TestSharingVerdictCantTellWhenTheCodeLeaked(t *testing.T) {
	s2 := mk(2, "or:openai", false, true)
	s2.Leaked = true
	out, code := capture([]SharingStep{
		mk(1, "openai", true, true), s2, mk(3, "or:openai", true, true), mk(4, "or:openai", false, true)})
	if !strings.Contains(out, "CAN'T TELL") || strings.Contains(out, "SHARED —") {
		t.Errorf("a leaked code must read CAN'T TELL, never SHARED; got:\n%s", out)
	}
	if code == 0 {
		t.Error("CAN'T TELL is not a result; exit non-zero")
	}
}

// The user asked (2026-10-11) whether a reused copy bills fewer input tokens.
// Step 2 (shared page) and step 3 (fresh control) are the same caller, question
// and page size, so their input tokens compare directly. The report prints both
// numbers as facts, whatever the verdict.
func TestReportShowsInputTokensOfMeasurementAndControl(t *testing.T) {
	s2 := mk(2, "or:openai", false, true)
	s2.Usage.InputTokens = 4321
	s3 := mk(3, "or:openai", true, true)
	s3.Usage.InputTokens = 8765
	out, _ := capture([]SharingStep{mk(1, "openai", true, true), s2, s3, mk(4, "or:openai", false, true)})
	if !strings.Contains(out, "4321") || !strings.Contains(out, "8765") {
		t.Errorf("report must print the input tokens of steps 2 and 3; got:\n%s", out)
	}
}

// The same caller twice is only allowed as the explicit within-account baseline,
// and the baseline must really be the same caller.
func TestValidateSharingPair(t *testing.T) {
	a := Caller{Name: "or:openai", Model: "openai/gpt-5-mini"}
	b := Caller{Name: "openai", Model: "gpt-5-mini"}
	if validateSharingPair(a, a, "", "", false) == nil {
		t.Error("identical callers without -sharingApi-run-sameCaller must be refused")
	}
	if err := validateSharingPair(a, a, "", "", true); err != nil {
		t.Errorf("baseline with the same caller must pass: %v", err)
	}
	if validateSharingPair(a, b, "", "", true) == nil {
		t.Error("-sharingApi-run-sameCaller with two different callers is a contradiction")
	}
	if validateSharingPair(a, a, "alice", "bob", true) == nil {
		t.Error("-sharingApi-run-sameCaller with two different users is a contradiction")
	}
	if err := validateSharingPair(a, b, "", "", false); err != nil {
		t.Errorf("two different callers must pass: %v", err)
	}
	if err := validateSharingPair(a, a, "alice", "bob", false); err != nil {
		t.Errorf("one caller, two users must pass: %v", err)
	}
}

// Comparisons run side by side as separate processes, so each needs its own lock —
// and the same comparison twice must collide.
func TestSharingLockPathPerComparison(t *testing.T) {
	a := Caller{Name: "or:openai", Model: "openai/gpt-5.5"}
	b := Caller{Name: "or:openai", Model: "openai/gpt-5-mini"}
	p1 := sharingLockPath(a, b, "", "")
	if p1 != sharingLockPath(a, b, "", "") {
		t.Error("same comparison must give the same lock")
	}
	if p1 == sharingLockPath(b, b, "", "") || p1 == sharingLockPath(b, b, "alice", "bob") {
		t.Error("different comparisons must not share a lock")
	}
	if !strings.HasPrefix(p1, "logs/.sharing-") || strings.ContainsAny(strings.TrimPrefix(p1, "logs/"), "/:") {
		t.Errorf("lock path must be a plain file under logs/: %s", p1)
	}
}

// The pause before the control (-sharingApi-controlAskGap) moves only the control step;
// zero leaves the plan exactly as before. Added 2026-10-31: Gemini's free tier
// answered 503 to the control, asked 20 s after step 2, in both of its tries.
func TestWithControlGapMovesOnlyTheControl(t *testing.T) {
	a := Caller{Name: "google", Model: "gemini-2.5-flash"}
	plan := buildSharingPlan("https://h.test", "r", "s", a, a, "", "", 5*time.Minute, 10*time.Minute)
	same := withControlGap(append([]sharingPlanStep(nil), plan...), 0)
	for i := range plan {
		if same[i].wait != plan[i].wait {
			t.Errorf("gap 0 changed step %d", i+1)
		}
	}
	got := withControlGap(append([]sharingPlanStep(nil), plan...), 2*time.Minute)
	for i := range plan {
		want := plan[i].wait
		if i == 2 {
			want = 2 * time.Minute
		}
		if got[i].wait != want {
			t.Errorf("step %d wait %s, want %s", i+1, got[i].wait, want)
		}
	}
}

// Every step keeps the provider's own report of the fetch (Gemini's
// urlRetrievalStatus, via probe.Usage.FetchStatus) — on a failed call too, where it
// matters most: a refusal and a reused copy can both mean "no download".
func TestSharingStepKeepsProviderFetchStatus(t *testing.T) {
	p := Caller{Name: "google", Model: "gemini-2.5-flash"}
	const url = "https://h.test/depthhtmlpage/x/2048"
	ok := probe.Usage{FetchStatus: "URL_RETRIEVAL_STATUS_SUCCESS " + url}
	st := sharingStepFromAsk(2, "2: ask", p, url, "q", "TRUNC-abc", time.Unix(100, 0), "TRUNC-abc", ok, nil)
	if st.FetchStatus != ok.FetchStatus || !st.Marker || st.Error != "" {
		t.Errorf("fetch status %q marker %v error %q", st.FetchStatus, st.Marker, st.Error)
	}
	bad := probe.Usage{FetchStatus: "URL_RETRIEVAL_STATUS_ERROR " + url}
	st = sharingStepFromAsk(1, "1: warm", p, url, "q", "TRUNC-abc", time.Unix(100, 0), "", bad, fmt.Errorf("boom"))
	if st.FetchStatus != bad.FetchStatus || st.Error != "boom" {
		t.Errorf("a failed call must keep its fetch status and error: %+v", st)
	}
}
