package main

import (
	"net/http"
	"strings"
	"testing"
)

// /txtpage/ is the plain-text arm: the "pure text, no structure" control. Same
// facts, served as text/plain. Per-page code + id present, stamped, 304, realistic
// header (Cache-Control omitted), deterministic bytes.
func TestTxtPageIsStampedAndCarriesMarker(t *testing.T) {
	srv, read, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")

	const id = "r0915-txt-a"
	resp := get(t, srv.URL+"/txtpage/"+id, nil)
	got := body(t, resp)

	if resp.Header.Get("ETag") == "" {
		t.Error("text page has no ETag — a crawler could not revalidate it")
	}
	if _, present := resp.Header["Cache-Control"]; present {
		t.Errorf("text page sent Cache-Control %q, want it omitted (realistic header)", resp.Header.Get("Cache-Control"))
	}
	if code := fmtRefCode(id); !strings.Contains(got, code) {
		t.Errorf("text page is missing its per-page code %q:\n%s", code, got)
	}
	if !strings.Contains(got, id) {
		t.Errorf("text page is missing its id:\n%s", got)
	}
	recs := read()
	if len(recs) != 1 || !recs[0].Stamped() {
		t.Fatalf("the log must show the text page as stamped: %+v", recs)
	}
}

func TestTxtPageAnswers304(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")
	first := get(t, srv.URL+"/txtpage/r0915-txt-b", nil)
	first.Body.Close()
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on first fetch")
	}
	second := get(t, srv.URL+"/txtpage/r0915-txt-b", map[string]string{"If-None-Match": etag})
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation returned %d, want 304", second.StatusCode)
	}
}

func TestTxtPageBareIs404(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")
	resp := get(t, srv.URL+"/txtpage/", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/txtpage/ with no id returned %d, want 404", resp.StatusCode)
	}
}

func TestTxtPageDeterministic(t *testing.T) {
	if string(renderTxtPage("same-id", "code123")) != string(renderTxtPage("same-id", "code123")) {
		t.Error("renderTxtPage is not deterministic")
	}
}
