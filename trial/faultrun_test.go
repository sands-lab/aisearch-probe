package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aisearch-probe/internal/probe"
)

// The verdict must name the two departures that matter, and must never claim
// more than the counts show.
func TestFaultVerdict(t *testing.T) {
	perm := FaultSpec{Name: "404", Kind: "permanent", ShouldRetry: false}
	transient := FaultSpec{Name: "500", Kind: "transient", ShouldRetry: true}
	recovery := FaultSpec{Name: "flaky/2", Kind: "recovery", ShouldRetry: true}

	cases := []struct {
		name   string
		fault  FaultSpec
		r      FaultResult
		expect string // substring the verdict must contain
	}{
		{"retried a permanent error",
			perm, FaultResult{Fetches: 4, ShouldRetry: false}, "SHOULD NOT"},
		{"retried a transient error, allowed",
			transient, FaultResult{Fetches: 3, ShouldRetry: true, SpanSeconds: 12}, "retried 3 times"},
		{"fetched once, no retry",
			perm, FaultResult{Fetches: 1}, "did not retry"},
		{"nothing arrived",
			transient, FaultResult{Fetches: 0}, "no fetch reached"},
		{"nothing arrived and errored",
			transient, FaultResult{Fetches: 0, Error: "boom"}, "void"},
		// Recovery is read from the depth codes, never from the answer's wording:
		// "recovered" in prose (or the shared RCP code) proves nothing.
		{"recovered",
			recovery, FaultResult{Fetches: 3, DeepestDepth: 95}, "recovered"},
		{"did not recover",
			recovery, FaultResult{Fetches: 1, DeepestDepth: -1, Answer: "fault: flaky recovered RCP-2026-08"}, "did not retry enough"},
		{"amplification noted",
			perm, FaultResult{Fetches: 2, ShouldRetry: false, OffPathHits: 1}, "amplification"},
	}
	for _, c := range cases {
		got := faultVerdict(c.fault, c.r)
		if !strings.Contains(got, c.expect) {
			t.Errorf("%s: verdict %q, want it to contain %q", c.name, got, c.expect)
		}
	}
}

// The battery must match the server's fault paths, or the driver asks for pages
// the server does not serve.
func TestFaultBatteryNamesRealPaths(t *testing.T) {
	known := map[string]bool{
		"404": true, "500": true, "503": true, "slow": true,
		"empty": true, "reset": true,
		"429": true, "429date": true, "403": true, "loop/a": true, "cut": true,
	}
	for _, f := range faultBattery {
		base := f.Name
		if i := strings.Index(base, "/"); i >= 0 {
			base = base[:i] // "flaky/2" -> "flaky"
		}
		if base == "flaky" {
			continue // parameterised, checked by the server tests
		}
		if !known[f.Name] {
			t.Errorf("battery has %q, which the server does not serve", f.Name)
		}
	}
}

// The run nonce must make a repeat run use fresh addresses — otherwise a caching
// provider serves the prior run's copy and the flaky counter never resets. Two
// nonces must differ, and both must be non-empty and query-safe.
func TestRunNonceIsFreshEachRun(t *testing.T) {
	a, b := runNonce(), runNonce()
	if a == "" || b == "" {
		t.Fatal("runNonce returned empty")
	}
	if a == b {
		t.Errorf("two run nonces are identical (%q) — a repeat run would reuse URLs", a)
	}
	// Folded into a token, it must survive tokenisation unchanged in shape
	// (letters, digits, dashes only) so it stays attributable.
	for _, c := range a {
		ok := c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z')
		if !ok {
			t.Errorf("nonce %q has a char that will not survive the URL token: %q", a, c)
		}
	}
}

// Two runs (different run ids) must produce different fault tokens for the same
// provider — the whole point of the fix.
func TestTwoRunsGetDifferentFaultTokens(t *testing.T) {
	p := Caller{Name: "or:anthropic", Model: "anthropic/claude-sonnet-5"}
	_, tok1 := probeURLFor("https://x/fault/500", p.Tag()+"-run-A")
	_, tok2 := probeURLFor("https://x/fault/500", p.Tag()+"-run-B")
	if tok1 == tok2 {
		t.Errorf("both runs got token %q — reused address, the bug this fixes", tok1)
	}
}

// --- the 2026-10-08 additions ------------------------------------------------

// The battery carries every new fault with the right "should a good client
// retry" label, so the verdict judges each one against the right standard.
func TestFaultBatteryHasTheNewFaults(t *testing.T) {
	want := map[string]bool{"429": true, "429date": true, "403": false, "loop/a": false, "cut": true}
	got := map[string]FaultSpec{}
	for _, f := range faultBattery {
		got[f.Name] = f
	}
	for name, retry := range want {
		f, ok := got[name]
		if !ok {
			t.Errorf("battery is missing %q", name)
			continue
		}
		if f.ShouldRetry != retry {
			t.Errorf("%s: ShouldRetry = %v, want %v", name, f.ShouldRetry, retry)
		}
	}
}

