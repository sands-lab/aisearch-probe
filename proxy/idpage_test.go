package main

import (
	"net/http"
	"strings"
	"testing"

	"aisearch-probe/internal/probe"
)

// THE PERSONAL PAGE (doc/handoff-url-equivalence-and-leak.md, extra 1). One page
// whose codes depend on ONE named identity item in the query, so the copy made for
// ?utm_content=user-1 is "personal" to user-1. A stranger who is handed that copy
// for a different address quotes user-1's codes — the leak the test looks for.
// These tests pin the spec before the code exists:
//   - the codes follow (id, name, value) and nothing else in the request
//   - no value (missing or empty) is the "anonymous" page, never someone's page
//   - normal headers (D13): no Cache-Control, no Expires, a validator
//   - malformed paths are 404s; no secret is a 503

func idServer(t *testing.T, secret string) string {
	t.Helper()
	st := newStaticServer(t.TempDir(), "")
	st.pageCodeSecret = secret
	srv, _, _ := newTestProxy(t, st, "")
	return srv.URL
}

func idCodes(id, name, value string) []string {
	var out []string
	for _, pct := range probe.DepthPercents {
		out = append(out, probe.DepthCode(fmtTestSecret, probe.IDPageCodeID(id, name, value), pct, probe.DepthPageRevision))
	}
	return out
}

func TestIDPageCodesFollowTheIdentityValue(t *testing.T) {
	base := idServer(t, fmtTestSecret)
	resp := get(t, base+"/idpage/ix/utm_content/2048?utm_content=user-1", nil)
	b := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	for _, c := range idCodes("ix", "utm_content", "user-1") {
		if !strings.Contains(b, c) {
			t.Errorf("user-1's page is missing its code %s", c)
		}
	}
	for _, other := range [][]string{idCodes("ix", "utm_content", "user-2"), idCodes("ix", "utm_content", "")} {
		for _, c := range other {
			if strings.Contains(b, c) {
				t.Errorf("user-1's page carries another identity's code %s", c)
			}
		}
	}
	// The page is the depth HTML page, laid out for this identity.
	want := string(renderDepthHTML(depthFormatLinesRev(fmtTestSecret, probe.IDPageCodeID("ix", "utm_content", "user-1"), 2048, true, probe.DepthPageRevision)))
	if b != want {
		t.Error("the page is not the depth HTML page made for this identity")
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type %q", ct)
	}
}

// Two people get two different pages (and so two different ETags); the same
// person asking twice gets the same page.
func TestIDPageDiffersPerPersonAndIsStable(t *testing.T) {
	base := idServer(t, fmtTestSecret)
	a1 := get(t, base+"/idpage/ix/mc_eid/2048?mc_eid=user-1", nil)
	a2 := get(t, base+"/idpage/ix/mc_eid/2048?mc_eid=user-1", nil)
	bb := get(t, base+"/idpage/ix/mc_eid/2048?mc_eid=user-2", nil)
	ba1, ba2, bbb := body(t, a1), body(t, a2), body(t, bb)
	if ba1 != ba2 || a1.Header.Get("ETag") != a2.Header.Get("ETag") {
		t.Error("the same identity got two different pages")
	}
	if ba1 == bbb || a1.Header.Get("ETag") == bb.Header.Get("ETag") {
		t.Error("two identities got the same page — nothing would be personal")
	}
}

// Only the NAMED item counts. Other items (a probe token, a tracking tag that is
// not the identity, a different order) must not change the page, or a difference
// in the answer could come from them instead of from the identity.
func TestIDPageIgnoresEveryOtherQueryItem(t *testing.T) {
	base := idServer(t, fmtTestSecret)
	plain := body(t, get(t, base+"/idpage/ix/uid/2048?uid=user-1", nil))
	for _, u := range []string{
		"/idpage/ix/uid/2048?uid=user-1&x=1",
		"/idpage/ix/uid/2048?x=1&uid=user-1",
		"/idpage/ix/uid/2048?utm_source=email&uid=user-1",
	} {
		if got := body(t, get(t, base+u, nil)); got != plain {
			t.Errorf("%s is not the same page as ?uid=user-1", u)
		}
	}
}

// No identity — the bare page, or the name with an empty value — is the
// anonymous page: codes nobody owns. It must never be someone's page.
func TestIDPageWithoutAnIdentityIsAnonymous(t *testing.T) {
	base := idServer(t, fmtTestSecret)
	bare := body(t, get(t, base+"/idpage/ix/_hsenc/2048", nil))
	empty := body(t, get(t, base+"/idpage/ix/_hsenc/2048?_hsenc=", nil))
	other := body(t, get(t, base+"/idpage/ix/_hsenc/2048?fbclid=user-1", nil))
	for _, c := range idCodes("ix", "_hsenc", "") {
		if !strings.Contains(bare, c) {
			t.Errorf("the bare page is missing the anonymous code %s", c)
		}
	}
	if empty != bare || other != bare {
		t.Error("an empty value, or only a different item, must give the anonymous page")
	}
	if probe.IDPageCodeID("ix", "_hsenc", "") == probe.IDPageCodeID("ix", "_hsenc", "anonymous") {
		t.Error("a person called \"anonymous\" would get the anonymous page")
	}
}

// The normal page (DECISIONS.md § D13): no caching header, a validator present.
func TestIDPageSendsNormalHeaders(t *testing.T) {
	base := idServer(t, fmtTestSecret)
	resp := get(t, base+"/idpage/ix/fbclid/2048?fbclid=user-1", nil)
	body(t, resp)
	if cc := resp.Header.Get("Cache-Control"); cc != "" {
		t.Errorf("Cache-Control %q — the personal page must send none", cc)
	}
	if ex := resp.Header.Get("Expires"); ex != "" {
		t.Errorf("Expires %q", ex)
	}
	if resp.Header.Get("ETag") == "" || resp.Header.Get("Last-Modified") == "" {
		t.Error("missing ETag or Last-Modified")
	}
}

func TestIDPageRefusesMalformedPaths(t *testing.T) {
	base := idServer(t, fmtTestSecret)
	for _, p := range []string{
		"/idpage/",
		"/idpage/ix",
		"/idpage/ix/uid",
		"/idpage/ix/uid/",
		"/idpage/ix/uid/abc",
		"/idpage/ix/uid/2048/extra",
		"/idpage//uid/2048",
		"/idpage/ix//2048",
		"/idpage/ix/u%20id/2048",
	} {
		resp := get(t, base+p, nil)
		body(t, resp)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s answered 200", p)
		}
	}
	resp := get(t, base+"/idpage/ix/uid/100?uid=user-1", nil)
	body(t, resp)
	if resp.StatusCode == http.StatusOK {
		t.Error("a page too small to hold the markers answered 200")
	}
}

func TestIDPageNeedsTheSecret(t *testing.T) {
	base := idServer(t, "")
	resp := get(t, base+"/idpage/ix/uid/2048?uid=user-1", nil)
	body(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d without the secret, want 503", resp.StatusCode)
	}
}
