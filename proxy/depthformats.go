package main

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"aisearch-probe/internal/probe"
)

// FORMAT variants of the depth page (truncation-depth probe), for the object-size
// study: does read depth and input-token cost scale differently by FORMAT? The
// original /depthpage/ serves marker-laden real prose as HTML; these serve the SAME
// depth markers (probe.DepthCode at probe.DepthPercents, so truncrun reads them back
// unchanged) as plain text, PDF, and DOCX.
//
// Why a LINE-based payload, not the byte-exact one /depthpage/ uses: a PDF or DOCX
// extractor returns text per line/paragraph, and a marker split across a line break
// would not survive the round-trip. So each marker sits on its OWN line, and the
// filler is whole lines of corpus. The marker still lands near its depth; the exact
// byte offset is approximate (fine for a cut READING, which is ordinal), and the
// ordinal depth — which marker was the deepest quoted — is exact.
//
// All three need the server secret (the codes are HMACs), like /depthpage/. They are
// sent with the normal headers (no Cache-Control). Sizes come in the PATH (/depthXpage/<id>/<bytes>) and
// mean the EXTRACTED-TEXT size (what the fetcher's token window sees) — the PDF/DOCX
// file is larger because of its container, which is the point of the comparison.

// depthFormatLines builds the payload as lines totalling ~nbytes: whole lines of
// corpus filler, with each depth marker inserted on its own line near its byte depth.
func depthFormatLines(secret, id string, nbytes int) []string {
	return depthFormatLinesOpt(secret, id, nbytes, true)
}

// depthFormatLinesOpt is depthFormatLines with a UTF-8 safety switch. trimUTF8=true is
// the normal, correct payload (every line valid UTF-8). trimUTF8=false DELIBERATELY
// leaves the pre-fix corruption in (filler lines chopped mid-character) — a fault
// injection used ONLY by the /depthpdfrawpage/ investigation route to test whether
// invalid-UTF-8 content is what triggered Anthropic's pause_turn bomb. Nothing on the
// normal path passes false.
func depthFormatLinesOpt(secret, id string, nbytes int, trimUTF8 bool) []string {
	return depthFormatLinesRev(secret, id, nbytes, trimUTF8, probe.DepthPageRevision)
}

// depthFormatLinesRev is the generator itself, with the codes made at an explicit
// revision. Every depth route passes probe.DepthPageRevision (via the two wrappers
// above); only the changeable page (verpage.go) passes its current version.
func depthFormatLinesRev(secret, id string, nbytes int, trimUTF8 bool, revision int) []string {
	offsets := make([]int, len(probe.DepthPercents))
	for i, pct := range probe.DepthPercents {
		offsets[i] = probe.DepthOffset(pct, nbytes)
	}
	var lines []string
	cur, mi, ci := 0, 0, 0
	for cur < nbytes {
		if mi < len(offsets) && cur >= offsets[mi] {
			ln := depthMarkerBlockRev(secret, id, probe.DepthPercents[mi], cur, revision)
			lines = append(lines, ln)
			cur += len(ln) + 1
			mi++
			continue
		}
		ln := depthCorpusLineOpt(&ci, 72, trimUTF8)
		lines = append(lines, ln)
		cur += len(ln) + 1
	}
	// Any markers past the last filler line (tiny pages) still get laid down, in order.
	for ; mi < len(offsets); mi++ {
		ln := depthMarkerBlockRev(secret, id, probe.DepthPercents[mi], cur, revision)
		lines = append(lines, ln)
		cur += len(ln) + 1
	}
	return lines
}

// depthCorpusLine returns n bytes of the frozen corpus as one clean line (no
// newlines), cycling. The corpus never contains the marker prefix (enforced by a
// test on /depthpage/), so filler cannot spell a marker.
func depthCorpusLine(ci *int, n int) string {
	return depthCorpusLineOpt(ci, n, true)
}

// depthCorpusLineOpt builds the filler line. The cut counts raw BYTES and can land
// mid-character, because depthCorpus holds multi-byte UTF-8 (smart quotes, em-dashes).
// A chopped trailing rune makes the line — and every format rendered from it — invalid
// UTF-8, which OpenAI's fetcher rejects with a 400 Unicode decoding error (measured
// 2026-10-06 at 4 KB; Anthropic/xAI silently read the corrupted bytes). With trimUTF8
// (the normal path) we drop any incomplete trailing rune so the line is always valid.
// trimUTF8=false deliberately keeps the chopped rune — the pre-fix corruption — for the
// /depthpdfrawpage/ bomb investigation ONLY.
func depthCorpusLineOpt(ci *int, n int, trimUTF8 bool) string {
	b := make([]byte, 0, n)
	for len(b) < n {
		c := depthCorpus[*ci%len(depthCorpus)]
		*ci++
		if c == '\n' || c == '\r' {
			c = ' '
		}
		b = append(b, c)
	}
	if trimUTF8 {
		for len(b) > 0 && !utf8.Valid(b) {
			b = b[:len(b)-1]
		}
	}
	return string(b)
}

