package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"net/http"

	"aisearch-probe/internal/probe"
)

// DOCX pages: the second BINARY arm. A .docx is a ZIP archive of XML parts, so it is
// buildable in pure Go with archive/zip + encoding/xml (no deps). The marker goes in
// the document text so a real extractor (Word, pandoc, unzip) can read it out — the
// same fairness concern as the PDF: a malformed DOCX makes a vendor that CAN read
// Word docs look like it cannot.
//
// DETERMINISM (critical). A zip embeds a modified time per entry; left at the default
// it is time.Now(), so the bytes — and the ETag — would drift every build and the
// caching read would be meaningless. So every entry is written with a FIXED modified
// time (pageModTime) and stored uncompressed (zip.Store), making the bytes a pure
// function of the id. Realistic header (Cache-Control omitted, NOT no-store).

const docxPagePrefix = "/docxpage/"

type docxPageRoute struct{}

func (docxPageRoute) prefix() string    { return docxPagePrefix }
func (docxPageRoute) needsSecret() bool { return true }

// The two fixed package parts. Only word/document.xml varies with the id.
const docxContentTypesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`

const docxRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`

// docxDocumentXML builds word/document.xml: one paragraph of text carrying the
// per-page code and the id, XML-escaped so any id stays well-formed.
func docxDocumentXML(id, code string) string {
	var esc bytes.Buffer
	_ = xml.EscapeText(&esc, []byte("reference code: "+code+" page id: "+id))
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>` + esc.String() + `</w:t></w:r></w:p></w:body></w:document>`
}

// renderDocxPage assembles the .docx as a deterministic zip of its three parts. Pure
// function of (id, code).
func renderDocxPage(id, code string) []byte {
	parts := []struct{ name, body string }{
		{"[Content_Types].xml", docxContentTypesXML},
		{"_rels/.rels", docxRelsXML},
		{"word/document.xml", docxDocumentXML(id, code)},
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range parts {
		hdr := &zip.FileHeader{Name: p.name, Method: zip.Store, Modified: pageModTime}
		wtr, err := zw.CreateHeader(hdr)
		if err != nil {
			continue
		}
		_, _ = wtr.Write([]byte(p.body))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func (docxPageRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	id, ok := parseGenID(w, r, docxPagePrefix)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	code := probe.CpageCode(s.pageCodeSecret, id, probe.CpageFieldRef, probe.FormatPageRevision)
	return renderDocxPage(id, code), "page.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", pageHeaders{}, true
}
