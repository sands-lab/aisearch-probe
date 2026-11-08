package main

import (
	"fmt"
	"net/http"

	"aisearch-probe/internal/probe"
)

// HTML pages: the HTML BASELINE of the object-type study. It is a SEPARATE route from
// /page/ on purpose: /page/ carries the shared static marker (the retention anchor);
// this needs the per-page UNFORGEABLE code so the HTML arm is symmetric with the
// other formats.
// So the format run compares like with like: every arm is /<fmt>page/<id>, each under
// a normal header, each carrying its own per-page code.
//
// Like the other format routes it needs the server secret (the code is an HMAC) and
// refuses with 503 without one. The body is a pure function of (id, code), so the
// ETag over the bytes is stable and revalidation works like /page/.

const htmlPagePrefix = "/htmlpage/"

type htmlPageRoute struct{}

func (htmlPageRoute) prefix() string    { return htmlPagePrefix }
func (htmlPageRoute) needsSecret() bool { return true }

// renderHTMLPage builds the HTML body for one id, printing the per-page code as the
// reference code. Pure function of (id, code).
func renderHTMLPage(id, code string) []byte {
	return []byte(fmt.Sprintf(`<!doctype html>
<meta charset="utf-8">
<title>probe document %s</title>
<h1>probe document</h1>
<p>status line: this document is live and serving normally.</p>
<p>page id: %s</p>
<p>reference code: %s</p>
`, id, id, code))
}

func (htmlPageRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	id, ok := parseGenID(w, r, htmlPagePrefix)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	code := probe.CpageCode(s.pageCodeSecret, id, probe.CpageFieldRef, probe.FormatPageRevision)
	return renderHTMLPage(id, code), "page.html", "text/html; charset=utf-8", pageHeaders{}, true
}
