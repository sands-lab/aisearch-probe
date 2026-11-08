package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

// A generated page must be a real, stamped, cacheable page carrying the marker,
// so a provider fetches and caches it exactly as it would a normal page.
func TestGeneratedPageIsStampedAndCarriesMarker(t *testing.T) {
	srv, read, _ := newTestProxy(t, newStaticServer(t.TempDir(), "public, max-age=3600"), "")

	resp := get(t, srv.URL+"/page/r0915-a1b2c-a", nil)
	got := body(t, resp)

	if resp.Header.Get("ETag") == "" {
		t.Error("generated page has no ETag — a crawler could not revalidate it")
	}
	if resp.Header.Get("Last-Modified") == "" {
		t.Error("generated page has no Last-Modified")
	}
	if _, present := resp.Header["Cache-Control"]; present {
		t.Errorf("Cache-Control = %q, want none (the normal page, DECISIONS.md § D13)", resp.Header.Get("Cache-Control"))
	}
	if !strings.Contains(got, "RCP-2026-08") {
		t.Errorf("generated page is missing the marker:\n%s", got)
	}
	if !strings.Contains(got, "r0915-a1b2c-a") {
		t.Errorf("generated page is missing its id:\n%s", got)
	}
	recs := read()
	if len(recs) != 1 || !recs[0].Stamped() {
		t.Fatalf("the log must show the generated page as stamped: %+v", recs)
	}
}

// It must answer "has this changed?" with a 304, or a polite crawler would
// re-download it and look wasteful.
func TestGeneratedPageAnswers304(t *testing.T) {
	srv, _, _ := newTestProxy(t, newStaticServer(t.TempDir(), ""), "")

	first := get(t, srv.URL+"/page/r0915-a1b2c-b", nil)
	first.Body.Close()
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on first fetch")
	}
	second := get(t, srv.URL+"/page/r0915-a1b2c-b", map[string]string{"If-None-Match": etag})
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation returned %d, want 304", second.StatusCode)
	}
}

// A bare /page/ (no id) must 404 rather than read as a real page.
func TestGeneratedPageBareIs404(t *testing.T) {
	srv, _, _ := newTestProxy(t, newStaticServer(t.TempDir(), ""), "")
	resp := get(t, srv.URL+"/page/", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/page/ with no id returned %d, want 404", resp.StatusCode)
	}
}

// GOLDEN TEST — the multi-day retention run depends on /page/<id> returning the
// EXACT same bytes and headers for the whole week, because a provider caches
// those bytes and we detect a re-fetch by them not changing. This pins the output
// for a fixed id so any change to the generator fails loudly. Run the suite
// before restarting the tunnelled origin during a retention run. If this test
// must change, a retention run in flight is invalidated — do not change it under
// a live run.
func TestGeneratedPageGolden(t *testing.T) {
	const id = "golden-fixed-id"
	wantBody := "<!doctype html>\n" +
		"<meta charset=\"utf-8\">\n" +
		"<title>probe page golden-fixed-id</title>\n" +
		"<h1>probe page</h1>\n" +
		"<p>status line: this page is live and serving normally.</p>\n" +
		"<p>page id: golden-fixed-id</p>\n" +
		"<p>study reference RCP-2026-08</p>\n"

	if got := renderPage(id); got != wantBody {
		t.Fatalf("renderPage output changed — a retention run would break.\n got: %q\nwant: %q", got, wantBody)
	}

	srv, _, _ := newTestProxy(t, newStaticServer(t.TempDir(), "public, max-age=3600"), "")
	resp := get(t, srv.URL+"/page/"+id, nil)
	if gotBody := body(t, resp); gotBody != wantBody {
		t.Errorf("served body changed:\n got: %q\nwant: %q", gotBody, wantBody)
	}
	if _, present := resp.Header["Cache-Control"]; present {
		t.Errorf("Cache-Control = %q, want none even though the server was started with max-age", resp.Header.Get("Cache-Control"))
	}
	// The ETag is the sha256(body)[:8] hex, so it is pinned by the body above; a
	// stable body means a stable ETag, which is what a provider revalidates on.
	sum := sha256.Sum256([]byte(wantBody))
	wantETag := `"` + hex.EncodeToString(sum[:8]) + `"`
	if et := resp.Header.Get("ETag"); et != wantETag {
		t.Errorf("ETag = %q, want %q (pinned by the golden body)", et, wantETag)
	}
}
