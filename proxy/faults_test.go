package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aisearch-probe/internal/probe"
)

// getNoRedirect fetches without following redirects and without failing the
// test on a non-2xx — the fault paths return errors on purpose.
func getRaw(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	return resp
}

// Each fault path must return exactly its defined status, so the failure-retry
// experiment is measuring what it thinks it is.
func TestFaultPathsReturnTheirDefinedStatus(t *testing.T) {
	dir := writeSite(t, map[string]string{"index.html": "x"})
	srv, _, _ := newTestProxy(t, newStaticServer(dir, ""), "")

	cases := []struct {
		path string
		want int
	}{
		{"/fault/404", http.StatusNotFound},
		{"/fault/500", http.StatusInternalServerError},
		{"/fault/503", http.StatusServiceUnavailable},
		{"/fault/empty", http.StatusOK},
	}
	for _, c := range cases {
		resp := getRaw(t, srv.URL+c.path)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s → %d, want %d", c.path, resp.StatusCode, c.want)
		}
	}
}

// The 503 must carry the one back-off signal the HTTP standard defines, or the
// "does it honour Retry-After" question cannot be asked.
func TestFault503CarriesRetryAfter(t *testing.T) {
	dir := writeSite(t, map[string]string{"index.html": "x"})
	srv, _, _ := newTestProxy(t, newStaticServer(dir, ""), "")

	resp := getRaw(t, srv.URL+"/fault/503")
	resp.Body.Close()
	if got := resp.Header.Get("Retry-After"); got != "10" {
		t.Errorf("Retry-After = %q, want \"10\"", got)
	}
}

// The empty fault is a valid 200 with no body — a fetcher should read it as
// success, so it must genuinely be empty and genuinely be 200.
func TestFaultEmptyIsAValidButEmpty200(t *testing.T) {
	dir := writeSite(t, map[string]string{"index.html": "x"})
	srv, _, _ := newTestProxy(t, newStaticServer(dir, ""), "")

	resp := getRaw(t, srv.URL+"/fault/empty")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Errorf("body was %d bytes, want empty", len(body))
	}
}

// The slow fault must actually delay, then return a real 200 with the page's
// reference code — so a fetcher that waits gets a genuine success at the end.
func TestFaultSlowDelaysThenSucceeds(t *testing.T) {
	orig := slowFaultDelay
	slowFaultDelay = 300 * time.Millisecond // keep the test quick
	t.Cleanup(func() { slowFaultDelay = orig })

	srv := newFaultServer(t, faultTestSecret)

	start := time.Now()
	resp := getRaw(t, srv.URL+"/fault/slow")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Errorf("returned after %s, expected it to wait ~300ms", elapsed)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "RCP-2026-08") {
		t.Errorf("slow response did not carry the page reference code: %q", body)
	}
}

// The reset fault drops the connection. What the caller sees depends on what
// sits in front: called directly it is a transport error; through a reverse
// proxy (which is how it is really deployed, behind cloudflared) the proxy turns
// the mid-response reset into a 502. Either way the fetcher sees a failure, not
// a page — which is the point. So accept a transport error OR a 5xx.
func TestFaultResetIsSeenAsAFailure(t *testing.T) {
	dir := writeSite(t, map[string]string{"index.html": "x"})
	srv, _, _ := newTestProxy(t, newStaticServer(dir, ""), "")

	resp, err := http.Get(srv.URL + "/fault/reset")
	if err != nil {
		return // transport error — a reset seen directly
	}
	defer resp.Body.Close()
	if resp.StatusCode < 500 {
		t.Errorf("reset surfaced as %d; expected a transport error or a 5xx", resp.StatusCode)
	}
}

// flaky/N fails N times for one caller and then succeeds — the recovery test.
// Each caller is counted on its own probe token.
func TestFaultFlakyFailsNTimesThenSucceeds(t *testing.T) {
	srv := newFaultServer(t, faultTestSecret)

	url := srv.URL + "/fault/flaky/2?probe=tester"
	for attempt := 1; attempt <= 3; attempt++ {
		resp := getRaw(t, url)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if attempt <= 2 {
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("attempt %d → %d, want 503 (still failing)", attempt, resp.StatusCode)
			}
		} else {
			if resp.StatusCode != http.StatusOK {
				t.Errorf("attempt %d → %d, want 200 (recovered)", attempt, resp.StatusCode)
			}
			if !strings.Contains(string(body), "RCP-2026-08") {
				t.Errorf("recovery response missing the reference code: %q", body)
			}
		}
	}
}

