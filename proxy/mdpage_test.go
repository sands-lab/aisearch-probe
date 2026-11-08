package main

import (
	"net/http"
	"strings"
	"testing"
)

// /mdpage/ is the Markdown arm — relevant because so much AI-facing content is now
// served as Markdown. Same facts, served as text/markdown with a heading. Per-page
// code + id present, stamped, 304, realistic header, deterministic bytes.
func TestMdPageIsStampedAndCarriesMarker(t *testing.T) {
	srv, read, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")

	const id = "r0915-md-a"
	resp := get(t, srv.URL+"/mdpage/"+id, nil)
	got := body(t, resp)

	if resp.Header.Get("ETag") == "" {
		t.Error("Markdown page has no ETag — a crawler could not revalidate it")
	}
	if _, present := resp.Header["Cache-Control"]; present {
		t.Errorf("Markdown page sent Cache-Control %q, want it omitted (realistic header)", resp.Header.Get("Cache-Control"))
	}
	if !strings.Contains(got, "# ") {
		t.Errorf("Markdown page has no heading — does not read as Markdown:\n%s", got)
	}
	if code := fmtRefCode(id); !strings.Contains(got, code) {
		t.Errorf("Markdown page is missing its per-page code %q:\n%s", code, got)
	}
	if !strings.Contains(got, id) {
		t.Errorf("Markdown page is missing its id:\n%s", got)
	}
	recs := read()
	if len(recs) != 1 || !recs[0].Stamped() {
		t.Fatalf("the log must show the Markdown page as stamped: %+v", recs)
	}
}

func TestMdPageAnswers304(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")
	first := get(t, srv.URL+"/mdpage/r0915-md-b", nil)
	first.Body.Close()
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on first fetch")
	}
	second := get(t, srv.URL+"/mdpage/r0915-md-b", map[string]string{"If-None-Match": etag})
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation returned %d, want 304", second.StatusCode)
	}
}

func TestMdPageBareIs404(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")
	resp := get(t, srv.URL+"/mdpage/", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/mdpage/ with no id returned %d, want 404", resp.StatusCode)
	}
}

func TestMdPageDeterministic(t *testing.T) {
	if string(renderMdPage("same-id", "code123")) != string(renderMdPage("same-id", "code123")) {
		t.Error("renderMdPage is not deterministic")
	}
}
