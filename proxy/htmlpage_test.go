package main

import (
	"net/http"
	"strings"
	"testing"
)

// /htmlpage/ is the HTML BASELINE of the object-type study: a real HTML page under a
// NORMAL header (no Cache-Control), carrying the PER-PAGE unforgeable code so it
// is symmetric with the other format arms. Code + id present, stamped, 304, header
// omitted, deterministic bytes.
func TestHTMLPageIsStampedAndCarriesMarker(t *testing.T) {
	srv, read, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")

	const id = "r0915-html-a"
	resp := get(t, srv.URL+"/htmlpage/"+id, nil)
	got := body(t, resp)

	if resp.Header.Get("ETag") == "" {
		t.Error("HTML page has no ETag — a crawler could not revalidate it")
	}
	// The baseline must be a NORMAL header, not no-store — that is why it is a new
	// route and not /page/.
	if _, present := resp.Header["Cache-Control"]; present {
		t.Errorf("HTML baseline sent Cache-Control %q, want it omitted (realistic header, not no-store)", resp.Header.Get("Cache-Control"))
	}
	if code := fmtRefCode(id); !strings.Contains(got, code) {
		t.Errorf("HTML page is missing its per-page code %q:\n%s", code, got)
	}
	if !strings.Contains(got, id) {
		t.Errorf("HTML page is missing its id:\n%s", got)
	}
	recs := read()
	if len(recs) != 1 || !recs[0].Stamped() {
		t.Fatalf("the log must show the HTML page as stamped: %+v", recs)
	}
}

func TestHTMLPageAnswers304(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")
	first := get(t, srv.URL+"/htmlpage/r0915-html-b", nil)
	first.Body.Close()
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on first fetch")
	}
	second := get(t, srv.URL+"/htmlpage/r0915-html-b", map[string]string{"If-None-Match": etag})
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation returned %d, want 304", second.StatusCode)
	}
}

func TestHTMLPageBareIs404(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")
	resp := get(t, srv.URL+"/htmlpage/", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/htmlpage/ with no id returned %d, want 404", resp.StatusCode)
	}
}

func TestHTMLPageDeterministic(t *testing.T) {
	if string(renderHTMLPage("same-id", "code123")) != string(renderHTMLPage("same-id", "code123")) {
		t.Error("renderHTMLPage is not deterministic")
	}
}
