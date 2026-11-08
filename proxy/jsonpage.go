package main

import (
	"encoding/json"
	"net/http"

	"aisearch-probe/internal/probe"
)

// JSON pages: the same page facts as /page/, served as a real JSON document. The
// object-type study asks whether a fetcher fetches, reads, and caches JSON the way
// it does HTML — so FORMAT is the only thing that differs from the other format
// routes. Like the other new routes it sends a REALISTIC header (Cache-Control
// omitted, NOT no-store — caching headers are reserved for the /ccdepthpage/ test).
//
// The body is a pure function of the id, so the ETag over the bytes is stable across
// fetches and revalidation works exactly like /page/. The marker rides as a FIELD
// value so a model that parses the JSON quotes it from a real field, not from stray
// text.

const jsonPagePrefix = "/jsonpage/"

// jsonPageRoute is the /jsonpage/ route. See genpage.go.
type jsonPageRoute struct{}

func (jsonPageRoute) prefix() string    { return jsonPagePrefix }
func (jsonPageRoute) needsSecret() bool { return true }

// renderJSONPage builds the JSON document for one id. A struct marshals to a stable
// field order, so the bytes are a pure function of (id, code) (stable ETag). The
// reference_code field carries the per-page UNFORGEABLE code; page_id and record
// carry the id. Pure function of (id, code).
func renderJSONPage(id, code string) []byte {
	doc := struct {
		Status        string `json:"status"`
		PageID        string `json:"page_id"`
		ReferenceCode string `json:"reference_code"`
		Record        string `json:"record"`
	}{
		Status:        "live",
		PageID:        id,
		ReferenceCode: code,
		Record:        id,
	}
	b, err := json.Marshal(doc)
	if err != nil {
		// The struct has only strings, so Marshal cannot fail; be defensive anyway.
		return []byte(`{"status":"live","reference_code":"` + code + `"}`)
	}
	return b
}

// build answers a request for a JSON page. A bare /jsonpage/ or an id with a slash
// is a 404. The spine has already 503'd if no secret is set, so the code is non-empty.
// The realistic header means no Cache-Control at all ("").
func (jsonPageRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	id, ok := parseGenID(w, r, jsonPagePrefix)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	code := probe.CpageCode(s.pageCodeSecret, id, probe.CpageFieldRef, probe.FormatPageRevision)
	return renderJSONPage(id, code), "page.json", "application/json", pageHeaders{}, true
}
