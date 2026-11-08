package main

import (
	"encoding/json"
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

// newTestProxy wires a probe in front of origin, logging to a temp file.
// Returns the proxy server and a function that reads back the log.
func newTestProxy(t *testing.T, origin http.Handler, filter string) (*httptest.Server, func() []probe.Record, string) {
	t.Helper()

	originSrv := httptest.NewServer(origin)
	t.Cleanup(originSrv.Close)

	originURL, err := url.Parse(originSrv.URL)
	if err != nil {
		t.Fatalf("parse origin URL: %v", err)
	}

	logPath := filepath.Join(t.TempDir(), "probe.jsonl")
	logger, err := NewLogger(logPath)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })

	h := NewHandler(originURL, logger, filter)
	h.quiet = true

	proxySrv := httptest.NewServer(h)
	t.Cleanup(proxySrv.Close)

	read := func() []probe.Record {
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
			t.Fatalf("%d unparseable lines in log", skipped)
		}
		return recs
	}
	return proxySrv, read, logPath
}

// stampedOrigin serves a fixed body with an ETag and honours If-None-Match —
// i.e. a correctly configured origin.
func stampedOrigin(body string, etag string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "max-age=3600")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body))
	})
}

func get(t *testing.T, url string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("User-Agent", "TestBot/1.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func TestLogsRequestAndResponseFields(t *testing.T) {
	body := "hello world"
	srv, read, _ := newTestProxy(t, stampedOrigin(body, `"v1"`), "")

	resp := get(t, srv.URL+"/docs/page?x=1", nil)
	resp.Body.Close()

	recs := read()
	if len(recs) != 1 {
		t.Fatalf("logged %d records, want 1", len(recs))
	}
	r := recs[0]

	if r.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", r.StatusCode)
	}
	if r.PageKey != "/docs/page" {
		t.Errorf("PageKey = %q, want /docs/page (query stripped)", r.PageKey)
	}
	if !strings.Contains(r.URL, "x=1") {
		t.Errorf("URL = %q, want the full URL including the query", r.URL)
	}
	if r.ETagSent != `"v1"` {
		t.Errorf("ETagSent = %q, want \"v1\"", r.ETagSent)
	}
	if !r.Stamped() {
		t.Error("Stamped() = false, want true")
	}
	if r.BytesSent != int64(len(body)) {
		t.Errorf("BytesSent = %d, want %d", r.BytesSent, len(body))
	}
	if r.CacheControlResp != "max-age=3600" {
		t.Errorf("CacheControlResp = %q", r.CacheControlResp)
	}
	if r.TSUnix <= 0 {
		t.Error("TSUnix not set — analysis intervals depend on it")
	}
	if r.AskedIfChanged() {
		t.Error("AskedIfChanged() = true, want false for a plain GET")
	}
}

// The cache-header test grades against the headers we actually sent, and some of
// its settings use Expires instead of Cache-Control — so Expires must be logged.
func TestLogsExpiresSent(t *testing.T) {
	const exp = "Thu, 01 Jan 2026 00:00:00 GMT"
	origin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Expires", exp)
		w.Write([]byte("x"))
	})
	srv, read, _ := newTestProxy(t, origin, "")
	resp := get(t, srv.URL+"/e", nil)
	resp.Body.Close()
	recs := read()
	if len(recs) != 1 {
		t.Fatalf("logged %d records, want 1", len(recs))
	}
	if recs[0].ExpiresSent != exp {
		t.Errorf("ExpiresSent = %q, want %q", recs[0].ExpiresSent, exp)
	}
}

func TestConditionalRequestIsRecordedAndForwarded(t *testing.T) {
	srv, read, _ := newTestProxy(t, stampedOrigin("hello", `"v1"`), "")

	resp := get(t, srv.URL+"/a", map[string]string{"If-None-Match": `"v1"`})
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("status = %d, want 304 — the probe must forward the conditional header", resp.StatusCode)
	}
	recs := read()
	if len(recs) != 1 {
		t.Fatalf("logged %d records, want 1", len(recs))
	}
	if !recs[0].AskedIfChanged() {
		t.Error("AskedIfChanged() = false, want true")
	}
	if recs[0].StatusCode != 304 {
		t.Errorf("StatusCode = %d, want 304", recs[0].StatusCode)
	}
	if recs[0].BytesSent != 0 {
		t.Errorf("BytesSent = %d, want 0 for a 304", recs[0].BytesSent)
	}
}

func TestNonGETIsProxiedButNotLogged(t *testing.T) {
	srv, read, _ := newTestProxy(t, stampedOrigin("hello", `"v1"`), "")

	resp, err := http.Post(srv.URL+"/a", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()

	if got := len(read()); got != 0 {
		t.Errorf("logged %d records, want 0 — only GET is evidence", got)
	}
}

func TestFilterLimitsLoggingButNotProxying(t *testing.T) {
	srv, read, _ := newTestProxy(t, stampedOrigin("hello", `"v1"`), "/docs")

	r1 := get(t, srv.URL+"/docs/a", nil)
	r1.Body.Close()
	r2 := get(t, srv.URL+"/other", nil)
	r2.Body.Close()

	if r2.StatusCode != 200 {
		t.Errorf("filtered-out path returned %d, want 200 — it must still be proxied", r2.StatusCode)
	}
	recs := read()
	if len(recs) != 1 {
		t.Fatalf("logged %d records, want 1", len(recs))
	}
	if recs[0].PageKey != "/docs/a" {
		t.Errorf("PageKey = %q, want /docs/a", recs[0].PageKey)
	}
}

func TestUnstampedOriginIsVisibleInTheLog(t *testing.T) {
	bare := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("no validators here"))
	})
	srv, read, _ := newTestProxy(t, bare, "")

	resp := get(t, srv.URL+"/a", nil)
	resp.Body.Close()

	recs := read()
	if len(recs) != 1 {
		t.Fatalf("logged %d records, want 1", len(recs))
	}
	if recs[0].Stamped() {
		t.Error("Stamped() = true, want false — origin sent no ETag or Last-Modified")
	}
}

