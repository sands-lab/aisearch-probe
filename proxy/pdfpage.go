package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"aisearch-probe/internal/probe"
)

// PDF pages: the first BINARY arm of the object-type study, and the real question.
// A PDF is a binary container the fetcher must run through a text extractor, so a
// "fetched but marker missing" result means the fetcher got the bytes but did not
// read the text out. For that to be a fair test the PDF must be genuinely
// well-formed and the marker must be SELECTABLE TEXT (a PDF text object), not an
// image — otherwise a vendor that CAN read PDFs looks like it cannot. We hand-roll a
// minimal one-page PDF in pure Go (no deps): catalog -> pages -> page -> a content
// stream with a BT/Tj/ET text block in base-14 Helvetica, plus a correct xref table
// and trailer.
//
// The bytes are a pure function of the id (no embedded timestamps), so the ETag over
// them is stable and revalidation works like /page/. Realistic header (Cache-Control
// omitted, NOT no-store).

const pdfPagePrefix = "/pdfpage/"

type pdfPageRoute struct{}

func (pdfPageRoute) prefix() string    { return pdfPagePrefix }
func (pdfPageRoute) needsSecret() bool { return true }

// pdfEscapeString escapes the three characters that are special inside a PDF literal
// string: backslash and the two parentheses. The ids we use are alphanumeric, but
// escaping keeps the generator correct for any id.
func pdfEscapeString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`)
	return r.Replace(s)
}

// renderPDFPage builds a minimal valid one-page PDF carrying the per-page code and id
// as selectable text. Pure function of (id, code). Object byte offsets are tracked as
// the file is assembled so the xref table is correct and startxref points exactly at it.
func renderPDFPage(id, code string) []byte {
	// The one line of selectable text: the per-page code, then the id.
	text := "reference code: " + pdfEscapeString(code) + " page id: " + pdfEscapeString(id)
	content := "BT /F1 12 Tf 72 720 Td (" + text + ") Tj ET\n"

	// Object bodies, in order 1..5. Object 5 is the content stream.
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		"<< /Length " + strconv.Itoa(len(content)) + " >>\nstream\n" + content + "endstream",
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")

	// offsets[i] is the byte offset of object i (1-indexed); offsets[0] is the free
	// head entry and stays 0.
	offsets := make([]int, len(objs)+1)
	for i, body := range objs {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}

	// The xref table: startxref must name this offset, and the per-object entries
	// must be the exact 20-byte records the format requires.
	xrefOffset := buf.Len()
	n := len(objs) + 1
	fmt.Fprintf(&buf, "xref\n0 %d\n", n)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i < n; i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", n, xrefOffset)
	return buf.Bytes()
}

func (pdfPageRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	id, ok := parseGenID(w, r, pdfPagePrefix)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	code := probe.CpageCode(s.pageCodeSecret, id, probe.CpageFieldRef, probe.FormatPageRevision)
	return renderPDFPage(id, code), "page.pdf", "application/pdf", pageHeaders{}, true
}
