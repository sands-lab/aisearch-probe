package main

import (
	"fmt"
	"net/http"

	"aisearch-probe/internal/probe"
)

// Plain-text pages: the "pure text, no structure" control for the object-type study.
// Same facts as /page/, served as text/plain. Body is a pure function of the id
// (stable ETag); realistic header (Cache-Control omitted, NOT no-store).

const txtPagePrefix = "/txtpage/"

type txtPageRoute struct{}

func (txtPageRoute) prefix() string    { return txtPagePrefix }
func (txtPageRoute) needsSecret() bool { return true }

// renderTxtPage builds the plain-text body for one id, the per-page code on its own
// line. Pure function of (id, code).
func renderTxtPage(id, code string) []byte {
	return []byte(fmt.Sprintf(`probe document
status line: this document is live and serving normally.
page id: %s
reference code: %s
`, id, code))
}

func (txtPageRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	id, ok := parseGenID(w, r, txtPagePrefix)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	code := probe.CpageCode(s.pageCodeSecret, id, probe.CpageFieldRef, probe.FormatPageRevision)
	return renderTxtPage(id, code), "page.txt", "text/plain; charset=utf-8", pageHeaders{}, true
}