// Two different callers must not share a flaky counter, or one provider's
// attempts would be blamed on another.
func TestFaultFlakyCountsEachCallerSeparately(t *testing.T) {
	dir := writeSite(t, map[string]string{"index.html": "x"})
	srv, _, _ := newTestProxy(t, newStaticServer(dir, ""), "")

	// Caller A exhausts its two failures.
	for i := 0; i < 2; i++ {
		getRaw(t, srv.URL+"/fault/flaky/2?probe=aaa").Body.Close()
	}
	// Caller B's FIRST attempt must still fail — its own counter is fresh.
	resp := getRaw(t, srv.URL+"/fault/flaky/2?probe=bbb")
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("second caller's first attempt → %d, want 503; counters leaked between callers", resp.StatusCode)
	}
}

// An unknown fault name must be an ordinary 404, never a silent success that
// would read as a page that exists.
func TestUnknownFaultNameIs404(t *testing.T) {
	dir := writeSite(t, map[string]string{"index.html": "x"})
	srv, _, _ := newTestProxy(t, newStaticServer(dir, ""), "")

	resp := getRaw(t, srv.URL+"/fault/teapot")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown fault → %d, want 404", resp.StatusCode)
	}
}

// The fault corner must not carry an ETag, or a fault path could pollute a
// caching experiment if it were ever reused.
func TestFaultResponsesCarryNoValidators(t *testing.T) {
	dir := writeSite(t, map[string]string{"index.html": "x"})
	srv, _, _ := newTestProxy(t, newStaticServer(dir, ""), "")

	for _, p := range []string{"/fault/404", "/fault/500", "/fault/503", "/fault/empty"} {
		resp := getRaw(t, srv.URL+p)
		resp.Body.Close()
		if resp.Header.Get("ETag") != "" || resp.Header.Get("Last-Modified") != "" {
			t.Errorf("%s carried a validator; fault responses must not", p)
		}
	}
}

// A normal page must still serve untouched — the fault prefix must not eat
// ordinary traffic.
func TestNormalPageStillServesAlongsideFaults(t *testing.T) {
	dir := writeSite(t, map[string]string{"method.html": "real page RCP-2026-08"})
	srv, _, _ := newTestProxy(t, newStaticServer(dir, ""), "")

	resp := getRaw(t, srv.URL+"/method.html")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "real page") {
		t.Errorf("normal page broke: %d %q", resp.StatusCode, body)
	}
}

// Every fault request must still be logged, or the retry count has nothing to
// count. (The reset fault is excluded: hijacking takes the socket from the
// recorder, which is documented.)
func TestFaultRequestsAreLogged(t *testing.T) {
	dir := writeSite(t, map[string]string{"index.html": "x"})
	srv, read, _ := newTestProxy(t, newStaticServer(dir, ""), "")

	for _, p := range []string{"/fault/404", "/fault/500", "/fault/503"} {
		getRaw(t, srv.URL+p).Body.Close()
	}
	recs := read()
	seen := map[string]bool{}
	for _, r := range recs {
		seen[r.PageKey] = true
	}
	for _, p := range []string{"/fault/404", "/fault/500", "/fault/503"} {
		if !seen[p] {
			t.Errorf("%s did not appear in the log; a retry count could not see it", p)
		}
	}
}

// No file named "fault" may exist under site/, or a real page could
// shadow the reserved prefix and turn a fault into a normal 200.
func TestNoRealPageShadowsTheFaultPrefix(t *testing.T) {
	entries, err := os.ReadDir("../site")
	if err != nil {
		t.Fatalf("read site: %v", err)
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name(), "fault") || strings.HasPrefix(strings.ToLower(e.Name()), "fault.") {
			t.Errorf("site/%s would shadow the reserved /fault/ prefix", e.Name())
		}
	}
}

// The flaky counter must be safe under concurrent callers — a real run has
// several providers in flight.
func TestFlakyStateIsConcurrencySafe(t *testing.T) {
	fs := newFlakyState()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); fs.nextAttempt("k") }()
	}
	wg.Wait()
	if got := fs.nextAttempt("k"); got != 51 {
		t.Errorf("counter = %d after 50 concurrent + 1, want 51 (a race lost increments)", got)
	}
}

