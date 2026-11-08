package main

import (
	"encoding/xml"
	"net/http"

	"aisearch-probe/internal/probe"
)

// XML pages: the XML arm of the object-type study (the shape of feeds, sitemaps and
// data dumps). Same facts as /page/, served as application/xml, the marker riding as
// an element value. Body is a pure function of the id (stable ETag); realistic header
// (Cache-Control omitted, NOT no-store).

const xmlPagePrefix = "/xmlpage/"

type xmlPageRoute struct{}

func (xmlPageRoute) prefix() string    { return xmlPagePrefix }
func (xmlPageRoute) needsSecret() bool { return true }

// xmlRecord is the document shape; a struct marshals to a stable element order, so
// the bytes are a pure function of the id.
type xmlRecord struct {
	XMLName       xml.Name `xml:"record"`
	Status        string   `xml:"status"`
	PageID        string   `xml:"page_id"`
	ReferenceCode string   `xml:"reference_code"`
}

// renderXMLPage builds the well-formed XML document for one id. Pure function of
// (id, code); the per-page code rides as the reference_code element.
func renderXMLPage(id, code string) []byte {
	body, err := xml.Marshal(xmlRecord{
		Status:        "live",
		PageID:        id,
		ReferenceCode: code,
	})
	if err != nil {
		// Only strings are marshalled, so this cannot fail; be defensive anyway.
		body = []byte("<record><reference_code>" + code + "</reference_code></record>")
	}
	return append([]byte(xml.Header), body...)
}

func (xmlPageRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	id, ok := parseGenID(w, r, xmlPagePrefix)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	code := probe.CpageCode(s.pageCodeSecret, id, probe.CpageFieldRef, probe.FormatPageRevision)
	return renderXMLPage(id, code), "page.xml", "application/xml; charset=utf-8", pageHeaders{}, true
}
