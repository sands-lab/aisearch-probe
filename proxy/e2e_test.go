package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aisearch-probe/internal/probe"
)

// This is the test that protects the experiment itself rather than the code.
//
// Two synthetic clients are driven through the real proxy against a real
// origin: one behaves exactly like a well-mannered cache, the other like a
// crawler that never revalidates. If the pipeline cannot tell them apart, the
// instrument is lying and no field data collected with it means anything.

// politeClient keeps the ETag it was given and asks before re-downloading.
type politeClient struct {
	ua    string
	etags map[string]string
}

func newPoliteClient() *politeClient {
	return &politeClient{ua: "PoliteBot/1.0", etags: map[string]string{}}
}

func (c *politeClient) fetch(t *testing.T, target string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("User-Agent", c.ua)
	if etag, ok := c.etags[target]; ok {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch %s: %v", target, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if etag := resp.Header.Get("ETag"); etag != "" {
		c.etags[target] = etag
	}
}

// naiveClient throws away everything it was told and re-downloads every time.
type naiveClient struct{ ua string }

func newNaiveClient() *naiveClient { return &naiveClient{ua: "NaiveBot/1.0"} }

func (c *naiveClient) fetch(t *testing.T, target string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("User-Agent", c.ua)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch %s: %v", target, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
}

// e2eSetup runs a stamped origin behind the probe and returns the proxy URL
// plus a function that analyses whatever has been logged so far.
func e2eSetup(t *testing.T, origin http.Handler) (string, func() []probe.BotReport) {
	t.Helper()

	originSrv := httptest.NewServer(origin)
	t.Cleanup(originSrv.Close)
	originURL, err := url.Parse(originSrv.URL)
	if err != nil {
		t.Fatalf("parse origin: %v", err)
	}
	return e2eWire(t, func(logger *Logger) *Handler { return NewHandler(originURL, logger, "") })
}

// e2eSetupBackend is e2eSetup for a backend the probe serves directly, with no
// separate origin process — the -serveDir mode path.
func e2eSetupBackend(t *testing.T, backend http.Handler) (string, func() []probe.BotReport) {
	t.Helper()
	return e2eWire(t, func(logger *Logger) *Handler { return NewHandlerWithBackend(backend, logger, "") })
}

func e2eWire(t *testing.T, build func(*Logger) *Handler) (string, func() []probe.BotReport) {
	t.Helper()

	logPath := filepath.Join(t.TempDir(), "probe.jsonl")
	logger, err := NewLogger(logPath)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })

	h := build(logger)
	h.quiet = true
	proxySrv := httptest.NewServer(h)
	t.Cleanup(proxySrv.Close)

	analyse := func() []probe.BotReport {
		f, err := os.Open(logPath)
		if err != nil {
			t.Fatalf("open log: %v", err)
		}
		defer f.Close()
		recs, skipped, err := probe.ParseRecords(f, "", time.Time{})
		if err != nil {
			t.Fatalf("parse log: %v", err)
		}
		if skipped > 0 {
			t.Fatalf("%d unparseable lines", skipped)
		}
		return probe.Analyze(recs)
	}
	return proxySrv.URL, analyse
}

func reportFor(t *testing.T, reports []probe.BotReport, bot string) probe.BotReport {
	t.Helper()
	for _, r := range reports {
		if r.Bot == bot {
			return r
		}
	}
	names := make([]string, 0, len(reports))
	for _, r := range reports {
		names = append(names, r.Bot)
	}
	t.Fatalf("no report for %q; got %v", bot, names)
	return probe.BotReport{}
}

// The headline test: the instrument must separate a good citizen from a bad
// one. Each client runs against its own origin+log so the two reports can be
// compared directly.
func TestPipelineDistinguishesPoliteFromNaiveClient(t *testing.T) {
	body := "the page body that both clients will fetch"

	politeURL, politeAnalyse := e2eSetup(t, stampedOrigin(body, `"v1"`))
	naiveURL, naiveAnalyse := e2eSetup(t, stampedOrigin(body, `"v1"`))

	polite, naive := newPoliteClient(), newNaiveClient()
	for i := 0; i < 3; i++ {
		polite.fetch(t, politeURL+"/docs/overview")
		naive.fetch(t, naiveURL+"/docs/overview")
	}

	good := reportFor(t, politeAnalyse(), "unknown")
	bad := reportFor(t, naiveAnalyse(), "unknown")

	// Identical traffic volume, opposite behaviour.
	if good.TotalRequests != bad.TotalRequests {
		t.Fatalf("clients made different numbers of requests: %d vs %d", good.TotalRequests, bad.TotalRequests)
	}
	if good.Judgeable != bad.Judgeable {
		t.Fatalf("different judgeable counts: %d vs %d — the comparison is not like-for-like", good.Judgeable, bad.Judgeable)
	}
	if good.AskedPct() != 100 || bad.AskedPct() != 0 {
		t.Errorf("AskedPct polite=%v naive=%v, want 100 and 0", good.AskedPct(), bad.AskedPct())
	}
	if good.WastedBytes != 0 {
		t.Errorf("polite client charged %d wasted bytes, want 0", good.WastedBytes)
	}
	if bad.WastedBytes <= 0 {
		t.Errorf("naive client charged %d wasted bytes, want > 0", bad.WastedBytes)
	}
	if !strings.Contains(good.Verdict(), "Well behaved") {
		t.Errorf("polite verdict = %q, want it recognised as well behaved", good.Verdict())
	}
	if !strings.Contains(bad.Verdict(), "Never asks") {
		t.Errorf("naive verdict = %q, want it called out", bad.Verdict())
	}
}

