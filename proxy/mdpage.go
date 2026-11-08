package main

import (
	"fmt"
	"net/http"

	"aisearch-probe/internal/probe"
)

// Markdown pages: the Markdown arm of the object-type study — the format so much
// AI-facing content is now served in. Same facts as /page/, served as text/markdown
// with a heading so it reads as Markdown. Body is a pure function of the id (stable
// ETag); realistic header (Cache-Control omitted, NOT no-store).

const mdPagePrefix = "/mdpage/"

type mdPageRoute struct{}

func (mdPageRoute) prefix() string    { return mdPagePrefix }
func (mdPageRoute) needsSecret() bool { return true }

// renderMdPage builds the Markdown body for one id. Pure function of (id, code).
func renderMdPage(id, code string) []byte {
	return []byte(fmt.Sprintf(`# probe document

status line: this document is live and serving normally.

- page id: %s
- reference code: %s
`, id, code))
}

func (mdPageRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	id, ok := parseGenID(w, r, mdPagePrefix)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	code := probe.CpageCode(s.pageCodeSecret, id, probe.CpageFieldRef, probe.FormatPageRevision)
	return renderMdPage(id, code), "page.md", "text/markdown; charset=utf-8", pageHeaders{}, true
}
