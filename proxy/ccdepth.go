package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"aisearch-probe/internal/probe"
)

// /ccdepthpage/<setting>/<validators>/<id>/<bytes> — THE CACHE-HEADER TEST.
//
// Does a vendor's fetch cache obey the caching headers a site sends? This route
// serves the SHARED depth content — byte-for-byte the /depthhtmlpage/ body for the
// same id and size, the one content definition every experiment uses — under a
// caching-header setting chosen by two words in the address:
//
//   - <setting> picks Cache-Control / Expires from probe.CacheSettings
//     (no-store, private, no-cache, max-age=0, the real-world combo, a past Expires,
//     max-age=60, max-age=60 + must-revalidate, s-maxage=60 under max-age=3600,
//     max-age=3600, or nothing at all);
//   - <validators> picks the version info from probe.ValidatorModes: "tag" (ETag +
//     the fixed old Last-Modified, the normal page), "recent" (ETag + Last-Modified =
//     when this origin started, so the spec's tenth-of-the-age guess shrinks to
//     minutes), or "notag" (neither, so "check with the site" can only be a full
//     re-download).
//
// THIS IS THE ONLY ROUTE ALLOWED TO CHOOSE CACHING HEADERS (DECISIONS.md § D13);
// every other route returns pageHeaders{} and a test enforces it. The grading rule
// (what RFC 9111 allows a shared cache for each setting) is probe.SharedReuseLimit.

const ccDepthPrefix = "/ccdepthpage/"

type ccDepthRoute struct{}

func (ccDepthRoute) prefix() string    { return ccDepthPrefix }
func (ccDepthRoute) needsSecret() bool { return true }

func (ccDepthRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, ccDepthPrefix), "/")
	if len(parts) != 4 {
		http.NotFound(w, r)
		return nil, "", "", pageHeaders{}, false
	}
	setTok, valTok, id, sizeTok := parts[0], parts[1], parts[2], parts[3]
	set, known := probe.CacheSettings[setTok]
	if !known || id == "" {
		http.NotFound(w, r)
		return nil, "", "", pageHeaders{}, false
	}
	hdr := pageHeaders{CacheControl: set.CacheControl, Expires: set.Expires}
	switch probe.ValidatorMode(valTok) {
	case probe.ValidatorsOld:
		// the normal version info: ETag + probe.PageLastModified
	case probe.ValidatorsRecent:
		hdr.LastModified = s.startedAt
	case probe.ValidatorsNone:
		hdr.NoValidators = true
	default:
		http.NotFound(w, r)
		return nil, "", "", pageHeaders{}, false
	}
	nbytes, err := strconv.Atoi(sizeTok)
	if err != nil || nbytes <= 0 {
		http.NotFound(w, r)
		return nil, "", "", pageHeaders{}, false
	}
	if nbytes > maxDepthPageBytes {
		http.Error(w, fmt.Sprintf("page too large: %d bytes exceeds the %d-byte cap", nbytes, maxDepthPageBytes), http.StatusRequestEntityTooLarge)
		return nil, "", "", pageHeaders{}, false
	}
	if nbytes < minDepthFormatBytes {
		http.Error(w, fmt.Sprintf("depth page too small: %d bytes cannot hold the markers", nbytes), http.StatusBadRequest)
		return nil, "", "", pageHeaders{}, false
	}
	// The same bytes /depthhtmlpage/<id>/<bytes> serves: only the headers differ.
	body := renderDepthHTML(depthFormatLines(s.pageCodeSecret, id, nbytes))
	return body, "depth.html", "text/html; charset=utf-8", hdr, true
}