// minDepthFormatBytes is the floor below which the markers do not fit as lines.
const minDepthFormatBytes = 1024

// --- shared build helper ---------------------------------------------------

func buildDepthFormat(s *staticServer, w http.ResponseWriter, r *http.Request, prefix string,
	render func(lines []string) []byte, name, contentType string) ([]byte, string, string, pageHeaders, bool) {
	id, nbytes, ok := parseSizePath(w, r, prefix, maxDepthPageBytes)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	if nbytes < minDepthFormatBytes {
		http.Error(w, fmt.Sprintf("depth page too small: %d bytes cannot hold the markers", nbytes), http.StatusBadRequest)
		return nil, "", "", pageHeaders{}, false
	}
	lines := depthFormatLines(s.pageCodeSecret, id, nbytes)
	// Realistic header (omit Cache-Control) — these are read-depth/format experiments,
	// NOT cache-control tests, so no-store would be an abnormal setup (BULLETIN 2026-10-04).
	return render(lines), name, contentType, pageHeaders{}, true
}

// buildDepthFormatRaw is buildDepthFormat with the UTF-8 safety trim DISABLED — it
// serves the pre-fix invalid-UTF-8 payload on purpose. FAULT INJECTION for the
// pause_turn bomb investigation (was corrupted content the trigger?): only the explicit
// /depthpdfrawpage/ route calls it, so no normal run can serve corruption by accident.
func buildDepthFormatRaw(s *staticServer, w http.ResponseWriter, r *http.Request, prefix string,
	render func(lines []string) []byte, name, contentType string) ([]byte, string, string, pageHeaders, bool) {
	id, nbytes, ok := parseSizePath(w, r, prefix, maxDepthPageBytes)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	if nbytes < minDepthFormatBytes {
		http.Error(w, fmt.Sprintf("depth page too small: %d bytes cannot hold the markers", nbytes), http.StatusBadRequest)
		return nil, "", "", pageHeaders{}, false
	}
	lines := depthFormatLinesOpt(s.pageCodeSecret, id, nbytes, false) // NO trim -> invalid UTF-8
	// Realistic header (omit Cache-Control), same as buildDepthFormat — not a cache test.
	return render(lines), name, contentType, pageHeaders{}, true
}

// --- /depthtxtpage/ --------------------------------------------------------

const depthTxtPrefix = "/depthtxtpage/"

type depthTxtRoute struct{}

func (depthTxtRoute) prefix() string    { return depthTxtPrefix }
func (depthTxtRoute) needsSecret() bool { return true }

