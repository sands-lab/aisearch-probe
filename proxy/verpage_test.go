package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aisearch-probe/internal/probe"
)

// THE CHANGEABLE PAGE (doc/handoff-changeable-page.md). One address whose content
// flips from version 1 (A) to version 2 (B) when the control says so. These tests
// pin the spec before the code exists:
//   - version 1 IS the existing 2 KB depth page, byte for byte (reuse, not a new page)
//   - version N is the same page with the codes made at revision N — nothing else moves
//   - the version is server state set by POST /control/version, never in the request
//   - Last-Modified is the time the current version was set, so a date-only
//     revalidation after a flip is answered with the new page, not a 304
//   - an id never set by the control is a 404; the control is secret-guarded
//   - the state survives a restart (saved to a file), and a missing file is an empty start

const verTestControlSecret = "test-control-secret"

// verServer is a static server with both secrets set and a fake clock, so the
// switch times (and so Last-Modified) are deterministic and a flip lands in a
// later second than the set-up.
func verServer(t *testing.T, statePath string) (*staticServer, *time.Time) {
	t.Helper()
	st := newStaticServer(t.TempDir(), "")
	st.pageCodeSecret = fmtTestSecret
	st.controlSecret = verTestControlSecret
	vs, err := newVersionState(statePath)
	if err != nil {
		t.Fatalf("newVersionState: %v", err)
	}
	clock := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	vs.now = func() time.Time { return clock }
	st.versions = vs
	return st, &clock
}

// setVersion drives the control endpoint the way the runner will and decodes the reply.
func setVersion(t *testing.T, base, id string, version int) versionControlReply {
	t.Helper()
	resp := postForm(t, base+controlVersionPath,
		url.Values{"id": {id}, "version": {strconv.Itoa(version)}},
		map[string]string{"X-Probe-Control-Secret": verTestControlSecret})
	b := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set %s to v%d: status %d: %s", id, version, resp.StatusCode, b)
	}
	var rep versionControlReply
	if err := json.Unmarshal([]byte(b), &rep); err != nil {
		t.Fatalf("control reply is not JSON: %v\n%s", err, b)
	}
	return rep
}

// Version 1 is the existing depth page, byte for byte, in both formats. This is
// what "reuse the depth design" means: the only new thing is which revision the
// codes are made at.
func TestVersionOneIsTheDepthPage(t *testing.T) {
	st, _ := verServer(t, "")
	srv, _, _ := newTestProxy(t, st, "")
	setVersion(t, srv.URL, "cv-a", 1)

	for _, pair := range [][2]string{
		{"/verhtmlpage/cv-a/2048", "/depthhtmlpage/cv-a/2048"},
		{"/vertxtpage/cv-a/2048", "/depthtxtpage/cv-a/2048"},
	} {
		ver := get(t, srv.URL+pair[0], nil)
		dep := get(t, srv.URL+pair[1], nil)
		vb, db := body(t, ver), body(t, dep)
		if ver.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", pair[0], ver.StatusCode)
		}
		if vb != db {
			t.Errorf("%s at version 1 differs from %s — version 1 must be the depth page unchanged", pair[0], pair[1])
		}
		if ver.Header.Get("Content-Type") != dep.Header.Get("Content-Type") {
			t.Errorf("%s Content-Type %q, depth page %q", pair[0], ver.Header.Get("Content-Type"), dep.Header.Get("Content-Type"))
		}
	}
}

// The page is a pure function of (secret, id, version, size): every code of the
// current version is on it, none of the other version's, and fetching twice gives
// the same bytes.
func TestVersionCodesAreOnThePage(t *testing.T) {
	st, _ := verServer(t, "")
	srv, _, _ := newTestProxy(t, st, "")
	setVersion(t, srv.URL, "cv-b", 2)

	for _, path := range []string{"/verhtmlpage/cv-b/2048", "/vertxtpage/cv-b/2048"} {
		first := body(t, get(t, srv.URL+path, nil))
		if again := body(t, get(t, srv.URL+path, nil)); again != first {
			t.Errorf("%s: two fetches of the same version differ", path)
		}
		for _, pct := range probe.DepthPercents {
			if c := probe.DepthCode(fmtTestSecret, "cv-b", pct, 2); !strings.Contains(first, probe.DepthMarkerPrefix+c) {
				t.Errorf("%s: version-2 code at %d%% missing", path, pct)
			}
			if c := probe.DepthCode(fmtTestSecret, "cv-b", pct, 1); strings.Contains(first, c) {
				t.Errorf("%s: version-1 code at %d%% present on the version-2 page", path, pct)
			}
		}
	}
}

