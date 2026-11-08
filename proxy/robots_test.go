package main

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// postForm posts a form to the proxy and returns the response.
func postForm(t *testing.T, target string, form url.Values, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// renderRobots must leave the base untouched when nothing is disallowed, and
// otherwise put the crawler's own group above the base so the crawler obeys it
// and does not merge it with the wildcard allow.
func TestRenderRobots(t *testing.T) {
	base := "User-agent: *\nAllow: /\n"

	if got := renderRobots(base, nil); got != base {
		t.Errorf("no rules must return the base unchanged:\n%q", got)
	}

	out := renderRobots(base, []robotsRule{{Agent: "Claude-User", Path: "/a.html"}})
	if !strings.Contains(out, "User-agent: Claude-User") || !strings.Contains(out, "Disallow: /a.html") {
		t.Errorf("missing the Claude-User disallow group:\n%s", out)
	}
	// The wildcard allow-all base must still be present, so every other crawler
	// and page B stay allowed.
	if !strings.Contains(out, "User-agent: *") {
		t.Errorf("base allow-all lost:\n%s", out)
	}
	// The crawler group must come before the base wildcard group.
	if strings.Index(out, "Claude-User") > strings.Index(out, "User-agent: *") {
		t.Errorf("crawler group must precede the wildcard group:\n%s", out)
	}
}

// With no secret the control endpoint is not a real endpoint — it 404s and
// robots.txt is exactly the file on disk.
func TestControlDisabledWithoutSecret(t *testing.T) {
	dir := writeSite(t, map[string]string{"robots.txt": "User-agent: *\nAllow: /\n"})
	srv, _, _ := newTestProxy(t, newStaticServer(dir, ""), "")

	resp := postForm(t, srv.URL+controlRobotsPath, url.Values{"op": {"disallow"}, "path": {"/a.html"}}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("control endpoint must 404 when disabled, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	rob := get(t, srv.URL+"/robots.txt", nil)
	if got := body(t, rob); strings.Contains(got, "Disallow") {
		t.Errorf("robots.txt must be untouched when the endpoint is disabled:\n%s", got)
	}
}

// The wrong secret is refused, and the right secret flips robots.txt so
// Claude-User is disallowed from page A while page B stays allowed.
func TestControlFlipsRobots(t *testing.T) {
	dir := writeSite(t, map[string]string{"robots.txt": "User-agent: *\nAllow: /\n"})
	st := newStaticServer(dir, "")
	st.controlSecret = "s3cret"
	srv, _, _ := newTestProxy(t, st, "")

	// Wrong secret: forbidden, nothing changes.
	bad := postForm(t, srv.URL+controlRobotsPath,
		url.Values{"op": {"disallow"}, "path": {"/a.html"}, "secret": {"nope"}}, nil)
	if bad.StatusCode != http.StatusForbidden {
		t.Errorf("wrong secret must be forbidden, got %d", bad.StatusCode)
	}
	bad.Body.Close()

	// Right secret via header: disallow page A for the default crawler.
	ok := postForm(t, srv.URL+controlRobotsPath,
		url.Values{"op": {"disallow"}, "path": {"/a.html"}},
		map[string]string{"X-Probe-Control-Secret": "s3cret"})
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("correct secret must succeed, got %d", ok.StatusCode)
	}
	ok.Body.Close()

	rob := get(t, srv.URL+"/robots.txt", nil)
	got := body(t, rob)
	if !strings.Contains(got, "User-agent: "+defaultCrawler) || !strings.Contains(got, "Disallow: /a.html") {
		t.Errorf("page A must be disallowed for %s:\n%s", defaultCrawler, got)
	}
	if strings.Contains(got, "/b.html") {
		t.Errorf("page B must never be named — it is the control:\n%s", got)
	}

	// robots.txt must be no-store, or a cache could hide the flip.
	if cc := rob.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("robots.txt Cache-Control = %q, want no-store", cc)
	}
	if rob.Header.Get("ETag") != "" {
		t.Errorf("robots.txt must carry no ETag, or a validator hides the flip")
	}

	// clear returns to allow-all.
	cl := postForm(t, srv.URL+controlRobotsPath,
		url.Values{"op": {"clear"}}, map[string]string{"X-Probe-Control-Secret": "s3cret"})
	cl.Body.Close()
	rob2 := get(t, srv.URL+"/robots.txt", nil)
	if got := body(t, rob2); strings.Contains(got, "Disallow") {
		t.Errorf("clear must remove all disallows:\n%s", got)
	}
}

// The respect test disallows a page for every crawler (User-agent: *). The
// output must be a SINGLE "*" group — the armed one replaces the base allow-all,
// or two "*" groups would be ambiguous — and the control page, never named,
// stays allowed.
func TestControlWildcardBlockReplacesBase(t *testing.T) {
	dir := writeSite(t, map[string]string{"robots.txt": "User-agent: *\nAllow: /\n"})
	st := newStaticServer(dir, "")
	st.controlSecret = "s3cret"
	srv, _, _ := newTestProxy(t, st, "")

	ok := postForm(t, srv.URL+controlRobotsPath,
		url.Values{"op": {"disallow"}, "agent": {"*"}, "path": {"/page/run-a"}},
		map[string]string{"X-Probe-Control-Secret": "s3cret"})
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("wildcard disallow failed: %d", ok.StatusCode)
	}
	ok.Body.Close()

	got := body(t, get(t, srv.URL+"/robots.txt", nil))
	if strings.Count(got, "User-agent: *") != 1 {
		t.Errorf("want exactly one wildcard group, got:\n%s", got)
	}
	if !strings.Contains(got, "Disallow: /page/run-a") {
		t.Errorf("page A must be disallowed for everyone:\n%s", got)
	}
	if strings.Contains(got, "/page/run-b") {
		t.Errorf("the control page must never be named:\n%s", got)
	}
	// The base "Allow: /" must be gone, so there is no second wildcard group.
	if strings.Contains(got, "Allow: /") {
		t.Errorf("base allow-all should be replaced by the armed wildcard group:\n%s", got)
	}
}