// The heal fault is the negative-caching probe: broken for its first window,
// then fixed. It models TIME-based transient recovery — a busy server that
// returns 503 with a Retry-After delay (RFC 9110 §10.2.3), healing on a clock
// rather than on a retry count (the attempt-based model is flaky/N, for cold
// start). See healState in faults.go.
func TestFaultHealIsBrokenThenHealsOnTime(t *testing.T) {
	srv := newFaultServer(t, faultTestSecret)

	// A 1-second window keeps the test quick. First sight starts the clock.
	url := srv.URL + "/fault/heal/1?probe=healtest"

	// While broken: 503 with a Retry-After delay, per RFC 9110 §10.2.3.
	resp := getRaw(t, url)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("during the window: status %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("503 during the window carried no Retry-After — the busy-server signal (RFC 9110 §10.2.3)")
	}
	if strings.Contains(string(body), "RCP-2026-08") {
		t.Error("a broken heal response must not carry the marker")
	}

	// After the window: healed 200 with the marker, so a re-fetch is provable.
	time.Sleep(1200 * time.Millisecond)
	resp = getRaw(t, url)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("after the window: status %d, want 200 (healed)", resp.StatusCode)
	}
	if !strings.Contains(string(body), "RCP-2026-08") {
		t.Errorf("healed response missing the marker: %q", body)
	}
}

// The heal clock is per caller (per token), so one caller warming a URL does not
// heal it for another — each provider's trial is independent.
func TestFaultHealClockIsPerCaller(t *testing.T) {
	dir := writeSite(t, map[string]string{"index.html": "x"})
	srv, _, _ := newTestProxy(t, newStaticServer(dir, ""), "")

	// Caller A starts a long window.
	getRaw(t, srv.URL+"/fault/heal/3600?probe=aaa").Body.Close()
	// Caller B's FIRST sight starts B's own clock — still broken, not healed.
	resp := getRaw(t, srv.URL+"/fault/heal/3600?probe=bbb")
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("second caller's first sight → %d, want 503; heal clocks leaked between callers", resp.StatusCode)
	}
}

// remaining() must count down and never resurrect a healed URL.
func TestHealRemainingCountsDownAndStaysHealed(t *testing.T) {
	h := newHealState()
	if rem := h.remaining("k", 50*time.Millisecond); rem <= 0 {
		t.Fatalf("first sight should be broken (remaining>0), got %v", rem)
	}
	time.Sleep(70 * time.Millisecond)
	if rem := h.remaining("k", 50*time.Millisecond); rem > 0 {
		t.Errorf("after the window remaining should be <=0 (healed), got %v", rem)
	}
}

// --- the 2026-10-08 additions: 429, 429date, 403, loop, cut, depth bodies ----

const faultTestSecret = "fault-test-secret"

// newFaultServer wires the static server the way production runs it — `-serveDir`
// mode, the logging handler calling the static server in-process
// (NewHandlerWithBackend), with no reverse proxy between them. That matters for
// /fault/cut: Go's ReverseProxy buffers a body with a known length and, when the
// upstream drops mid-body, aborts without flushing — so through newTestProxy the
// client sees a bare EOF and never the partial page. Production has no such hop.
//
// The page-code secret is set, which the content-ending faults need for their
// depth markers. cacheControl is set to a real value on purpose: it applies to
// ordinary files, and the validator test proves it never leaks onto /fault/.
func newFaultServer(t *testing.T, secret string) *httptest.Server {
	t.Helper()
	dir := writeSite(t, map[string]string{"index.html": "x"})
	st := newStaticServer(dir, "max-age=60")
	st.pageCodeSecret = secret
	logger, err := NewLogger(filepath.Join(t.TempDir(), "probe.jsonl"))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })
	h := NewHandlerWithBackend(st, logger, "")
	h.quiet = true
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// noFollow is a client that hands back a redirect instead of following it, so a
// test can inspect each hop of the loop.
var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}}

func TestNewFaultPathsReturnTheirDefinedStatus(t *testing.T) {
	srv := newFaultServer(t, faultTestSecret)
	cases := []struct {
		path string
		want int
	}{
		{"/fault/429", http.StatusTooManyRequests},
		{"/fault/429date", http.StatusTooManyRequests},
		{"/fault/403", http.StatusForbidden},
		{"/fault/loop/a?probe=t1", http.StatusFound},
		{"/fault/loop/b?probe=t1", http.StatusFound},
	}
	for _, c := range cases {
		resp, err := noFollow.Get(srv.URL + c.path)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s → %d, want %d", c.path, resp.StatusCode, c.want)
		}
	}
}

// 429 carries the same seconds-form Retry-After as 503, so the two rows compare.
func TestFault429CarriesRetryAfterSeconds(t *testing.T) {
	srv := newFaultServer(t, faultTestSecret)
	resp := getRaw(t, srv.URL+"/fault/429")
	resp.Body.Close()
	if got := resp.Header.Get("Retry-After"); got != "10" {
		t.Errorf("Retry-After = %q, want \"10\"", got)
	}
}

