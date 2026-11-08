package main

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf8"

	"aisearch-probe/internal/probe"
)

// The depth markers must survive the round-trip into each format AND come back in
// order — otherwise a vendor's "stopped at marker k" would be our extraction
// artifact, not the fetcher's cut. These tests are the guard to pass before any
// paid run.

const depthFmtSecret = "test-page-code-secret"
const depthFmtID = "sz-abc"
const depthFmtBytes = 20000 // multi-page / multi-paragraph

func wantDepthCodes() []string {
	codes := make([]string, len(probe.DepthPercents))
	for i, pct := range probe.DepthPercents {
		codes[i] = probe.DepthCode(depthFmtSecret, depthFmtID, pct, probe.DepthPageRevision)
	}
	return codes
}

// assertCodesInOrder checks every code appears in text, strictly increasing in
// position (so the ladder is intact and ordered), and that DeepestDepth reads the
// bottom marker.
func assertCodesInOrder(t *testing.T, text string) {
	t.Helper()
	prev := -1
	for i, c := range wantDepthCodes() {
		at := strings.Index(text, c)
		if at < 0 {
			t.Errorf("depth code #%d (%s) missing from extracted text", i, c)
			continue
		}
		if at <= prev {
			t.Errorf("depth code #%d (%s) is out of order (at %d, previous %d)", i, c, at, prev)
		}
		prev = at
	}
	if d := probe.DeepestDepth(text, depthFmtSecret, depthFmtID, probe.DepthPageRevision); d != probe.DepthPercents[len(probe.DepthPercents)-1] {
		t.Errorf("DeepestDepth = %d, want %d (deepest marker should read back)", d, probe.DepthPercents[len(probe.DepthPercents)-1])
	}
}

func TestDepthTxtRoundTrip(t *testing.T) {
	lines := depthFormatLines(depthFmtSecret, depthFmtID, depthFmtBytes)
	assertCodesInOrder(t, string(renderDepthTxt(lines)))
}

func TestDepthDocxRoundTrip(t *testing.T) {
	lines := depthFormatLines(depthFmtSecret, depthFmtID, depthFmtBytes)
	raw := renderDepthDocx(lines)
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("docx is not a valid zip: %v", err)
	}
	var doc []byte
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			rc, _ := f.Open()
			doc, _ = io.ReadAll(rc)
			rc.Close()
		}
	}
	if doc == nil {
		t.Fatal("docx has no word/document.xml")
	}
	// Strip tags so the codes are plain text, as an extractor would see them.
	text := stripXMLTags(string(doc))
	assertCodesInOrder(t, text)
}

// TestDepthPDFRoundTrip is the one that matters most: PDF text must extract in order
// via a real tool. Skips when pdftotext is absent (CI portability); the smoke run
// does it for real before spending.
func TestDepthPDFRoundTrip(t *testing.T) {
	tool, err := exec.LookPath("pdftotext")
	if err != nil {
		t.Skip("pdftotext not installed — PDF round-trip must be confirmed by hand before any paid run")
	}
	lines := depthFormatLines(depthFmtSecret, depthFmtID, depthFmtBytes)
	raw := renderDepthPDF(lines)
	cmd := exec.Command(tool, "-layout", "-", "-")
	cmd.Stdin = bytes.NewReader(raw)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("pdftotext failed on the generated PDF (malformed): %v", err)
	}
	assertCodesInOrder(t, out.String())
}

// TestDepthCtSameBytesDifferentLabel is the clean lever: /depth/declaredType/ serves the
// IDENTICAL body under different content-type tokens, so a read-depth difference
// between them is purely the label, not the layout.
func TestDepthCtSameBytesDifferentLabel(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, depthFmtSecret), "")

	rp := get(t, srv.URL+"/depth/declaredType/plain/ab/8192", nil)
	ctp := rp.Header.Get("Content-Type")
	etp := rp.Header.Get("ETag")
	bp := body(t, rp)

	rh := get(t, srv.URL+"/depth/declaredType/html/ab/8192", nil)
	cth := rh.Header.Get("Content-Type")
	eth := rh.Header.Get("ETag")
	bh := body(t, rh)

	if bp != bh {
		t.Error("bodies differ across content-type tokens — the A/B would not be byte-identical")
	}
	if etp != eth {
		t.Errorf("ETags differ (%q vs %q) — bodies are not identical", etp, eth)
	}
	if ctp != "text/plain; charset=utf-8" {
		t.Errorf("plain token Content-Type = %q", ctp)
	}
	if cth != "text/html; charset=utf-8" {
		t.Errorf("html token Content-Type = %q", cth)
	}
	if ctp == cth {
		t.Error("the two tokens must serve different Content-Types")
	}
	if n := strings.Count(bp, "TRUNC-"); n < 5 {
		t.Errorf("want all 5 markers in the body, got %d", n)
	}
	// A bad token must 404, not serve.
	rbad := get(t, srv.URL+"/depth/declaredType/bogus/ab/8192", nil)
	rbad.Body.Close()
	if rbad.StatusCode != http.StatusNotFound {
		t.Errorf("unknown content-type token returned %d, want 404", rbad.StatusCode)
	}
}

