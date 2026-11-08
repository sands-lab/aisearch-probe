package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aisearch-probe/internal/probe"
)

// fmtTestSecret is the page-code secret the object-type format route tests run with,
// and fmtRefCode is the per-page code those routes print for a given id — recomputed
// here exactly as the driver would, so the tests check the real unforgeable code, not
// a shared marker.
const fmtTestSecret = "test-page-code-secret"

func fmtRefCode(id string) string {
	return probe.CpageCode(fmtTestSecret, id, probe.CpageFieldRef, probe.FormatPageRevision)
}

// The per-route behaviour is covered end-to-end by pages_test / sizepage_test /
// depthpage_test / cpage_test / ccdepth_test. These tests pin the SHARED machinery
// the routes now run through, so a regression in the spine is caught on its own.
// The caching-header rules (and the leak guard) live in pageheaders_test.go.

// The content type is now per-route: writePage must send exactly the type build
// returns. A route that serves JSON/PDF/etc. relies on this, and the 5 HTML routes
// rely on it still being text/html. An empty type is the one exception — writePage
// lets ServeContent sniff one rather than send a blank type.
func TestWritePageSetsContentType(t *testing.T) {
	cases := []struct {
		name string
		ct   string
	}{
		{"html type", "text/html; charset=utf-8"},
		{"json type", "application/json"},
		{"pdf type", "application/pdf"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/x/y", nil)
			writePage(w, r, "y", c.ct, pageHeaders{}, []byte("some bytes here"))
			if got := w.Result().Header.Get("Content-Type"); got != c.ct {
				t.Errorf("Content-Type = %q, want %q", got, c.ct)
			}
		})
	}

	// Empty type: writePage must not pin a blank type; ServeContent sniffs one.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x/y.txt", nil)
	writePage(w, r, "y.txt", "", pageHeaders{}, []byte("hello plain text"))
	if got := w.Result().Header.Get("Content-Type"); got == "" {
		t.Error("with an empty type, writePage must let ServeContent sniff one, not send a blank Content-Type")
	}
}

// Each generated route serves its own content type: in the document-type test the
// format is the only thing that changes, so the server must label each format.
func TestGeneratedRouteContentTypes(t *testing.T) {
	// A secret, so the secret-gated routes are reachable.
	srv, _, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")

	cases := []struct {
		path     string
		wantType string
	}{
		{"/ccdepthpage/none/tag/ct-a/4096", "text/html; charset=utf-8"},
		{"/depthhtmlpage/ct-a/4096", "text/html; charset=utf-8"},
		{"/depthjsonpage/ct-a/4096", "application/json"},
		{"/depthtxtpage/ct-a/4096", "text/plain; charset=utf-8"},
		{"/depthmdpage/ct-a/4096", "text/markdown; charset=utf-8"},
		{"/depthxmlpage/ct-a/4096", "application/xml; charset=utf-8"},
		{"/depthcsvpage/ct-a/4096", "text/csv; charset=utf-8"},
		{"/depthpdfpage/ct-a/4096", "application/pdf"},
		{"/depthdocxpage/ct-a/4096", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			resp := get(t, srv.URL+c.path, nil)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s: status %d, want 200", c.path, resp.StatusCode)
			}
			if got := resp.Header.Get("Content-Type"); got != c.wantType {
				t.Errorf("GET %s: Content-Type = %q, want %q", c.path, got, c.wantType)
			}
		})
	}
}

// The routing table must carry every generated route exactly once, with the right
// secret gate, and no prefix may be a prefix of another (or dispatch order would
// silently matter).
func TestGenPageRoutesCoverAndGate(t *testing.T) {
	wantSecret := map[string]bool{
		// The cache-header test serves the shared depth content (per-page codes).
		"/ccdepthpage/": true,
		// The depth page in each format, and the declared-type variant.
		"/depthtxtpage/":  true,
		"/depthpdfpage/":  true,
		"/depthdocxpage/": true,
		"/depthctpage/":   true,
		"/depthmdpage/":   true,
		"/depthhtmlpage/": true,
		"/depthjsonpage/": true,
		"/depthxmlpage/":  true,
		"/depthcsvpage/":  true,
		// JavaScript test: same depth lines, written in by a script.
		"/depthjsinlinepage/": true,
		"/depthjsextpage/":    true,
		"/depthjsdatapage/":   true,
		"/depthjsldpage/":     true,
		"/depthjsfile/":       true,
		// The changeable page: the depth HTML/text page with versioned codes.
		"/verhtmlpage/": true,
		"/vertxtpage/":  true,
		// The healable page: broken until the control fixes it, then the depth HTML page.
		"/healpage/": true,
	}
	got := map[string]bool{}
	for _, p := range genPageRoutes {
		if _, dup := got[p.prefix()]; dup {
			t.Errorf("prefix %q appears twice in genPageRoutes", p.prefix())
		}
		got[p.prefix()] = p.needsSecret()
	}
	if len(got) != len(wantSecret) {
		t.Fatalf("routes = %v, want %d entries", got, len(wantSecret))
	}
	for pfx, sec := range wantSecret {
		g, ok := got[pfx]
		if !ok {
			t.Errorf("route %s missing from genPageRoutes", pfx)
			continue
		}
		if g != sec {
			t.Errorf("%s needsSecret() = %v, want %v", pfx, g, sec)
		}
	}
	for _, a := range genPageRoutes {
		for _, b := range genPageRoutes {
			if a.prefix() != b.prefix() && strings.HasPrefix(a.prefix(), b.prefix()) {
				t.Errorf("%q is a prefix of %q — dispatch order would matter", b.prefix(), a.prefix())
			}
		}
	}
}

// A path that is not a generated page must fall through (return false), so
// ServeHTTP goes on to the static file server.
func TestServeGeneratedFallsThrough(t *testing.T) {
	s := newStaticServer(t.TempDir(), "")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/some/static/file.html", nil)
	if s.serveGenerated(w, r) {
		t.Error("serveGenerated must return false for a non-generated path")
	}
}

// The secret gate lives in the shared spine: every route needs a secret and
// answers 503 without one.
func TestServeGeneratedSecretGate(t *testing.T) {
	for _, path := range []string{"/depthhtmlpage/id/2048", "/depthjsonpage/x/2048", "/depthpdfpage/x/2048", "/ccdepthpage/none/tag/x/2048"} {
		s := newStaticServer(t.TempDir(), "") // no secret set
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if !s.serveGenerated(w, r) {
			t.Fatalf("%s: serveGenerated should have handled it", path)
		}
		if code := w.Result().StatusCode; code != http.StatusServiceUnavailable {
			t.Errorf("%s without a secret: status = %d, want 503", path, code)
		}
	}
}