// A flip keeps the address and changes the body, every code, the ETag and the
// Last-Modified — and nothing else: the size stays the same and the page outside
// the codes is identical.
func TestFlipChangesCodesTagAndDateOnly(t *testing.T) {
	st, clock := verServer(t, "")
	srv, _, _ := newTestProxy(t, st, "")

	for _, path := range []string{"/verhtmlpage/cv-c/2048", "/vertxtpage/cv-c/2048"} {
		*clock = time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
		setVersion(t, srv.URL, "cv-c", 1)
		a := get(t, srv.URL+path, nil)
		ab := body(t, a)

		*clock = clock.Add(5 * time.Minute)
		setVersion(t, srv.URL, "cv-c", 2)
		b := get(t, srv.URL+path, nil)
		bb := body(t, b)

		if ab == bb {
			t.Fatalf("%s: body unchanged by the flip", path)
		}
		if len(ab) != len(bb) {
			t.Errorf("%s: size changed %d -> %d; only the codes may change", path, len(ab), len(bb))
		}
		// Swap B's codes back to A's: what remains must be A exactly.
		restored := bb
		for _, pct := range probe.DepthPercents {
			restored = strings.ReplaceAll(restored,
				probe.DepthCode(fmtTestSecret, "cv-c", pct, 2), probe.DepthCode(fmtTestSecret, "cv-c", pct, 1))
		}
		if restored != ab {
			t.Errorf("%s: the flip changed more than the codes", path)
		}
		if a.Header.Get("ETag") == b.Header.Get("ETag") {
			t.Errorf("%s: ETag unchanged by the flip", path)
		}
		wantLM := clock.Format(http.TimeFormat)
		if got := b.Header.Get("Last-Modified"); got != wantLM {
			t.Errorf("%s: Last-Modified after flip = %q, want the flip time %q", path, got, wantLM)
		}
	}
}

// After a flip, a fetcher revalidating with what it was told for version 1 — the
// tag, or the date alone — must get version 2, never a 304. A fetcher asking with
// version 2's own tag and date does get a 304: the page is a normal page.
func TestRevalidationAfterFlip(t *testing.T) {
	st, clock := verServer(t, "")
	srv, _, _ := newTestProxy(t, st, "")
	path := srv.URL + "/verhtmlpage/cv-d/2048"

	setVersion(t, srv.URL, "cv-d", 1)
	a := get(t, path, nil)
	body(t, a)
	*clock = clock.Add(5 * time.Minute)
	setVersion(t, srv.URL, "cv-d", 2)

	for name, hdr := range map[string]map[string]string{
		"old tag":       {"If-None-Match": a.Header.Get("ETag")},
		"old date only": {"If-Modified-Since": a.Header.Get("Last-Modified")},
	} {
		resp := get(t, path, hdr)
		got := body(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d, want 200 with version 2", name, resp.StatusCode)
		}
		if !strings.Contains(got, probe.DepthCode(fmtTestSecret, "cv-d", 95, 2)) {
			t.Errorf("%s: answer is not version 2", name)
		}
	}

	b := get(t, path, nil)
	body(t, b)
	resp := get(t, path, map[string]string{"If-None-Match": b.Header.Get("ETag")})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("revalidating version 2 with its own tag: status %d, want 304", resp.StatusCode)
	}
}

// Setting the version a page already has is not a change: the switch time, and so
// Last-Modified, must not move, or a no-op would read as a correction.
func TestSettingSameVersionKeepsSwitchTime(t *testing.T) {
	st, clock := verServer(t, "")
	srv, _, _ := newTestProxy(t, st, "")
	first := setVersion(t, srv.URL, "cv-e", 1)
	*clock = clock.Add(time.Hour)
	again := setVersion(t, srv.URL, "cv-e", 1)
	if again.SwitchedAt != first.SwitchedAt || again.LastModified != first.LastModified {
		t.Errorf("re-setting the same version moved the switch time: %s -> %s", first.SwitchedAt, again.SwitchedAt)
	}
}

