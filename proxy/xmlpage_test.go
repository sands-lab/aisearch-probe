package main

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"
)

// wellFormedXML reports whether b parses cleanly as XML, by streaming every token to
// EOF. (xml.Unmarshal needs a concrete target, so a token scan is the way to check
// well-formedness alone.) Shared with the DOCX test.
func wellFormedXML(b []byte) error {
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// /xmlpage/ is the XML arm (feeds, sitemaps, data). Same facts, served as
// application/xml, body well-formed XML carrying the per-page code as an element
// value. Stamped, 304, realistic header, deterministic bytes.
func TestXMLPageIsStampedWellFormedAndCarriesMarker(t *testing.T) {
	srv, read, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")

	const id = "r0915-xml-a"
	resp := get(t, srv.URL+"/xmlpage/"+id, nil)
	got := body(t, resp)

	if resp.Header.Get("ETag") == "" {
		t.Error("XML page has no ETag — a crawler could not revalidate it")
	}
	if _, present := resp.Header["Cache-Control"]; present {
		t.Errorf("XML page sent Cache-Control %q, want it omitted (realistic header)", resp.Header.Get("Cache-Control"))
	}
	// Well-formedness: the whole body must parse as XML, or a fetcher that parses
	// strictly would reject it and the format would look broken rather than uncached.
	if err := wellFormedXML([]byte(got)); err != nil {
		t.Errorf("XML page body is not well-formed XML: %v\n%s", err, got)
	}
	if code := fmtRefCode(id); !strings.Contains(got, code) {
		t.Errorf("XML page is missing its per-page code %q:\n%s", code, got)
	}
	if !strings.Contains(got, id) {
		t.Errorf("XML page is missing its id:\n%s", got)
	}
	recs := read()
	if len(recs) != 1 || !recs[0].Stamped() {
		t.Fatalf("the log must show the XML page as stamped: %+v", recs)
	}
}

func TestXMLPageAnswers304(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")
	first := get(t, srv.URL+"/xmlpage/r0915-xml-b", nil)
	first.Body.Close()
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on first fetch")
	}
	second := get(t, srv.URL+"/xmlpage/r0915-xml-b", map[string]string{"If-None-Match": etag})
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation returned %d, want 304", second.StatusCode)
	}
}

func TestXMLPageBareIs404(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")
	resp := get(t, srv.URL+"/xmlpage/", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/xmlpage/ with no id returned %d, want 404", resp.StatusCode)
	}
}

func TestXMLPageDeterministic(t *testing.T) {
	if string(renderXMLPage("same-id", "code123")) != string(renderXMLPage("same-id", "code123")) {
		t.Error("renderXMLPage is not deterministic")
	}
}
