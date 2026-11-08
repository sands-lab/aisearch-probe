package main

// Generated pages share one spine. Every route invents a page in memory from its
// address, stamps it, and serves it so a crawler caches and revalidates it exactly
// as a real page. The head of each (parse the path, gate on the secret, cap a size,
// look an id up) genuinely differs — those differences ARE the experiments — but
// the tail is identical: fingerprint the bytes into an ETag, set the type, set the
// caching headers, and ServeContent so a conditional request still gets a 304.
//
// genPage is that shared shape, the page-side twin of the Vendor interface on the
// trial side: serveGenerated runs the spine for every route, and each route fills
// in only what is special to it. The tail lives in ONE place (writePage), so no
// route can drift on the ETag or silently ship the wrong cache header.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aisearch-probe/internal/probe"
)

// pageHeaders is the caching headers a route asks for. The ZERO VALUE is the
// normal page every experiment serves: no Cache-Control, no Expires, an ETag, and
// Last-Modified = probe.PageLastModified. Only the cache-header test
// (/ccdepthpage/) may return anything else — a test setting must never leak into
// another experiment's pages (DECISIONS.md § D13), and
// TestOnlyTheCacheTestRouteChoosesCacheHeaders enforces it.
type pageHeaders struct {
	CacheControl string    // "" omits the header
	Expires      string    // "" omits the header
	NoValidators bool      // send neither ETag nor Last-Modified
	LastModified time.Time // zero means probe.PageLastModified
}

// genPage is one generated-page route.
type genPage interface {
	// prefix is the path prefix this route answers (e.g. "/page/").
	prefix() string
	// needsSecret reports whether the route must refuse (503) when no page-code
	// secret is configured — true for the routes whose markers must be unforgeable.
	needsSecret() bool
	// build validates the request and produces the page: the bytes, the name for
	// ServeContent, the Content-Type to send ("" lets ServeContent sniff one from
	// the name/bytes), and the caching headers (pageHeaders{} for every route but
	// the cache-header test). On a bad request it writes the 404/413/400 itself and
	// returns ok=false, so the spine does nothing further.
	build(s *staticServer, w http.ResponseWriter, r *http.Request) (content []byte, name, contentType string, hdr pageHeaders, ok bool)
}

// genPageRoutes is the routing table serveGenerated walks. No two prefixes are a
// prefix of another, so match order does not matter.
var genPageRoutes = []genPage{
	// The cache-header test: the ONLY route that chooses caching headers.
	ccDepthRoute{},
	// The depth page in each document format, plus the declared-type variant.
	depthTxtRoute{},
	depthPdfRoute{},
	depthDocxRoute{},
	depthCtRoute{}, // same bytes, content-type chosen by path token (label lever)
	depthMdRoute{},
	depthHTMLRoute{},
	depthJSONRoute{},
	depthXMLRoute{},
	depthCSVRoute{},
	// JavaScript study: the depth lines put on the page by a script (depthjspage.go).
	depthJSInlineRoute{},
	depthJSExtRoute{},
	depthJSDataRoute{},
	depthJSLDRoute{},   // the deeper markers in a JSON-LD block, never rendered
	depthJSFileRoute{}, // the external variant's script file
	// The changeable page: the depth HTML/text page whose codes follow a version
	// the control sets, at one fixed address (verpage.go).
	versionRoutes["html"],
	versionRoutes["txt"],
	// The healable page: broken (503/404/403) until the control fixes it, then the
	// depth HTML page (healpage.go).
	healPageRoute{},
}

// serveGenerated dispatches a request to its generated-page route and runs the
// shared spine: the secret gate, then build, then the common tail. It returns
// false when the path is not a generated page, so ServeHTTP falls through.
func (s *staticServer) serveGenerated(w http.ResponseWriter, r *http.Request) bool {
	for _, p := range genPageRoutes {
		if !strings.HasPrefix(r.URL.Path, p.prefix()) {
			continue
		}
		// No secret means a route whose markers must be unforgeable cannot make
		// them, so it refuses rather than serve a guessable page.
		if p.needsSecret() && s.pageCodeSecret == "" {
			http.Error(w, "this page type is not configured", http.StatusServiceUnavailable)
			return true
		}
		if content, name, contentType, hdr, ok := p.build(s, w, r); ok {
			writePage(w, r, name, contentType, hdr, content)
		}
		return true
	}
	return false
}

// writePage is the tail every generated page shares: a strong ETag over the exact
// bytes (so a crawler caches and revalidates it like a real page), the per-route
// Content-Type (so the object-type study can serve JSON/PDF/DOCX/… each under its
// own type; "" lets ServeContent sniff one from the name/bytes instead), the
// caching headers in h (zero value: none, see pageHeaders), then ServeContent,
// which turns a matching If-None-Match / If-Modified-Since into a 304 with no body.
func writePage(w http.ResponseWriter, r *http.Request, name, contentType string, h pageHeaders, content []byte) {
	// A zero modtime makes ServeContent send no Last-Modified and ignore
	// If-Modified-Since; with no ETag either, nothing can answer 304.
	modTime := time.Time{}
	if !h.NoValidators {
		w.Header().Set("ETag", pageETag(content))
		modTime = probe.PageLastModified
		if !h.LastModified.IsZero() {
			modTime = h.LastModified
		}
	}
	// An explicit type is set before ServeContent (which only sniffs when none is
	// set); "" deliberately leaves it for ServeContent to sniff from the name/bytes.
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if h.CacheControl != "" {
		w.Header().Set("Cache-Control", h.CacheControl)
	}
	if h.Expires != "" {
		w.Header().Set("Expires", h.Expires)
	}
	http.ServeContent(w, r, name, modTime, bytes.NewReader(content))
}

// pageETag is the strong ETag every generated page sends: the first 8 bytes of the
// body's SHA-256. It is one function so the changeable page's control can report
// the exact tag the page will serve (verpage.go) without drifting from writePage.
func pageETag(content []byte) string {
	sum := sha256.Sum256(content)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// parseGenID extracts the single "<id>" segment after prefix, for the format routes
// that key on one id with no sub-path (/jsonpage/, /pdfpage/, …). An empty id (a
// bare prefix) or one containing a slash is a 404, so a typo cannot read as a page.
func parseGenID(w http.ResponseWriter, r *http.Request, prefix string) (string, bool) {
	id := strings.TrimPrefix(r.URL.Path, prefix)
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return "", false
	}
	return id, true
}

// parseSizePath splits "<id>/<bytes>" after prefix and enforces the upper cap. On
// a malformed address it writes a 404, on an over-cap size a 413, and returns
// ok=false in either case. The per-route lower bound (does the size fit the page's
// own header/markers?) stays in the route, because it differs per route and one of
// them needs the secret to compute it.
func parseSizePath(w http.ResponseWriter, r *http.Request, prefix string, maxBytes int) (id string, nbytes int, ok bool) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return "", 0, false
	}
	nbytes, err := strconv.Atoi(parts[1])
	if err != nil || nbytes <= 0 {
		http.NotFound(w, r)
		return "", 0, false
	}
	if nbytes > maxBytes {
		http.Error(w, fmt.Sprintf("page too large: %d bytes exceeds the %d-byte cap", nbytes, maxBytes), http.StatusRequestEntityTooLarge)
		return "", 0, false
	}
	return parts[0], nbytes, true
}

// pageModTime is the fixed date generated binary formats (docx/pdf zip entries)
// stamp inside their bytes, so those bytes stay a pure function of the address.
// It IS probe.PageLastModified, the Last-Modified writePage sends, defined once.
var pageModTime = probe.PageLastModified