func renderDepthTxt(lines []string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

func (depthTxtRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	return buildDepthFormat(s, w, r, depthTxtPrefix, renderDepthTxt, "depth.txt", "text/plain; charset=utf-8")
}

// --- /depthpdfpage/ --------------------------------------------------------

const depthPdfPrefix = "/depthpdfpage/"

// depthPdfLinesPerPage keeps every line inside the MediaBox so pdftotext (and a
// vendor extractor) returns all of it — the single-page experiment showed off-page
// lines are clipped and lost. 55 lines at 9pt/12 leading from y=770 ends near y=110.
const depthPdfLinesPerPage = 55

type depthPdfRoute struct{}

func (depthPdfRoute) prefix() string    { return depthPdfPrefix }
func (depthPdfRoute) needsSecret() bool { return true }

// renderDepthPDF lays the lines across as many pages as needed, each a linear content
// stream, so extraction preserves order. Object layout: 1 catalog, 2 pages, 3 font,
// then per page a Page object (4+2p) and its content stream (5+2p).
func renderDepthPDF(lines []string) []byte {
	var streams []string
	for i := 0; i < len(lines); i += depthPdfLinesPerPage {
		end := i + depthPdfLinesPerPage
		if end > len(lines) {
			end = len(lines)
		}
		var cs bytes.Buffer
		cs.WriteString("BT /F1 9 Tf 12 TL 20 770 Td\n")
		for _, ln := range lines[i:end] {
			cs.WriteString("(" + pdfEscapeString(ln) + ") Tj T*\n")
		}
		cs.WriteString("ET\n")
		streams = append(streams, cs.String())
	}
	nPages := len(streams)
	kids := make([]string, nPages)
	for p := 0; p < nPages; p++ {
		kids[p] = strconv.Itoa(4+2*p) + " 0 R"
	}
	var objs []string
	objs = append(objs, "<< /Type /Catalog /Pages 2 0 R >>")
	objs = append(objs, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), nPages))
	objs = append(objs, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for p := 0; p < nPages; p++ {
		contentNum := 5 + 2*p
		objs = append(objs, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", contentNum))
		cs := streams[p]
		objs = append(objs, "<< /Length "+strconv.Itoa(len(cs))+" >>\nstream\n"+cs+"endstream")
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	off := make([]int, len(objs)+1)
	for i, o := range objs {
		off[i+1] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := b.Len()
	n := len(objs) + 1
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", n)
	for i := 1; i < n; i++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", off[i])
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", n, x)
	return b.Bytes()
}

func (depthPdfRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	return buildDepthFormat(s, w, r, depthPdfPrefix, renderDepthPDF, "depth.pdf", "application/pdf")
}

// --- /depthpdfrawpage/ — FAULT INJECTION, investigation only ----------------
// Serves the PDF built from the pre-fix INVALID-UTF-8 payload (chopped multi-byte
// chars), to test whether that corruption is what triggered Anthropic's pause_turn cost
// bomb (the clean /depthpdfpage/ never bombs — FINDINGS). Deliberately NOT a general
// format route: it exists only so one Anthropic call can be run against corrupted PDF
// bytes and its `rounds` compared to the clean one. Retire once the question is settled.

const depthPdfRawPrefix = "/depthpdfrawpage/"

// --- /depthdocxpage/ -------------------------------------------------------

const depthDocxPrefix = "/depthdocxpage/"

type depthDocxRoute struct{}

func (depthDocxRoute) prefix() string    { return depthDocxPrefix }
func (depthDocxRoute) needsSecret() bool { return true }

// renderDepthDocx writes each line as its own paragraph, so extraction preserves
// order. Deterministic zip (fixed modtime, Store), like docxpage.go.
func renderDepthDocx(lines []string) []byte {
	var body bytes.Buffer
	for _, ln := range lines {
		var esc bytes.Buffer
		_ = xml.EscapeText(&esc, []byte(ln))
		body.WriteString(`<w:p><w:r><w:t xml:space="preserve">` + esc.String() + `</w:t></w:r></w:p>`)
	}
	doc := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + body.String() + `</w:body></w:document>`

	parts := []struct{ name, data string }{
		{"[Content_Types].xml", docxContentTypesXML},
		{"_rels/.rels", docxRelsXML},
		{"word/document.xml", doc},
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range parts {
		hdr := &zip.FileHeader{Name: p.name, Method: zip.Store, Modified: pageModTime}
		wtr, err := zw.CreateHeader(hdr)
		if err != nil {
			continue
		}
		_, _ = wtr.Write([]byte(p.data))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func (depthDocxRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	return buildDepthFormat(s, w, r, depthDocxPrefix, renderDepthDocx, "depth.docx",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document")
}

// --- /depthctpage/ — the content-type LABEL lever --------------------------
//
// Serves the IDENTICAL line-based body under a content-type chosen by a path token
// (/depthctpage/<token>/<id>/<bytes>). Same bytes, only the label differs, so a
// read-depth difference between tokens is purely the content-type — the clean,
// byte-identical A/B that /depthpage/-vs-/depthtxtpage/ could not be (their bodies
// differ in layout). This is how we isolate "does the stamp change the read window".

const depthCtPrefix = "/depthctpage/"

// depthCtTypes maps a path token to the Content-Type served. The body is the same
// prose+markers regardless; only this label changes. A test pins the table.
var depthCtTypes = map[string]string{
	"plain": "text/plain; charset=utf-8",
	"html":  "text/html; charset=utf-8",
	"xml":   "application/xml; charset=utf-8",
	"md":    "text/markdown; charset=utf-8",
	"csv":   "text/csv; charset=utf-8",
}

type depthCtRoute struct{}

func (depthCtRoute) prefix() string    { return depthCtPrefix }
func (depthCtRoute) needsSecret() bool { return true }

func (depthCtRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, depthCtPrefix), "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		http.NotFound(w, r)
		return nil, "", "", pageHeaders{}, false
	}
	ct, ok := depthCtTypes[parts[0]]
	if !ok {
		http.NotFound(w, r)
		return nil, "", "", pageHeaders{}, false
	}
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
		http.Error(w, fmt.Sprintf("depth page too small: %d bytes cannot hold the markers", nbytes), http.StatusBadRequest)
		return nil, "", "", pageHeaders{}, false
	}
	// The body is the SAME line-based bytes for every token — only ct changes.
	lines := depthFormatLines(s.pageCodeSecret, parts[1], nbytes)
	// Realistic header (omit Cache-Control) — this is the content-type lever, not a
	// cache-control test (BULLETIN 2026-10-04).
	return renderDepthTxt(lines), "depth.txt", ct, pageHeaders{}, true
}

// --- content-equivalent encoders: ONE depth source → every object format -----
//
// These take the SAME depthFormatLines output (prose + markers at byte depths) and
// encode it as a VALID document in each format, so one source yields all formats and
// the only variable is the format (fixed size) or the size (same format). The TRUNC
// code is alphanumeric, so it survives JSON/XML/CSV/HTML escaping and
// probe.DeepestDepth reads back how deep each fetcher got, in any format. txt/pdf/docx
// already have their encoders above; these add md/html/json/xml/csv.

// renderDepthMd: lines as Markdown paragraphs (a heading so it reads as Markdown).
func renderDepthMd(lines []string) []byte {
	return []byte("# probe document\n\n" + strings.Join(lines, "\n\n") + "\n")
}

// renderDepthHTML: each line as a <p>, HTML-escaped. Marker text stays selectable.
func renderDepthHTML(lines []string) []byte {
	var b bytes.Buffer
	b.WriteString("<!doctype html>\n<meta charset=\"utf-8\">\n<title>probe document</title>\n")
	for _, ln := range lines {
		b.WriteString("<p>")
		b.WriteString(html.EscapeString(ln))
		b.WriteString("</p>\n")
	}
	return b.Bytes()
}

// renderDepthJSON: a valid JSON object whose "document" array holds the lines in
// order, so a marker's position in the array tracks its byte depth in the serialized
// JSON. json.Marshal is deterministic for a fixed struct.
func renderDepthJSON(lines []string) []byte {
	doc := struct {
		Document []string `json:"document"`
	}{Document: lines}
	b, err := json.Marshal(doc)
	if err != nil {
		return []byte(`{"document":[]}`)
	}
	return b
}

// renderDepthXML: each line as a <line> element under <document>, XML-escaped.
func renderDepthXML(lines []string) []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString("<document>")
	for _, ln := range lines {
		b.WriteString("<line>")
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(ln))
		b.Write(e.Bytes())
		b.WriteString("</line>")
	}
	b.WriteString("</document>")
	return b.Bytes()
}

// renderDepthCSV: a header plus one row per line (index, content). encoding/csv
// quotes cells as needed (marker lines contain a comma), and writes deterministically.
func renderDepthCSV(lines []string) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"n", "content"})
	for i, ln := range lines {
		_ = w.Write([]string{strconv.Itoa(i), ln})
	}
	w.Flush()
	return buf.Bytes()
}