// Same experiment, but each synthetic client is isolated so the report can be
// asserted precisely.
func TestPoliteClientIsJudgedWellBehaved(t *testing.T) {
	body := "page body"
	proxyURL, analyse := e2eSetup(t, stampedOrigin(body, `"v1"`))
	target := proxyURL + "/docs/overview"

	polite := newPoliteClient()
	for i := 0; i < 3; i++ {
		polite.fetch(t, target)
	}

	r := reportFor(t, analyse(), "unknown")

	if r.TotalRequests != 3 {
		t.Fatalf("TotalRequests = %d, want 3", r.TotalRequests)
	}
	if r.FirstSight != 1 {
		t.Errorf("FirstSight = %d, want 1", r.FirstSight)
	}
	if r.ReturnUnstamped != 0 {
		t.Errorf("ReturnUnstamped = %d, want 0 — the origin stamps every response", r.ReturnUnstamped)
	}
	if r.Judgeable != 2 {
		t.Errorf("Judgeable = %d, want 2", r.Judgeable)
	}
	if r.Asked != 2 || r.Blind != 0 {
		t.Errorf("Asked=%d Blind=%d, want 2/0", r.Asked, r.Blind)
	}
	if r.NotModified != 2 {
		t.Errorf("NotModified = %d, want 2 — the origin should have answered 304", r.NotModified)
	}
	if r.WastedBytes != 0 {
		t.Errorf("WastedBytes = %d, want 0 — a polite client wastes nothing", r.WastedBytes)
	}
	if got := r.AskedPct(); got != 100 {
		t.Errorf("AskedPct = %v, want 100", got)
	}
}

func TestNaiveClientIsJudgedWasteful(t *testing.T) {
	body := "page body that gets re-sent again and again"
	proxyURL, analyse := e2eSetup(t, stampedOrigin(body, `"v1"`))
	target := proxyURL + "/docs/overview"

	naive := newNaiveClient()
	for i := 0; i < 3; i++ {
		naive.fetch(t, target)
	}

	r := reportFor(t, analyse(), "unknown")

	if r.FirstSight != 1 {
		t.Errorf("FirstSight = %d, want 1", r.FirstSight)
	}
	if r.Judgeable != 2 {
		t.Errorf("Judgeable = %d, want 2", r.Judgeable)
	}
	if r.Asked != 0 || r.Blind != 2 {
		t.Errorf("Asked=%d Blind=%d, want 0/2", r.Asked, r.Blind)
	}
	// Only the two return visits are waste; the discovery fetch is not.
	if want := int64(2 * len(body)); r.WastedBytes != want {
		t.Errorf("WastedBytes = %d, want %d (two re-downloads, discovery excluded)", r.WastedBytes, want)
	}
	if got := r.AskedPct(); got != 0 {
		t.Errorf("AskedPct = %v, want 0", got)
	}
}

// The control that catches a broken origin: when we never stamp, even a client
// that WOULD have asked cannot, and the report must blame us instead of it.
func TestUnstampedOriginProducesNoVerdictAgainstTheBot(t *testing.T) {
	bare := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("no validators here"))
	})
	proxyURL, analyse := e2eSetup(t, bare)
	target := proxyURL + "/docs/overview"

	polite := newPoliteClient() // wants to be good, but is given nothing to ask with
	for i := 0; i < 3; i++ {
		polite.fetch(t, target)
	}

	r := reportFor(t, analyse(), "unknown")

	if r.Judgeable != 0 {
		t.Errorf("Judgeable = %d, want 0 — we gave it nothing to revalidate against", r.Judgeable)
	}
	if r.ReturnUnstamped != 2 {
		t.Errorf("ReturnUnstamped = %d, want 2", r.ReturnUnstamped)
	}
	if r.WastedBytes != 0 {
		t.Errorf("WastedBytes = %d, want 0 — this is our bug, not waste to charge the bot", r.WastedBytes)
	}
	if v := r.Verdict(); v == "" || !strings.Contains(v, "never stamped") {
		t.Errorf("Verdict = %q, want it to point at our own missing ETag headers", v)
	}
}

// A crawler that only ever discovers new pages must not be labelled wasteful —
// the exact bias the original all-requests ratio suffered from.
func TestDiscoveryOnlyCrawlerIsNotAccused(t *testing.T) {
	proxyURL, analyse := e2eSetup(t, stampedOrigin("body", `"v1"`))

	naive := newNaiveClient()
	for _, p := range []string{"/a", "/b", "/c", "/d", "/e"} {
		naive.fetch(t, proxyURL+p)
	}

	r := reportFor(t, analyse(), "unknown")

	if r.FirstSight != 5 {
		t.Errorf("FirstSight = %d, want 5", r.FirstSight)
	}
	if r.Judgeable != 0 {
		t.Errorf("Judgeable = %d, want 0", r.Judgeable)
	}
	if r.WastedBytes != 0 {
		t.Errorf("WastedBytes = %d, want 0 — discovering new pages is not waste", r.WastedBytes)
	}
	if v := r.Verdict(); !strings.Contains(v, "Can't tell yet") {
		t.Errorf("Verdict = %q, want a 'can't tell yet' verdict", v)
	}
}