// 429date sends Retry-After in its OTHER legal form, an HTTP-date about ten
// seconds ahead (RFC 9110 §10.2.3). It must parse as a date, not as a number.
func TestFault429DateCarriesRetryAfterAsHTTPDate(t *testing.T) {
	srv := newFaultServer(t, faultTestSecret)
	before := time.Now()
	resp := getRaw(t, srv.URL+"/fault/429date")
	resp.Body.Close()
	got := resp.Header.Get("Retry-After")
	at, err := http.ParseTime(got)
	if err != nil {
		t.Fatalf("Retry-After = %q is not an HTTP-date: %v", got, err)
	}
	// HTTP-dates have whole-second precision, so allow a second either side.
	if d := at.Sub(before); d < 9*time.Second || d > 12*time.Second {
		t.Errorf("Retry-After date is %s ahead, want about 10s", d)
	}
}

// Each loop hop points at the other one and keeps the query, so every hop
// carries the caller's ?probe= token and is attributable in the log.
func TestFaultLoopBouncesAndKeepsTheToken(t *testing.T) {
	srv := newFaultServer(t, faultTestSecret)
	for _, c := range []struct{ from, to string }{
		{"/fault/loop/a?probe=tok1", "/fault/loop/b?probe=tok1"},
		{"/fault/loop/b?probe=tok1", "/fault/loop/a?probe=tok1"},
	} {
		resp, err := noFollow.Get(srv.URL + c.from)
		if err != nil {
			t.Fatalf("%s: %v", c.from, err)
		}
		resp.Body.Close()
		if loc := resp.Header.Get("Location"); loc != c.to {
			t.Errorf("%s redirected to %q, want %q", c.from, loc, c.to)
		}
	}
}

// A client that never stops following must not hammer us forever: after
// loopHopCap hops for one caller the loop answers 508 Loop Detected instead.
func TestFaultLoopStopsAtTheHopCap(t *testing.T) {
	orig := loopHopCap
	loopHopCap = 5
	t.Cleanup(func() { loopHopCap = orig })
	srv := newFaultServer(t, faultTestSecret)

	next := "/fault/loop/a?probe=capper"
	for hop := 1; hop <= 5; hop++ {
		resp, err := noFollow.Get(srv.URL + next)
		if err != nil {
			t.Fatalf("hop %d: %v", hop, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("hop %d → %d, want 302 (still inside the cap)", hop, resp.StatusCode)
		}
		next = resp.Header.Get("Location")
	}
	resp, err := noFollow.Get(srv.URL + next)
	if err != nil {
		t.Fatalf("hop 6: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusLoopDetected {
		t.Errorf("hop past the cap → %d, want 508", resp.StatusCode)
	}
	// Another caller's loop is unaffected.
	resp, err = noFollow.Get(srv.URL + "/fault/loop/a?probe=other")
	if err != nil {
		t.Fatalf("second caller: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("a second caller's first hop → %d, want 302; hop counters leaked", resp.StatusCode)
	}
}

// The cut fault promises the whole depth page in Content-Length, delivers the
// part before the cut, then drops the connection. Through our own reverse proxy
// the client must see a SHORT body ending in an error — the shallow markers
// present, the deep ones absent. (Whether cloudflared preserves this is checked
// live, not here: see docs/experiments/fault.md.)
func TestFaultCutDeliversAPartialDepthPage(t *testing.T) {
	srv := newFaultServer(t, faultTestSecret)
	resp := getRaw(t, srv.URL+"/fault/cut?probe=cutter")
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Errorf("body read cleanly (%d bytes); want an unexpected end", len(body))
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200 (the failure is in the body, not the status)", resp.StatusCode)
	}
	full := len(faultDepthPage(faultTestSecret, "cutter", "fault: cut"))
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(full) {
		t.Errorf("Content-Length %q, want the full page %d", got, full)
	}
	if len(body) >= full {
		t.Errorf("got %d bytes, want fewer than the promised %d", len(body), full)
	}
	if d := probe.DeepestDepth(string(body), faultTestSecret, "cutter", probe.DepthPageRevision); d != 25 {
		t.Errorf("deepest marker in the partial body = %d, want 25 (cut falls between 25%% and 50%%)", d)
	}
	if !strings.Contains(string(body), "RCP-2026-08") {
		t.Error("partial body lost the study reference line at the top")
	}
}

// Every fault that ends in real content serves the depth page, with all five
// markers keyed on the caller's token, and keeps the RCP-2026-08 line that the
// negative-caching runner looks for.
func TestContentEndingFaultsServeTheDepthPage(t *testing.T) {
	orig := slowFaultDelay
	slowFaultDelay = 50 * time.Millisecond
	t.Cleanup(func() { slowFaultDelay = orig })
	srv := newFaultServer(t, faultTestSecret)

	for _, p := range []string{"/fault/slow", "/fault/flaky/0", "/fault/heal/0"} {
		resp := getRaw(t, srv.URL+p+"?probe=deep1")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s → %d, want 200", p, resp.StatusCode)
			continue
		}
		if d := probe.DeepestDepth(string(body), faultTestSecret, "deep1", probe.DepthPageRevision); d != 95 {
			t.Errorf("%s: deepest marker = %d, want 95 (the whole depth page)", p, d)
		}
		if !strings.Contains(string(body), "RCP-2026-08") {
			t.Errorf("%s: lost the RCP-2026-08 line", p)
		}
	}
}

// The fault page is the cache-header test's page (/cacheHeaders/ at its default
// 2048 bytes, HTML) with the two fault lines added on top — the same renderer,
// size and type, so the setup is uniform across experiments.
func TestFaultDepthPageMatchesTheCacheHeaderPage(t *testing.T) {
	cc := string(renderDepthHTML(depthFormatLines(faultTestSecret, "same", 2048)))
	fault := string(faultDepthPage(faultTestSecret, "same", "fault: x"))
	const head = "<title>probe document</title>\n"
	i := strings.Index(cc, head)
	if i < 0 {
		t.Fatal("cache-header page lost its title line; update this test")
	}
	ccBody := cc[i+len(head):]
	want := cc[:i+len(head)] + "<p>fault: x</p>\n<p>" + faultStudyLine + "</p>\n" + ccBody
	if fault != want {
		t.Errorf("fault page is not the cache-header page plus the two fault lines")
	}

	srv := newFaultServer(t, faultTestSecret)
	resp := getRaw(t, srv.URL+"/fault/flaky/0?probe=ct")
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type %q, want text/html like /cacheHeaders/", ct)
	}
}

