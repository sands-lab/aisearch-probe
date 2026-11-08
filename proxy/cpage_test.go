package main

import (
	"net/http"
	"strings"
	"testing"

	"aisearch-probe/internal/probe"
)

func cpageServer(t *testing.T, secret string) *staticServer {
	t.Helper()
	st := newStaticServer(t.TempDir(), "public, max-age=3600")
	st.pageCodeSecret = secret
	return st
}

// A coded page carries the subject and BOTH codes, and caches like a normal page
// (ETag + Last-Modified, no Cache-Control), so a provider stores it as it would any page.
func TestServeCpageHasSubjectAndBothCodes(t *testing.T) {
	const secret = "test-page-code-secret"
	srv, read, _ := newTestProxy(t, cpageServer(t, secret), "")

	id := "cr05-abc12"
	resp := get(t, srv.URL+"/cpage/"+id, nil)
	got := body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", resp.StatusCode, got)
	}
	subject, _ := probe.CpageSubjectForID(id)
	ref := probe.CpageCode(secret, id, probe.CpageFieldRef, 1)
	rec := probe.CpageCode(secret, id, probe.CpageFieldRec, 1)
	for _, want := range []string{subject, ref, rec, "revision: 1", id} {
		if !strings.Contains(got, want) {
			t.Errorf("cpage body missing %q:\n%s", want, got)
		}
	}
	if resp.Header.Get("ETag") == "" || resp.Header.Get("Last-Modified") == "" {
		t.Error("cpage must carry ETag + Last-Modified so it caches like a normal page")
	}
	if _, present := resp.Header["Cache-Control"]; present {
		t.Errorf("Cache-Control = %q, want none (the normal page)", resp.Header.Get("Cache-Control"))
	}
	if recs := read(); len(recs) != 1 || !recs[0].Stamped() {
		t.Fatalf("the log must show the cpage as one stamped fetch: %+v", recs)
	}
}

// The real-prose body must never describe the coded-page scheme or this
// experiment: our own study notes spell out the warm/blind/record-number method,
// and serving that would hand the model the trick. The sanitised corpus, and any
// page rendered from it, must contain none of the reserved terms.
func TestCpageCorpusHasNoSelfDescription(t *testing.T) {
	corpus := cpageCorpus()
	if len(corpus) < 1000 {
		t.Fatalf("sanitised cpage corpus is implausibly small (%d bytes) — over-dropped?", len(corpus))
	}
	low := strings.ToLower(corpus)
	for _, term := range cpageDropTerms {
		if strings.Contains(low, term) {
			t.Errorf("sanitised cpage corpus still contains a scheme-describing term %q", term)
		}
	}
	// And a rendered page body must be clean too (excluding the field labels the
	// page legitimately carries, which are not part of the prose body).
	body := strings.ToLower(cpageProse("cr03-selftest-xyz"))
	for _, term := range []string{"content-reuse", "never named", "internal store", "record number"} {
		if strings.Contains(body, term) {
			t.Errorf("rendered cpage prose leaks %q:\n%s", term, body)
		}
	}
}

func TestServeCpageAnswers304(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, "s3cr3t"), "")
	first := get(t, srv.URL+"/cpage/cr01-xy", nil)
	first.Body.Close()
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on first fetch")
	}
	second := get(t, srv.URL+"/cpage/cr01-xy", map[string]string{"If-None-Match": etag})
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation returned %d, want 304", second.StatusCode)
	}
}

func TestServeCpageBadIdIs404(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, "s3cr3t"), "")
	for _, path := range []string{"/cpage/", "/cpage/nope", "/cpage/cr9999-x", "/cpage/cr01/extra"} {
		resp := get(t, srv.URL+path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s returned %d, want 404", path, resp.StatusCode)
		}
	}
}

// Without a secret the route must refuse, never print a guessable page.
func TestServeCpageRefusesWithoutSecret(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, ""), "")
	resp := get(t, srv.URL+"/cpage/cr01-xy", nil)
	got := body(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no secret: status = %d, want 503", resp.StatusCode)
	}
	if strings.Contains(got, "reference code") || strings.Contains(got, "record number") {
		t.Error("no secret must not print any code")
	}
}

// The new route must NOT change /page/, whose bytes the retention run depends on.
func TestCpageLeavesPageRouteUntouched(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, "s3cr3t"), "")
	resp := get(t, srv.URL+"/page/golden-fixed-id", nil)
	got := body(t, resp)
	if !strings.Contains(got, "RCP-2026-08") {
		t.Errorf("/page/ must still carry the shared marker:\n%s", got)
	}
	if strings.Contains(got, "record number") || strings.Contains(got, "subject:") {
		t.Error("/page/ must NOT gain cpage fields")
	}
}
