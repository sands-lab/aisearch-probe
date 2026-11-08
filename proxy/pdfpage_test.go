package main

import (
	"bytes"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"testing"
)

// /pdfpage/ is the first binary arm — the real question of the study. A PDF is a
// binary container the fetcher must run through a text extractor, so "fetched but
// code missing" means the fetcher got the bytes but could not read the text out.
// For that read to be a fair test, OUR PDF must be genuinely well-formed and the
// per-page code must be SELECTABLE TEXT, not an image. A malformed PDF would make a
// vendor that CAN read PDFs look like it cannot — a false negative. These tests guard
// that. A fixed code stands in for the real HMAC code in the structural checks.
const pdfTestCode = "codeABC123de"

// fetchBytes GETs a URL and returns the raw response body (binary-safe).
func fetchBytes(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp := get(t, url, nil)
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body of %s: %v", url, err)
	}
	return resp, b
}

func TestPDFPageIsStampedAndServed(t *testing.T) {
	srv, read, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")

	const id = "r0915-pdf-a"
	resp, raw := fetchBytes(t, srv.URL+"/pdfpage/"+id)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("PDF page has no ETag — a crawler could not revalidate it")
	}
	if _, present := resp.Header["Cache-Control"]; present {
		t.Errorf("PDF page sent Cache-Control %q, want it omitted (realistic header)", resp.Header.Get("Cache-Control"))
	}
	if code := fmtRefCode(id); !bytes.Contains(raw, []byte(code)) {
		t.Errorf("served PDF does not contain its per-page code %q", code)
	}
	recs := read()
	if len(recs) != 1 || !recs[0].Stamped() {
		t.Fatalf("the log must show the PDF page as stamped: %+v", recs)
	}
}

// READABILITY GUARD 1 — structure. The generated PDF must be well-formed: a
// %PDF-1.x header, a %%EOF, and a startxref whose offset actually points at the
// xref table. A broken object graph is the thing that would silently turn a
// "reads PDF" vendor into a false "doesn't read PDF".
func TestPDFPageStructureIsWellFormed(t *testing.T) {
	raw := renderPDFPage("struct-id", pdfTestCode)

	if !bytes.HasPrefix(raw, []byte("%PDF-1.")) {
		t.Errorf("PDF does not start with a %%PDF-1.x header: %q", first16(raw))
	}
	if !bytes.Contains(raw, []byte("%%EOF")) {
		t.Errorf("PDF has no %s trailer marker", "%%EOF")
	}
	if !bytes.Contains(raw, []byte("xref")) || !bytes.Contains(raw, []byte("trailer")) {
		t.Error("PDF is missing its xref table or trailer")
	}
	// startxref must name a byte offset that lands on the "xref" keyword.
	off := startxrefOffset(t, raw)
	if off < 0 || off >= len(raw) {
		t.Fatalf("startxref offset %d is out of range (len %d)", off, len(raw))
	}
	if !bytes.HasPrefix(raw[off:], []byte("xref")) {
		t.Errorf("startxref offset %d does not point at the xref table; found %q", off, first16(raw[off:]))
	}
}

// READABILITY GUARD 2 — selectable text, not an image. The code must sit inside a
// PDF text object (BT ... Tj ... ET) so a text extractor can pull it out. If it were
// drawn as an image the study would be measuring OCR, not text reading.
func TestPDFPageMarkerIsSelectableText(t *testing.T) {
	raw := renderPDFPage("text-id", pdfTestCode)
	bt := bytes.Index(raw, []byte("BT"))
	et := bytes.Index(raw, []byte("ET"))
	if bt < 0 || et < 0 || et < bt {
		t.Fatal("PDF has no BT...ET text object — code is not selectable text")
	}
	block := raw[bt:et]
	if !bytes.Contains(block, []byte("Tj")) && !bytes.Contains(block, []byte("TJ")) {
		t.Error("PDF text object has no Tj/TJ show-text operator")
	}
	if !bytes.Contains(block, []byte(pdfTestCode)) {
		t.Errorf("code is not shown as text inside the BT...ET block:\n%s", block)
	}
}

// READABILITY GUARD 3 — real extractor, when one is available. If pdftotext is on
// PATH, the generated PDF must extract the code as text. Skipped (not failed) where
// the tool is absent, so the suite stays std-lib and portable; the dev run does this
// by hand and notes it in the handover-back.
func TestPDFPageExtractsWithPdftotext(t *testing.T) {
	tool, err := exec.LookPath("pdftotext")
	if err != nil {
		t.Skip("pdftotext not installed — structural guards cover well-formedness; extraction confirmed by hand in dev")
	}
	raw := renderPDFPage("extract-id", pdfTestCode)
	cmd := exec.Command(tool, "-", "-") // read stdin, write stdout
	cmd.Stdin = bytes.NewReader(raw)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("pdftotext failed on the generated PDF (it is likely malformed): %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte(pdfTestCode)) {
		t.Errorf("pdftotext did not extract the code — the PDF is not readable text:\n%s", out.String())
	}
}

func TestPDFPageAnswers304(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")
	first := get(t, srv.URL+"/pdfpage/r0915-pdf-b", nil)
	first.Body.Close()
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on first fetch")
	}
	second := get(t, srv.URL+"/pdfpage/r0915-pdf-b", map[string]string{"If-None-Match": etag})
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation returned %d, want 304", second.StatusCode)
	}
}

func TestPDFPageBareIs404(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")
	resp := get(t, srv.URL+"/pdfpage/", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/pdfpage/ with no id returned %d, want 404", resp.StatusCode)
	}
}

// Deterministic bytes → stable ETag. A PDF built from fixed object text has no
// embedded timestamp, so two builds of the same inputs must be byte-identical.
func TestPDFPageDeterministic(t *testing.T) {
	if !bytes.Equal(renderPDFPage("same-id", pdfTestCode), renderPDFPage("same-id", pdfTestCode)) {
		t.Error("renderPDFPage is not deterministic — ETag would drift across builds")
	}
}

// startxrefOffset parses the integer following the last "startxref" keyword.
func startxrefOffset(t *testing.T, raw []byte) int {
	t.Helper()
	i := bytes.LastIndex(raw, []byte("startxref"))
	if i < 0 {
		t.Fatal("PDF has no startxref keyword")
	}
	rest := raw[i+len("startxref"):]
	// Skip leading whitespace/newlines, then read digits.
	j := 0
	for j < len(rest) && (rest[j] == '\r' || rest[j] == '\n' || rest[j] == ' ' || rest[j] == '\t') {
		j++
	}
	k := j
	for k < len(rest) && rest[k] >= '0' && rest[k] <= '9' {
		k++
	}
	n, err := strconv.Atoi(string(rest[j:k]))
	if err != nil {
		t.Fatalf("startxref value is not a number: %q", rest[j:k])
	}
	return n
}

func first16(b []byte) []byte {
	if len(b) > 16 {
		return b[:16]
	}
	return b
}