// TestDepthFormatEncoders: the same depth source, encoded into each object format,
// must stay VALID for that format AND keep all 5 markers in depth order (so the
// truncation read-back works in any format), and be deterministic.
func TestDepthFormatEncoders(t *testing.T) {
	lines := depthFormatLines(depthFmtSecret, depthFmtID, depthFmtBytes)
	cases := []struct {
		name   string
		render func([]string) []byte
		valid  func(*testing.T, []byte)
	}{
		{"md", renderDepthMd, func(t *testing.T, b []byte) {
			if !bytes.Contains(b, []byte("# ")) {
				t.Error("markdown has no heading")
			}
		}},
		{"html", renderDepthHTML, func(t *testing.T, b []byte) {
			if !bytes.Contains(b, []byte("<p>")) {
				t.Error("html has no <p> element")
			}
		}},
		{"json", renderDepthJSON, func(t *testing.T, b []byte) {
			if !json.Valid(b) {
				t.Errorf("not valid JSON:\n%s", b)
			}
		}},
		{"xml", renderDepthXML, func(t *testing.T, b []byte) {
			if err := wellFormedXML(b); err != nil {
				t.Errorf("not well-formed XML: %v", err)
			}
		}},
		{"csv", renderDepthCSV, func(t *testing.T, b []byte) {
			if _, err := csv.NewReader(bytes.NewReader(b)).ReadAll(); err != nil {
				t.Errorf("not valid CSV: %v", err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := c.render(lines)
			c.valid(t, b)
			assertCodesInOrder(t, string(b)) // all 5 markers survive, in depth order, deepest reads back
			if !bytes.Equal(b, c.render(lines)) {
				t.Error("not deterministic")
			}
		})
	}
}

// TestDepthFormatsValidUTF8 guards the bug caught 2026-10-06: the byte-exact size cut
// in depthCorpusLine sliced a multi-byte UTF-8 char (depthCorpus has smart quotes), so
// the 4 KB txt page was invalid UTF-8 and OpenAI's fetcher 400'd on it. Every format is
// built from the same lines, so all must stay valid UTF-8 at every size — especially the
// sizes near where a cut lands mid-character.
func TestDepthFormatsValidUTF8(t *testing.T) {
	renders := map[string]func([]string) []byte{
		"txt": renderDepthTxt, "md": renderDepthMd, "html": renderDepthHTML,
		"json": renderDepthJSON, "xml": renderDepthXML, "csv": renderDepthCSV,
	}
	for _, n := range []int{1024, 2048, 4096, 8192, 20000, 40000, 65536} {
		lines := depthFormatLines(depthFmtSecret, depthFmtID, n)
		for name, render := range renders {
			if b := render(lines); !utf8.Valid(b) {
				t.Errorf("size %d: %s render is not valid UTF-8 (a multi-byte char was sliced by the size cut)", n, name)
			}
		}
	}
}

func TestDepthFormatsDeterministic(t *testing.T) {
	l1 := depthFormatLines(depthFmtSecret, depthFmtID, depthFmtBytes)
	l2 := depthFormatLines(depthFmtSecret, depthFmtID, depthFmtBytes)
	if string(renderDepthTxt(l1)) != string(renderDepthTxt(l2)) {
		t.Error("txt not deterministic")
	}
	if !bytes.Equal(renderDepthPDF(l1), renderDepthPDF(l2)) {
		t.Error("pdf not deterministic")
	}
	if !bytes.Equal(renderDepthDocx(l1), renderDepthDocx(l2)) {
		t.Error("docx not deterministic")
	}
}

// stripXMLTags removes <...> so element text reads as a plain string.
func stripXMLTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