// The control's reply carries the ETag of every version for both formats, so the
// runner can label each logged fetch as version 1 or 2. They must be the tags the
// page actually serves.
func TestControlReplyTagsMatchServedPages(t *testing.T) {
	st, clock := verServer(t, "")
	srv, _, _ := newTestProxy(t, st, "")
	setVersion(t, srv.URL, "cv-f", 1)
	tagsAtV1 := map[string]string{}
	for format, prefix := range map[string]string{"html": verHTMLPrefix, "txt": verTxtPrefix} {
		resp := get(t, srv.URL+prefix+"cv-f/2048", nil)
		body(t, resp)
		tagsAtV1[format] = resp.Header.Get("ETag")
	}
	*clock = clock.Add(5 * time.Minute)
	rep := setVersion(t, srv.URL, "cv-f", 2)

	if rep.ID != "cv-f" || rep.Version != 2 || rep.Size != 2048 {
		t.Errorf("reply = %+v, want id cv-f, version 2, size 2048", rep)
	}
	for format, prefix := range map[string]string{"html": verHTMLPrefix, "txt": verTxtPrefix} {
		resp := get(t, srv.URL+prefix+"cv-f/2048", nil)
		body(t, resp)
		if got := rep.ETags[format]["2"]; got != resp.Header.Get("ETag") {
			t.Errorf("%s v2 tag in reply %q, served %q", format, got, resp.Header.Get("ETag"))
		}
		if got := rep.ETags[format]["1"]; got != tagsAtV1[format] {
			t.Errorf("%s v1 tag in reply %q, served %q", format, got, tagsAtV1[format])
		}
	}
}

// An id the control never set is a 404 on both formats: a typo or a page lost to a
// restart must be visible, never a silent version 1.
func TestUnknownIDIs404(t *testing.T) {
	st, _ := verServer(t, "")
	srv, _, _ := newTestProxy(t, st, "")
	for _, path := range []string{"/verhtmlpage/never-set/2048", "/vertxtpage/never-set/2048"} {
		resp := get(t, srv.URL+path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, resp.StatusCode)
		}
	}
}

// Malformed addresses are 404 too (no size, extra segment), and a size too small
// to hold the markers is refused like the depth pages refuse it.
func TestMalformedAddresses(t *testing.T) {
	st, _ := verServer(t, "")
	srv, _, _ := newTestProxy(t, st, "")
	setVersion(t, srv.URL, "cv-g", 1)
	for path, want := range map[string]int{
		"/verhtmlpage/cv-g":        http.StatusNotFound,
		"/verhtmlpage/cv-g/2048/x": http.StatusNotFound,
		"/vertxtpage/cv-g/abc":     http.StatusNotFound,
		"/verhtmlpage/cv-g/100":    http.StatusBadRequest,
	} {
		resp := get(t, srv.URL+path, nil)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: status %d, want %d", path, resp.StatusCode, want)
		}
	}
}

