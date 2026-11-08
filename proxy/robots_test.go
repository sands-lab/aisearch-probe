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

// robots.txt is the site's own file, sent no-store with no ETag.
func TestRobotsServesTheSiteFileNoStore(t *testing.T) {
	srv, _, _ := newTestProxy(t, newStaticServer("../site", ""), "")
	resp := get(t, srv.URL+"/robots.txt", nil)
	got := body(t, resp)
	if !strings.Contains(got, "User-agent: *") {
		t.Errorf("robots.txt body %q, want the site's own file", got)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", cc)
	}
	if et := resp.Header.Get("ETag"); et != "" {
		t.Errorf("ETag %q, want none", et)
	}
}

// With no robots.txt of its own, the site answers with an allow-all.
func TestRobotsFallsBackToAllowAll(t *testing.T) {
	srv, _, _ := newTestProxy(t, newStaticServer(t.TempDir(), ""), "")
	if got := body(t, get(t, srv.URL+"/robots.txt", nil)); got != builtinRobots {
		t.Errorf("robots.txt body %q, want %q", got, builtinRobots)
	}
}