// A caller that strips the token still gets markers, keyed on "untokenised",
// so the runner can still read how deep it got.
func TestDepthFaultWithoutTokenUsesUntokenisedID(t *testing.T) {
	srv := newFaultServer(t, faultTestSecret)
	resp := getRaw(t, srv.URL+"/fault/flaky/0")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if d := probe.DeepestDepth(string(body), faultTestSecret, "untokenised", probe.DepthPageRevision); d != 95 {
		t.Errorf("deepest marker under the untokenised id = %d, want 95", d)
	}
}

// Without the secret the depth markers cannot be made. Those faults must refuse
// loudly rather than quietly serve a page with no markers.
func TestDepthFaultsRefuseWithoutTheSecret(t *testing.T) {
	srv := newFaultServer(t, "")
	for _, p := range []string{"/fault/flaky/0", "/fault/heal/0", "/fault/cut"} {
		resp := getRaw(t, srv.URL+p)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s served 200 without a secret; it must refuse", p)
		}
		if !strings.Contains(string(body), "secret") {
			t.Errorf("%s refusal does not name the missing secret: %q", p, body)
		}
	}
}

// No /fault/ response may carry ETag, Last-Modified or Cache-Control — including
// the depth-bodied ones, whose normal route DOES stamp validators. A fault page
// that could be cached could leak into a caching run.
func TestEveryFaultResponseIsValidatorFree(t *testing.T) {
	orig := slowFaultDelay
	slowFaultDelay = 50 * time.Millisecond
	t.Cleanup(func() { slowFaultDelay = orig })
	srv := newFaultServer(t, faultTestSecret)

	paths := []string{
		"/fault/404", "/fault/500", "/fault/503", "/fault/429", "/fault/429date",
		"/fault/403", "/fault/empty", "/fault/slow", "/fault/flaky/0", "/fault/flaky/3",
		"/fault/heal/0", "/fault/heal/3600", "/fault/loop/a", "/fault/loop/b", "/fault/cut",
	}
	for _, p := range paths {
		resp, err := noFollow.Get(srv.URL + p + "?probe=v1")
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		io.Copy(io.Discard, resp.Body) // the cut fault errors here; headers already arrived
		resp.Body.Close()
		for _, h := range []string{"ETag", "Last-Modified", "Cache-Control"} {
			if v := resp.Header.Get(h); v != "" {
				t.Errorf("%s carried %s: %q; fault responses must carry none", p, h, v)
			}
		}
	}
}
