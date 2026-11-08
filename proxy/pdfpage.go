package main

import (
	"strings"
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

// pdfEscapeString escapes the three characters that are special inside a PDF literal
// string: backslash and the two parentheses. The ids we use are alphanumeric, but
// escaping keeps the generator correct for any id.
func pdfEscapeString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`)
	return r.Replace(s)
}