// --- routes for the five added formats ---------------------------------------

const depthMdPrefix = "/depthmdpage/"

type depthMdRoute struct{}

func (depthMdRoute) prefix() string    { return depthMdPrefix }
func (depthMdRoute) needsSecret() bool { return true }
func (depthMdRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	return buildDepthFormat(s, w, r, depthMdPrefix, renderDepthMd, "depth.md", "text/markdown; charset=utf-8")
}

const depthHTMLPrefix = "/depthhtmlpage/"

type depthHTMLRoute struct{}

func (depthHTMLRoute) prefix() string    { return depthHTMLPrefix }
func (depthHTMLRoute) needsSecret() bool { return true }
func (depthHTMLRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	return buildDepthFormat(s, w, r, depthHTMLPrefix, renderDepthHTML, "depth.html", "text/html; charset=utf-8")
}

const depthJSONPrefix = "/depthjsonpage/"

type depthJSONRoute struct{}

func (depthJSONRoute) prefix() string    { return depthJSONPrefix }
func (depthJSONRoute) needsSecret() bool { return true }
func (depthJSONRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	return buildDepthFormat(s, w, r, depthJSONPrefix, renderDepthJSON, "depth.json", "application/json")
}

const depthXMLPrefix = "/depthxmlpage/"

type depthXMLRoute struct{}

func (depthXMLRoute) prefix() string    { return depthXMLPrefix }
func (depthXMLRoute) needsSecret() bool { return true }
func (depthXMLRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	return buildDepthFormat(s, w, r, depthXMLPrefix, renderDepthXML, "depth.xml", "application/xml; charset=utf-8")
}

const depthCSVPrefix = "/depthcsvpage/"

type depthCSVRoute struct{}

func (depthCSVRoute) prefix() string    { return depthCSVPrefix }
func (depthCSVRoute) needsSecret() bool { return true }
func (depthCSVRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	return buildDepthFormat(s, w, r, depthCSVPrefix, renderDepthCSV, "depth.csv", "text/csv; charset=utf-8")
}
