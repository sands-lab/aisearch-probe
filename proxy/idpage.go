package main

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"aisearch-probe/internal/probe"
)

// THE PERSONAL PAGE — /idpage/<id>/<name>/<bytes>?<name>=<value> — for the
// private-link test (doc/handoff-url-equivalence-and-leak.md, extra 1).
//
// It is the 2 KB depth HTML page (/depthhtmlpage/), except that its five codes are
// made from the page id PLUS the value of one named query item
// (probe.IDPageCodeID). So the copy made for ?mc_eid=user-1 carries codes only
// user-1's copy has. Caller A fetches that; caller B — another account — then asks
// for the same page WITHOUT A's value (the bare page, or a different value). If B's
// answer quotes user-1's codes with no download, the provider handed B A's
// personal copy: the leak.
//
// THE QUERY CHANGES THE PAGE HERE, ON PURPOSE. Every other generated page keeps
// everything that matters in the path, so a provider that strips the query cannot
// change it. This page is the one exception: whether a provider keeps or drops the
// identity item IS the measurement. Only the named item counts; every other query
// item (a probe token, a tracking tag, the order) is ignored, so it cannot be the
// cause of a different answer. The item's NAME sits in the path, so one route
// serves every name under test (utm_content, mc_eid, _hsenc, fbclid, uid).
//
// No value (missing, or present but empty) is the anonymous page: codes nobody
// owns. Normal headers (D13): no Cache-Control, no Expires, ETag + Last-Modified.

const idPagePrefix = "/idpage/"

// idPageName is what a name in the path may be: the characters real tracking and
// identity items use. Anything else is a 404, so a typo cannot read as a page.
var idPageName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

type idPageRoute struct{}

func (idPageRoute) prefix() string    { return idPagePrefix }
func (idPageRoute) needsSecret() bool { return true }

func (idPageRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, idPagePrefix), "/")
	if len(parts) != 3 || parts[0] == "" || !idPageName.MatchString(parts[1]) {
		http.NotFound(w, r)
		return nil, "", "", pageHeaders{}, false
	}
	id, name := parts[0], parts[1]
	nbytes, err := strconv.Atoi(parts[2])
	if err != nil || nbytes <= 0 {
		http.NotFound(w, r)
		return nil, "", "", pageHeaders{}, false
	}
	if nbytes > maxDepthPageBytes {
		http.Error(w, fmt.Sprintf("page too large: %d bytes exceeds the %d-byte cap", nbytes, maxDepthPageBytes), http.StatusRequestEntityTooLarge)
		return nil, "", "", pageHeaders{}, false
	}
	if nbytes < minDepthFormatBytes {
		http.Error(w, fmt.Sprintf("page too small: %d bytes cannot hold the markers", nbytes), http.StatusBadRequest)
		return nil, "", "", pageHeaders{}, false
	}
	codeID := probe.IDPageCodeID(id, name, r.URL.Query().Get(name))
	page := renderDepthHTML(depthFormatLinesRev(s.pageCodeSecret, codeID, nbytes, true, probe.DepthPageRevision))
	return page, "depth.html", "text/html; charset=utf-8", pageHeaders{}, true
}
