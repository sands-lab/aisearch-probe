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

// Each generated route serves its own content type — the whole point of the
// object-type study is that FORMAT is the only variable, which means the server
// must actually label each format. The 5 original routes stay text/html (behaviour
// unchanged); the 7 new routes each carry their format's type.
func TestGeneratedRouteContentTypes(t *testing.T) {
	// A secret so the secret-gated routes (/cpage/, /depthpage/, and all 8 format
	// routes) are reachable; /page/ does not need it.
	srv, _, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")

	cases := []struct {
		path     string
		wantType string
	}{
		{"/page/ct-a", "text/html; charset=utf-8"},
		{"/ccdepthpage/none/tag/ct-a/4096", "text/html; charset=utf-8"},
		{"/htmlpage/ct-a", "text/html; charset=utf-8"},
		{"/jsonpage/ct-a", "application/json"},
		{"/txtpage/ct-a", "text/plain; charset=utf-8"},
		{"/mdpage/ct-a", "text/markdown; charset=utf-8"},
		{"/xmlpage/ct-a", "application/xml; charset=utf-8"},
		{"/csvpage/ct-a", "text/csv; charset=utf-8"},
		{"/pdfpage/ct-a", "application/pdf"},
		{"/docxpage/ct-a", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
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
		"/page/":      false,
		"/sizepage/":  false,
		"/depthpage/": true,
		"/cpage/":     true,
		// The cache-header test serves the shared depth content (per-page codes).
		"/ccdepthpage/": true,
		// Object-type study: 8 format routes, ALL need the secret — each prints a
		// per-page unforgeable code, so "read" proves the model parsed THAT body.
		"/htmlpage/": true,
		"/jsonpage/": true,
		"/txtpage/":  true,
		"/mdpage/":   true,
		"/xmlpage/":  true,
		"/csvpage/":  true,
		"/pdfpage/":  true,
		"/docxpage/": true,
		// Object-size study: format depth routes (scaled, HMAC markers) need the secret.
		"/depthtxtpage/":    true,
		"/depthpdfpage/":    true,
		"/depthpdfrawpage/": true, // fault injection: invalid-UTF-8 PDF (investigation)
		"/depthdocxpage/":   true,
		"/depthctpage/":     true,
		"/depthmdpage/":     true,
		"/depthhtmlpage/":   true,
		"/depthjsonpage/":   true,
		"/depthxmlpage/":    true,
		"/depthcsvpage/":    true,
		// JavaScript study: same depth lines, written in by a script.
		"/depthjsinlinepage/": true,
		"/depthjsextpage/":    true,
		"/depthjsdatapage/":   true,
		"/depthjsldpage/":     true,
		"/depthjsfile/":       true,
		"/depthjsverpage/":    true,
		"/depthjsverfile/":    true,
		// The changeable page: the depth HTML/text page with versioned codes.
		"/verhtmlpage/": true,
		"/vertxtpage/":  true,
		// The healable page: broken until the control fixes it, then the depth HTML page.
		"/healpage/": true,
		// The personal page: depth HTML whose codes follow one identity query item.
		"/idpage/": true,
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

// The secret gate lives in the shared spine: a route that needs a secret 503s
// without one, and a route that does not must NOT 503.
func TestServeGeneratedSecretGate(t *testing.T) {
	for _, path := range []string{"/cpage/0", "/depthpage/id/2048", "/jsonpage/x", "/pdfpage/x"} {
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

	// /page/ needs no secret, so it must not 503 when none is set.
	s := newStaticServer(t.TempDir(), "")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/page/abc", nil)
	if !s.serveGenerated(w, r) {
		t.Fatal("/page/ should have been handled")
	}
	if code := w.Result().StatusCode; code == http.StatusServiceUnavailable {
		t.Errorf("/page/ must not 503 when no secret is set (status = %d)", code)
	}
}