// The server keys depth markers on the ?probe= VALUE; the runner's token is the
// whole "probe=…" pair. Reading with the wrong id would score every answer -1.
func TestFaultDepthIDIsTheProbeValue(t *testing.T) {
	if got := faultDepthID("probe=or-x-ai-grok-abc"); got != "or-x-ai-grok-abc" {
		t.Errorf("faultDepthID = %q, want the value after probe=", got)
	}
}

// The cut verdict reads the deepest marker the model quoted, and never claims a
// partial-page finding without saying it rests on the tunnel check.
func TestFaultVerdictReadsTheCut(t *testing.T) {
	cut := FaultSpec{Name: "cut", Kind: "truncated", ShouldRetry: true}
	cases := []struct {
		name   string
		r      FaultResult
		expect []string
	}{
		{"handed the partial page", FaultResult{Fetches: 1, DeepestDepth: 25}, []string{"25%", "partial", "tunnel"}},
		{"got the whole page", FaultResult{Fetches: 2, DeepestDepth: 95}, []string{"whole page"}},
		{"quoted nothing", FaultResult{Fetches: 1, DeepestDepth: -1}, []string{"no depth marker"}},
	}
	for _, c := range cases {
		got := faultVerdict(cut, c.r)
		for _, e := range c.expect {
			if !strings.Contains(got, e) {
				t.Errorf("%s: verdict %q, want it to contain %q", c.name, got, e)
			}
		}
	}
}

// A redirect loop's arrivals are hops, not retries — the verdict counts hops and
// must not call following a redirect a "retry it should not have made".
func TestFaultVerdictCountsLoopHops(t *testing.T) {
	loop := FaultSpec{Name: "loop/a", Kind: "redirect", ShouldRetry: false}
	got := faultVerdict(loop, FaultResult{Fetches: 7})
	if !strings.Contains(got, "7 hops") || strings.Contains(got, "SHOULD NOT") {
		t.Errorf("loop verdict %q, want a hop count and no retry accusation", got)
	}
	capped := faultVerdict(loop, FaultResult{Fetches: 101})
	if !strings.Contains(capped, "hop cap") {
		t.Errorf("loop verdict at the cap %q, want it to say the server's hop cap stopped it", capped)
	}
}

// On 403 several user agents are reported, but NOT as "came back as someone
// else": run 1 showed xAI uses the same two browser agents on every fault and
// sends its requests together, before any 403 could arrive. The verdict names
// the count and says to compare with the other faults.
func TestFaultVerdict403ReportsAgentsWithoutClaimingEvasion(t *testing.T) {
	f403 := FaultSpec{Name: "403", Kind: "permanent", ShouldRetry: false}
	got := faultVerdict(f403, FaultResult{Fetches: 2, UserAgents: []string{"BotA/1", "Mozilla/5.0 Chrome"}})
	if !strings.Contains(got, "2 user agents") || !strings.Contains(got, "other faults") {
		t.Errorf("verdict %q, want the agent count and the compare-with-other-faults note", got)
	}
	if strings.Contains(got, "came back") || strings.Contains(got, "different user agent") {
		t.Errorf("verdict %q claims a reaction to the 403 the data cannot show", got)
	}
	same := faultVerdict(f403, FaultResult{Fetches: 1, UserAgents: []string{"BotA/1"}})
	if strings.Contains(same, "user agents") {
		t.Errorf("verdict %q mentions agents with only one", same)
	}
}

// The question asks for the TRUNC- depth codes in the page-size experiment's own
// words, with "has" softened to "may have": most fault pages carry no codes, and
// promising codes that are not there could make a model call its fetch tool
// again — inflating the very count being measured.
func TestFaultQuestionAsksForDepthCodes(t *testing.T) {
	q := faultQuestion(FaultSpec{Name: "404"})
	for _, want := range []string{"What does this page say?", "may have marker codes that begin with TRUNC-",
		"Quote every TRUNC- code you can see on the page, exactly as written."} {
		if !strings.Contains(q, want) {
			t.Errorf("question %q is missing %q", q, want)
		}
	}
	if strings.Contains(q, "has several") || strings.Contains(q, "RCP") || strings.Contains(q, "study reference") {
		t.Errorf("question %q promises codes or asks for the shared RCP code", q)
	}
}

// A slow page that the fetcher gave up on and asked again is a timeout, not a
// mistake: the verdict says so, and neither it nor a redirect loop's hops land
// on the "retried a failure it should have left alone" list.
func TestTimeoutsAndHopsAreNotListedAsMistakes(t *testing.T) {
	slow := FaultSpec{Name: "slow", Kind: "timing", ShouldRetry: false}
	if v := faultVerdict(slow, FaultResult{Fetches: 3, SpanSeconds: 13}); !strings.Contains(v, "timed out") || strings.Contains(v, "SHOULD NOT") {
		t.Errorf("slow verdict %q, want a timeout reading and no accusation", v)
	}
	results := []FaultResult{
		{Fault: "slow", Kind: "timing", Fetches: 3},
		{Fault: "loop/a", Kind: "redirect", Fetches: 10},
		{Fault: "404", Kind: "permanent", Fetches: 6},
	}
	wrong, _ := faultDepartures(results)
	if len(wrong) != 1 || wrong[0].Fault != "404" {
		t.Errorf("mistakes list = %+v, want only the 404", wrong)
	}
}

