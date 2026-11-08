package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// /jsonpage/ is the JSON arm of the object-type study: same page facts as /page/,
// but served as a real JSON document so we can ask whether a fetcher fetches,
// reads, and caches JSON the way it does HTML. The body must be valid JSON that
// carries the PER-PAGE unforgeable code as a field value and the id, be a pure
// function of the id (stable ETag), revalidate with a 304, and send a realistic
// header (no Cache-Control at all — NOT no-store).
func TestJSONPageIsStampedValidAndCarriesMarker(t *testing.T) {
	srv, read, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")

	const id = "r0915-json-a"
	resp := get(t, srv.URL+"/jsonpage/"+id, nil)
	got := body(t, resp)

	if resp.Header.Get("ETag") == "" {
		t.Error("JSON page has no ETag — a crawler could not revalidate it")
	}
	// Realistic header on the new format routes means Cache-Control is OMITTED, not
	// no-store (caching headers are reserved for the /ccdepthpage/ test).
	if _, present := resp.Header["Cache-Control"]; present {
		t.Errorf("JSON page sent Cache-Control %q, want it omitted (realistic header)", resp.Header.Get("Cache-Control"))
	}
	if !json.Valid([]byte(got)) {
		t.Errorf("JSON page body is not valid JSON:\n%s", got)
	}
	if code := fmtRefCode(id); !strings.Contains(got, code) {
		t.Errorf("JSON page is missing its per-page code %q:\n%s", code, got)
	}
	if !strings.Contains(got, id) {
		t.Errorf("JSON page is missing its id:\n%s", got)
	}
	recs := read()
	if len(recs) != 1 || !recs[0].Stamped() {
		t.Fatalf("the log must show the JSON page as stamped: %+v", recs)
	}
}

func TestJSONPageAnswers304(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")
	first := get(t, srv.URL+"/jsonpage/r0915-json-b", nil)
	first.Body.Close()
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on first fetch")
	}
	second := get(t, srv.URL+"/jsonpage/r0915-json-b", map[string]string{"If-None-Match": etag})
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation returned %d, want 304", second.StatusCode)
	}
}

func TestJSONPageBareIs404(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")
	resp := get(t, srv.URL+"/jsonpage/", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/jsonpage/ with no id returned %d, want 404", resp.StatusCode)
	}
}

// The body is a pure function of (id, code), so two renders of the same inputs are
// byte-identical — the ETag over the bytes is stable across the run.
func TestJSONPageDeterministic(t *testing.T) {
	a := renderJSONPage("same-id", "code123")
	b := renderJSONPage("same-id", "code123")
	if string(a) != string(b) {
		t.Errorf("renderJSONPage is not deterministic:\n a: %q\n b: %q", a, b)
	}
}