// The probe must stay transparent to streaming origins. The origin below
// refuses to finish until the client has actually received its first chunk, so
// if the probe swallows the flush the client never sees anything and this test
// times out rather than quietly passing on buffered data delivered at the end.
func TestStreamingOriginReachesClientBeforeHandlerReturns(t *testing.T) {
	released := make(chan struct{})
	defer close(released) // never leave the origin handler wedged

	streaming := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		rc := http.NewResponseController(w)
		if _, err := w.Write([]byte("data: first\n\n")); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			t.Errorf("origin could not flush through the probe: %v", err)
			return
		}
		<-released // hold the response open until the client has read
		w.Write([]byte("data: second\n\n"))
		rc.Flush()
	})

	srv, _, _ := newTestProxy(t, streaming, "")

	// ResponseHeaderTimeout bounds the exact failure mode: if the probe
	// buffers, the response headers never arrive either, and we want a clear
	// failure in seconds rather than the package-wide test timeout.
	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 3 * time.Second}}
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("User-Agent", "TestBot/1.0")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("streaming request failed — the probe did not pass the response through: %v", err)
	}
	defer resp.Body.Close()

	type readResult struct {
		s   string
		err error
	}
	done := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := resp.Body.Read(buf)
		done <- readResult{string(buf[:n]), err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("reading the stream failed: %v", got.err)
		}
		if !strings.Contains(got.s, "data: first") {
			t.Errorf("got %q, want the first streamed event", got.s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the first chunk — the probe buffered a streaming response instead of passing it through")
	}
}

func TestClientIPPrefersCloudflareHeaderAndSurvivesIPv6(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"cloudflare header wins", map[string]string{"CF-Connecting-IP": "203.0.113.9", "X-Forwarded-For": "198.51.100.1"}, "203.0.113.9"},
		{"first XFF entry", map[string]string{"X-Forwarded-For": "198.51.100.1, 10.0.0.1"}, "198.51.100.1"},
		{"ipv6 XFF is not truncated", map[string]string{"X-Forwarded-For": "2001:db8::1"}, "2001:db8::1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, read, _ := newTestProxy(t, stampedOrigin("x", `"v1"`), "")
			resp := get(t, srv.URL+"/a", tc.headers)
			resp.Body.Close()
			recs := read()
			if len(recs) != 1 {
				t.Fatalf("logged %d records, want 1", len(recs))
			}
			if recs[0].ClientIP != tc.want {
				t.Errorf("ClientIP = %q, want %q", recs[0].ClientIP, tc.want)
			}
		})
	}
}

// A raw IPv6 socket address must not be mangled into "[".
func TestClientIPFromIPv6RemoteAddr(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/a", nil)
	r.RemoteAddr = "[2001:db8::1]:54321"
	if got := clientIP(r); got != "2001:db8::1" {
		t.Errorf("clientIP = %q, want 2001:db8::1", got)
	}
}

func TestRequestSchemeHonoursForwardedProto(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/a", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := requestScheme(r); got != "https" {
		t.Errorf("requestScheme = %q, want https", got)
	}
	r2 := httptest.NewRequest(http.MethodGet, "/a", nil)
	if got := requestScheme(r2); got != "http" {
		t.Errorf("requestScheme = %q, want http for a plaintext request", got)
	}
}

func TestCFCacheStatusIsRecorded(t *testing.T) {
	srv, read, _ := newTestProxy(t, stampedOrigin("x", `"v1"`), "")
	resp := get(t, srv.URL+"/a", map[string]string{"CF-Cache-Status": "HIT", "CF-Ray": "abc123-CDG"})
	resp.Body.Close()

	recs := read()
	if len(recs) != 1 {
		t.Fatalf("logged %d records, want 1", len(recs))
	}
	if !recs[0].EdgeServed() {
		t.Error("EdgeServed() = false, want true — a CDN hit must be visible as contamination")
	}
	if recs[0].CFRay != "abc123-CDG" {
		t.Errorf("CFRay = %q", recs[0].CFRay)
	}
}

// Every logged line must be valid JSON on its own — the analyzer reads JSONL.
func TestLogLinesAreValidJSONL(t *testing.T) {
	srv, _, logPath := newTestProxy(t, stampedOrigin("hello", `"v1"`), "")
	for i := 0; i < 3; i++ {
		resp := get(t, srv.URL+"/a", nil)
		resp.Body.Close()
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Errorf("line %d is not valid JSON: %v", i+1, err)
		}
	}
}