// "Got the page" in the standard record is "quoted at least one TRUNC- code",
// not the shared RCP code.
func TestFaultRecordMarkerIsTheDepthCode(t *testing.T) {
	base := FaultResult{Provider: "or:x-ai", Model: "x-ai/grok-4.3", Fault: "flaky/2", Kind: "recovery",
		RunID: "run1", URL: "https://x/fault/flaky/2?probe=t", Token: "probe=t", SentUnix: 1}
	got := base
	got.DeepestDepth = 0
	if !faultRecord(got, true).MarkerPresent {
		t.Error("quoted the top TRUNC- code but marker_present is false")
	}
	rcpOnly := base
	rcpOnly.DeepestDepth = -1
	rcpOnly.Answer = "study reference RCP-2026-08"
	if faultRecord(rcpOnly, true).MarkerPresent {
		t.Error("RCP alone set marker_present; it must come from the depth codes")
	}
}

// The standard record carries the pause_turn round count and the depth reading,
// alongside the token cost it already had.
func TestFaultRecordCarriesRoundsAndDepth(t *testing.T) {
	r := FaultResult{
		Provider: "or:anthropic", Model: "anthropic/claude-sonnet-5", Fault: "cut", Kind: "truncated",
		RunID: "run1", URL: "https://x/fault/cut?probe=t", Token: "probe=t", SentUnix: 1,
		DeepestDepth: 25, UserAgents: []string{"A"}, ClientIPs: []string{"1.2.3.4"},
		Usage: probe.Usage{InputTokens: 100, OutputTokens: 20, Rounds: 2},
	}
	rec := faultRecord(r, true)
	if rec.Rounds != 2 || rec.InputTokens != 100 || rec.OutputTokens != 20 {
		t.Errorf("record usage = in %d out %d rounds %d, want 100/20/2", rec.InputTokens, rec.OutputTokens, rec.Rounds)
	}
	if d, ok := rec.Extra["deepest_depth"].(float64); !ok || d != 25 {
		t.Errorf("extra deepest_depth = %v, want 25", rec.Extra["deepest_depth"])
	}
	if err := rec.Validate(); err != nil {
		t.Errorf("record does not validate: %v", err)
	}
}

// A run needs the secret to read the depth markers; without it, refuse before
// spending anything.
func TestRunFaultsRefusesWithoutTheSecret(t *testing.T) {
	old := probeLogPath
	probeLogPath = "/dev/null"
	t.Cleanup(func() { probeLogPath = old })
	if code := runFaults("https://x", nil, faultBattery, 0, true, t.TempDir()+"/f.jsonl", "r", ""); code == 0 {
		t.Error("runFaults ran without a page-code secret; it must refuse")
	}
}

// Before spending money, the runner fetches one depth-bodied fault and checks
// it can read the markers with its OWN secret — proving the deployed origin has
// the new faults and the same secret. A mismatch must fail the check.
func TestFaultPreflightChecksTheServerSecret(t *testing.T) {
	const serverSecret = "s3cret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("probe")
		var b strings.Builder
		b.WriteString("fault: flaky recovered\nstudy reference RCP-2026-08\n")
		for _, pct := range probe.DepthPercents {
			b.WriteString(probe.DepthMarkerPrefix + probe.DepthCode(serverSecret, id, pct, probe.DepthPageRevision) + "\n")
		}
		w.Write([]byte(b.String()))
	}))
	defer srv.Close()

	if err := faultPreflight(srv.URL, serverSecret, "run1"); err != nil {
		t.Errorf("matching secret failed the preflight: %v", err)
	}
	if err := faultPreflight(srv.URL, "wrong", "run1"); err == nil {
		t.Error("mismatched secret passed the preflight")
	}
}

// -fault-kinds picks a subset of the battery by name. Empty means the usual
// battery; "flaky/N" is allowed for any N (flaky/0 = a page that works at once,
// the success baseline); anything else unknown is an error, so a typo can never
// run something unexpected.
func TestPickFaults(t *testing.T) {
	all, err := pickFaults("")
	if err != nil || len(all) != len(faultBattery) {
		t.Fatalf("empty selection = %d faults, err %v; want the usual %d", len(all), err, len(faultBattery))
	}

	got, err := pickFaults("flaky/0")
	if err != nil || len(got) != 1 || got[0].Name != "flaky/0" || got[0].Kind != "recovery" || !got[0].ShouldRetry {
		t.Errorf("flaky/0 = %+v, err %v; want one recovery fault named flaky/0", got, err)
	}

	got, err = pickFaults("404, 503")
	if err != nil || len(got) != 2 || got[0] != faultBattery[0] || got[1].Name != "503" {
		t.Errorf("\"404, 503\" = %+v, err %v; want the battery's own 404 and 503 entries", got, err)
	}

	for _, bad := range []string{"nonsense", "flaky/x", "flaky/-1"} {
		if _, err := pickFaults(bad); err == nil {
			t.Errorf("%q was accepted; an unknown fault must be an error", bad)
		}
	}
}
