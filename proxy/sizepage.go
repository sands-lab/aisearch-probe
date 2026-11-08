package main

import (
	"fmt"
	"net/http"
	"strings"
)

// Sized pages: like /page/, but the body is padded to a byte length asked for in
// the ADDRESS, so the object-size sweep can serve one page per rung of a size
// ladder and see whether a fetcher caches large pages the same as small ones.
// See doc/object-size-sweep-design.md.
//
// A SEPARATE route from /page/ on purpose: the retention run depends on /page/
// bytes not moving, so this route never touches them.
//
// Two invariants carry the experiment:
//
//   - The size is in the PATH (/sizepage/<id>/<bytes>), not a query, per the
//     standing gotcha (TODO.md § Instrument): a provider that strips the query
//     cannot silently change the size out from under us.
//   - The marker sits in the FIRST few hundred bytes, before the padding. A
//     fetcher may truncate long content by token count and cache the stub; if the
//     marker sat at the bottom, a truncated-but-cached big page would read as
//     "never had the content" — a false size cliff. Padding comes after the
//     marker so a missing marker means genuinely not held. A test enforces this.
//
// The bytes are a pure function of the address (id + size), so the ETag over the
// body is stable across fetches and revalidation works — exactly like /page/.

const sizePagePrefix = "/sizepage/"

// maxSizePageBytes caps what the origin will build on request, so a stray or
// hostile address cannot make it allocate an enormous body. The ladder tops out
// at a few MiB; 32 MiB is comfortable headroom and still bounded.
const maxSizePageBytes = 32 << 20

// sizePagePadOpen/Close wrap the deterministic filler in an HTML comment, so the
// padding does not render as visible junk. The filler never contains "--", so it
// cannot close the comment early.
const sizePagePadOpen = "<!-- pad "
const sizePagePadClose = " -->\n"

// sizePageRoute is the /sizepage/ route. See genpage.go.
type sizePageRoute struct{}

func (sizePageRoute) prefix() string    { return sizePagePrefix }
func (sizePageRoute) needsSecret() bool { return false }

// sizePageHead is the top of the page: the marker and identity, before any
// padding. Kept compact so it fits inside the smallest rung (1 KiB) and well
// inside any truncation window.
func sizePageHead(id string, nbytes int) string {
	return fmt.Sprintf(`<!doctype html>
<meta charset="utf-8">
<title>sized page %s</title>
<h1>sized page</h1>
<p>status line: this page is live and serving normally.</p>
<p>page id: %s</p>
<p>size bytes: %d</p>
<p>study reference RCP-2026-08</p>
`, id, id, nbytes)
}

// sizePageFits reports whether a page of nbytes can hold the head plus the
// padding wrapper. Smaller than this cannot be built byte-exactly.
func sizePageFits(id string, nbytes int) bool {
	overhead := len(sizePageHead(id, nbytes)) + len(sizePagePadOpen) + len(sizePagePadClose)
	return nbytes >= overhead
}

// renderSizePage builds a page of EXACTLY nbytes: the head (with the marker at
// the top), then deterministic filler inside an HTML comment to reach the length.
// It assumes sizePageFits(id, nbytes); callers check that first.
func renderSizePage(id string, nbytes int) string {
	head := sizePageHead(id, nbytes)
	padLen := nbytes - len(head) - len(sizePagePadOpen) - len(sizePagePadClose)
	var b strings.Builder
	b.Grow(nbytes)
	b.WriteString(head)
	b.WriteString(sizePagePadOpen)
	// Deterministic filler: a single repeated byte. It is a pure function of the
	// length, and the id already differs in the head, so two addresses never share
	// bytes. 'x' never forms "--", so the HTML comment cannot close early.
	for i := 0; i < padLen; i++ {
		b.WriteByte('x')
	}
	b.WriteString(sizePagePadClose)
	return b.String()
}

// build answers a request for a sized page. A malformed address is a 404, an
// over-cap size a 413 (both from parseSizePath), and a size too small to hold the
// header a 400. Normal headers (pageHeaders{}), like every route but /ccdepthpage/.
func (sizePageRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	id, nbytes, ok := parseSizePath(w, r, sizePagePrefix, maxSizePageBytes)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	if !sizePageFits(id, nbytes) {
		http.Error(w, fmt.Sprintf("sized page too small: %d bytes cannot hold the page header", nbytes), http.StatusBadRequest)
		return nil, "", "", pageHeaders{}, false
	}
	return []byte(renderSizePage(id, nbytes)), "sizepage.html", "text/html; charset=utf-8", pageHeaders{}, true
}
