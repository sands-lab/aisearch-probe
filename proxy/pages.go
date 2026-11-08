package main

import (
	"fmt"
	"net/http"
	"strings"

	"aisearch-probe/internal/probe"
)

// MADE-UP PAGES, generated on demand at a per-run address.
//
// The robots.txt experiments need a page that has NEVER been fetched before, so
// that "no request reached us" can only mean "the crawler was blocked", never
// "the crawler answered from an old copy it already had". A file on disk cannot
// give that: reuse its address across runs and a stale cached copy poisons the
// result. So the origin instead invents a page for any address under /page/,
// building the HTML in memory from the address itself. Each run picks a fresh
// address (a run nonce in the path), so the page is guaranteed new, and nothing
// is ever written to or cleaned off disk.
//
// The page is a REAL cacheable page: it carries an ETag over its bytes and a
// Last-Modified, and answers "has this changed?" with a 304, exactly like the
// static server — so a provider caches it the same way it caches a normal page.
// That is what lets the same route serve the later invalidation test too (where
// the page must first be fetched and stored).
//
// The bytes are a pure function of the address, so the ETag is stable across
// fetches of the same address and revalidation works. Two facts live on the
// page — a status line at the top and the study reference code at the bottom —
// so a warm and a re-ask can ask about different facts (the design that keeps a
// "no re-fetch" from being a replayed answer; see trial/negrun.go).

const pagePrefix = "/page/"

// pageModTime is the fixed date generated binary formats (docx/pdf zip entries)
// stamp inside their bytes, so those bytes stay a pure function of the address.
// It IS probe.PageLastModified — the Last-Modified writePage sends — defined once.
var pageModTime = probe.PageLastModified

// pageRoute is the /page/ generated-page route. See genpage.go.
type pageRoute struct{}

func (pageRoute) prefix() string    { return pagePrefix }
func (pageRoute) needsSecret() bool { return false }

// renderPage builds the HTML for one generated page from its id.
func renderPage(id string) string {
	return fmt.Sprintf(`<!doctype html>
<meta charset="utf-8">
<title>probe page %s</title>
<h1>probe page</h1>
<p>status line: this page is live and serving normally.</p>
<p>page id: %s</p>
<p>study reference RCP-2026-08</p>
`, id, id)
}

// build answers a request for a generated page. An empty id (a bare /page/) or an
// id with a slash is a 404, so a typo cannot read as a real page.
//
// /page/ sends the NORMAL headers (pageHeaders{}: no Cache-Control), deliberately
// NOT the server's -cache-control flag, so a careless origin restart cannot change
// what these pages send mid-run. It sent no-store until 2026-10-07; that was
// dropped so no experiment runs under a cache-test setting (DECISIONS.md § D13).
// The CDN edge is kept out by the Cloudflare bypass rule (FINDINGS Result 5b).
func (pageRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	id := strings.TrimPrefix(r.URL.Path, pagePrefix)
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return nil, "", "", pageHeaders{}, false
	}
	return []byte(renderPage(id)), "page.html", "text/html; charset=utf-8", pageHeaders{}, true
}
