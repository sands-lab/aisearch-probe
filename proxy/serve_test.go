package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func writeSite(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// The built-in origin exists to make the experiment valid by construction, so
// its whole job is stamping every response.
func TestServeModeStampsEveryResponse(t *testing.T) {
	dir := writeSite(t, map[string]string{"index.html": "<h1>hello</h1>"})
	srv, read, _ := newTestProxy(t, newStaticServer(dir, "public, max-age=3600"), "")

	resp := get(t, srv.URL+"/index.html", nil)
	resp.Body.Close()

	if resp.Header.Get("ETag") == "" {
		t.Error("no ETag — a crawler would have nothing to revalidate against")
	}
	if resp.Header.Get("Last-Modified") == "" {
		t.Error("no Last-Modified")
	}
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=3600" {
		t.Errorf("Cache-Control = %q", got)
	}

	recs := read()
	if len(recs) != 1 || !recs[0].Stamped() {
		t.Fatalf("the log must show the response as stamped: %+v", recs)
	}
}

// It must actually honour "has this changed?" — otherwise a polite crawler
// gets a full body anyway and looks wasteful in the report.
func TestServeModeAnswers304(t *testing.T) {
	dir := writeSite(t, map[string]string{"page.html": "<h1>hello</h1>"})
	srv, _, _ := newTestProxy(t, newStaticServer(dir, ""), "")

	first := get(t, srv.URL+"/page.html", nil)
	first.Body.Close()
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the first fetch")
	}

	second := get(t, srv.URL+"/page.html", map[string]string{"If-None-Match": etag})
	second.Body.Close()
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("status = %d, want 304 when the ETag still matches", second.StatusCode)
	}
}

// The ETag must follow the content, or a changed page would keep serving 304s.
func TestServeModeETagChangesWithContent(t *testing.T) {
	dir := writeSite(t, map[string]string{"page.html": "one"})
	srv, _, _ := newTestProxy(t, newStaticServer(dir, ""), "")

	r1 := get(t, srv.URL+"/page.html", nil)
	r1.Body.Close()
	first := r1.Header.Get("ETag")

	if err := os.WriteFile(filepath.Join(dir, "page.html"), []byte("two"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	r2 := get(t, srv.URL+"/page.html", nil)
	r2.Body.Close()
	if second := r2.Header.Get("ETag"); second == first {
		t.Error("ETag did not change when the content did — updated pages would serve stale 304s")
	}
}

func TestServeModeDirectoryIndexAndMissingFile(t *testing.T) {
	dir := writeSite(t, map[string]string{"docs/index.html": "<h1>docs</h1>"})
	srv, _, _ := newTestProxy(t, newStaticServer(dir, ""), "")

	ok := get(t, srv.URL+"/docs/", nil)
	ok.Body.Close()
	if ok.StatusCode != 200 {
		t.Errorf("directory request = %d, want 200 via index.html", ok.StatusCode)
	}

	missing := get(t, srv.URL+"/nope.html", nil)
	missing.Body.Close()
	if missing.StatusCode != 404 {
		t.Errorf("missing file = %d, want 404", missing.StatusCode)
	}
}

// Serving files must not let a request climb out of the directory.
func TestServeModeRejectsPathTraversal(t *testing.T) {
	dir := writeSite(t, map[string]string{"page.html": "safe"})
	secret := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(secret, []byte("do not serve me"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	defer os.Remove(secret)

	srv, _, _ := newTestProxy(t, newStaticServer(dir, ""), "")
	for _, p := range []string{"/../secret.txt", "/..%2fsecret.txt", "/docs/../../secret.txt"} {
		resp := get(t, srv.URL+p, nil)
		body := make([]byte, 64)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		if resp.StatusCode == 200 && string(body[:n]) == "do not serve me" {
			t.Errorf("%s escaped the served directory", p)
		}
	}
}

// End to end in serve mode: a polite and a naive client, judged correctly,
// with no external origin involved at all.
func TestServeModeEndToEndVerdicts(t *testing.T) {
	body := "<h1>the probe page</h1>"
	dir := writeSite(t, map[string]string{"page.html": body})

	politeURL, politeAnalyse := e2eSetupBackend(t, newStaticServer(dir, "public, max-age=3600"))
	naiveURL, naiveAnalyse := e2eSetupBackend(t, newStaticServer(dir, "public, max-age=3600"))

	polite, naive := newPoliteClient(), newNaiveClient()
	for i := 0; i < 3; i++ {
		polite.fetch(t, politeURL+"/page.html")
		naive.fetch(t, naiveURL+"/page.html")
	}

	good := reportFor(t, politeAnalyse(), "unknown")
	bad := reportFor(t, naiveAnalyse(), "unknown")

	if good.ReturnUnstamped != 0 || bad.ReturnUnstamped != 0 {
		t.Errorf("built-in origin left visits unstamped: polite=%d naive=%d", good.ReturnUnstamped, bad.ReturnUnstamped)
	}
	if good.AskedPct() != 100 || good.WastedBytes != 0 {
		t.Errorf("polite client judged wrongly: asked=%v wasted=%d", good.AskedPct(), good.WastedBytes)
	}
	if bad.AskedPct() != 0 || bad.WastedBytes != int64(2*len(body)) {
		t.Errorf("naive client judged wrongly: asked=%v wasted=%d want %d", bad.AskedPct(), bad.WastedBytes, 2*len(body))
	}
}

// EVERY SPELLING OF AN ADDRESS MUST RETURN THE SAME BYTES.
//
// The address-variation experiment hands a provider /vary.html and then
// /VARY.html, /vary.html/, /./vary.html and so on, and reads our server log to
// see whether a second download arrives. If our own server answered some of
// those with 404 the page's text would never reach the model, so the second,
// independent confirmation — the page's own word coming back in the answer —
// would be lost on exactly the rows that matter.
//
// It changes nothing about the measurement itself: a provider decides whether
// to fetch before it learns what we answer.
func TestServeModeAnswersEverySpellingOfOneAddress(t *testing.T) {
	dir := writeSite(t, map[string]string{"vary.html": "<h1>register</h1>ORCHID"})
	srv, _, _ := newTestProxy(t, newStaticServer(dir, ""), "")

	want := ""
	for _, path := range []string{
		"/vary.html",
		"/vary.html/",     // trailing slash
		"/VARY.html",      // the file name in capitals
		"/Vary.HTML",      // mixed capitals
		"/./vary.html",    // a pointless "go here" step
		"/x/../vary.html", // a step up and back down
	} {
		resp := get(t, srv.URL+path, nil)
		raw, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			t.Fatalf("read %s: %v", path, rerr)
		}
		body := string(raw)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s → %d, want 200; the model would never see the page at this spelling", path, resp.StatusCode)
			continue
		}
		if want == "" {
			want = body
			continue
		}
		if body != want {
			t.Errorf("%s returned different bytes from /vary.html", path)
		}
	}
}

// A missing file must still be a 404. The case-insensitive fallback is there to
// serve one page under several spellings, not to invent pages.
func TestServeModeStillReturnsNotFoundForAMissingPage(t *testing.T) {
	dir := writeSite(t, map[string]string{"vary.html": "x"})
	srv, _, _ := newTestProxy(t, newStaticServer(dir, ""), "")

	resp := get(t, srv.URL+"/nosuchpage.html", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing page → %d, want 404", resp.StatusCode)
	}
}