// The control is guarded: off (404) with no control secret, 403 on a wrong secret,
// POST only, and it refuses a missing id or a version outside 1..9 without
// changing anything.
func TestVersionControlGuards(t *testing.T) {
	off := newStaticServer(t.TempDir(), "")
	off.pageCodeSecret = fmtTestSecret
	offSrv, _, _ := newTestProxy(t, off, "")
	resp := postForm(t, offSrv.URL+controlVersionPath, url.Values{"id": {"x"}, "version": {"1"}}, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("control with no secret configured: status %d, want 404", resp.StatusCode)
	}

	st, _ := verServer(t, "")
	srv, _, _ := newTestProxy(t, st, "")
	good := map[string]string{"X-Probe-Control-Secret": verTestControlSecret}
	cases := []struct {
		name string
		form url.Values
		hdr  map[string]string
		want int
	}{
		{"wrong secret", url.Values{"id": {"cv-h"}, "version": {"1"}}, map[string]string{"X-Probe-Control-Secret": "nope"}, http.StatusForbidden},
		{"no secret", url.Values{"id": {"cv-h"}, "version": {"1"}}, nil, http.StatusForbidden},
		{"missing id", url.Values{"version": {"1"}}, good, http.StatusBadRequest},
		{"id with slash", url.Values{"id": {"a/b"}, "version": {"1"}}, good, http.StatusBadRequest},
		{"missing version", url.Values{"id": {"cv-h"}}, good, http.StatusBadRequest},
		{"version 0", url.Values{"id": {"cv-h"}, "version": {"0"}}, good, http.StatusBadRequest},
		{"version 10", url.Values{"id": {"cv-h"}, "version": {"10"}}, good, http.StatusBadRequest},
		{"version word", url.Values{"id": {"cv-h"}, "version": {"B"}}, good, http.StatusBadRequest},
	}
	for _, c := range cases {
		resp := postForm(t, srv.URL+controlVersionPath, c.form, c.hdr)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
	// None of the refused calls may have created the page.
	if r := get(t, srv.URL+"/verhtmlpage/cv-h/2048", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("a refused control call created the page (status %d)", r.StatusCode)
	}

	resp = get(t, srv.URL+controlVersionPath, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET on the control: status %d, want 405", resp.StatusCode)
	}
}

// Without the page-code secret the page cannot make unforgeable codes, so it
// refuses (503) like every coded route — even for an id the state knows.
func TestVersionPageNeedsPageSecret(t *testing.T) {
	st, _ := verServer(t, "")
	st.pageCodeSecret = ""
	if _, err := st.versions.set("cv-i", 1); err != nil {
		t.Fatal(err)
	}
	srv, _, _ := newTestProxy(t, st, "")
	resp := get(t, srv.URL+"/verhtmlpage/cv-i/2048", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503 without the page-code secret", resp.StatusCode)
	}
}

// The page fetch is logged like any GET, with the tag that tells version 1 from 2;
// the control POST is not logged as a visit.
func TestVersionFetchIsLoggedAndControlIsNot(t *testing.T) {
	st, clock := verServer(t, "")
	srv, read, _ := newTestProxy(t, st, "")
	setVersion(t, srv.URL, "cv-j", 1)
	*clock = clock.Add(5 * time.Minute)
	rep := setVersion(t, srv.URL, "cv-j", 2)
	resp := get(t, srv.URL+"/verhtmlpage/cv-j/2048", nil)
	body(t, resp)

	recs := read()
	if len(recs) != 1 {
		t.Fatalf("logged %d records, want exactly the one page GET", len(recs))
	}
	if recs[0].PageKey != "/verhtmlpage/cv-j/2048" || recs[0].ETagSent != rep.ETags["html"]["2"] {
		t.Errorf("logged %q tag %q, want the page with version 2's tag %q", recs[0].PageKey, recs[0].ETagSent, rep.ETags["html"]["2"])
	}
}

// The state survives a restart: a second server reading the same file serves the
// same version with the same Last-Modified.
func TestVersionStateSurvivesRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "verpage-state.json")
	st, clock := verServer(t, statePath)
	srv, _, _ := newTestProxy(t, st, "")
	setVersion(t, srv.URL, "cv-k", 1)
	*clock = clock.Add(5 * time.Minute)
	setVersion(t, srv.URL, "cv-k", 2)
	before := get(t, srv.URL+"/verhtmlpage/cv-k/2048", nil)
	beforeBody := body(t, before)

	st2, _ := verServer(t, statePath) // the "restarted" origin
	srv2, _, _ := newTestProxy(t, st2, "")
	after := get(t, srv2.URL+"/verhtmlpage/cv-k/2048", nil)
	if got := body(t, after); got != beforeBody {
		t.Error("after a restart the page serves different bytes")
	}
	if after.Header.Get("Last-Modified") != before.Header.Get("Last-Modified") {
		t.Errorf("Last-Modified after restart %q, before %q", after.Header.Get("Last-Modified"), before.Header.Get("Last-Modified"))
	}
}

// Backward compatibility of the state file: no file is an empty start (an origin
// that never used the page starts exactly as before), unknown fields are ignored,
// and a file that cannot be read as state is an error at start-up — never a silent
// empty start that would 404 a live run's pages.
func TestVersionStateFile(t *testing.T) {
	dir := t.TempDir()

	vs, err := newVersionState(filepath.Join(dir, "absent.json"))
	if err != nil {
		t.Fatalf("missing file must be an empty start, got %v", err)
	}
	if _, ok := vs.get("anything"); ok {
		t.Error("missing file produced pages")
	}

	extra := filepath.Join(dir, "extra.json")
	os.WriteFile(extra, []byte(`{"format":1,"future_field":true,"pages":{"cv-x":{"version":2,"switched_at":"2026-10-13T09:05:00Z","note":"later"}}}`), 0o644)
	vs, err = newVersionState(extra)
	if err != nil {
		t.Fatalf("unknown fields must be ignored, got %v", err)
	}
	if e, ok := vs.get("cv-x"); !ok || e.Version != 2 || !e.SwitchedAt.Equal(time.Date(2026, 10, 13, 9, 5, 0, 0, time.UTC)) {
		t.Errorf("read back %+v ok=%v", e, ok)
	}

	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("not json"), 0o644)
	if _, err := newVersionState(bad); err == nil {
		t.Error("a corrupt state file must be an error, not an empty start")
	}

	// An empty path is memory-only: nothing is written anywhere.
	mem, err := newVersionState("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.set("cv-y", 1); err != nil {
		t.Fatal(err)
	}
}
