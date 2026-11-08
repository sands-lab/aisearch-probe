package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"aisearch-probe/internal/probe"
)

// The JavaScript depth pages are only a measurement if two things hold: a script
// that RUNS rebuilds exactly the depth lines (else "no deep code quoted" is our bug),
// and the scrambled pages hide every code past the top (else quoting one proves
// nothing about running). These tests check both against the served bytes.

// depthJSHarness runs one script under node with just enough of a document for it:
// #depth-body collects each written paragraph, #depth-data returns the data block.
const depthJSHarness = `
const inp = JSON.parse(require("fs").readFileSync(0, "utf8"));
const out = [];
globalThis.document = {
  getElementById(id) {
    if (id === "depth-body") return { appendChild(p) { out.push(p.textContent); } };
    if (id === "depth-data") return { textContent: inp.data };
    return null;
  },
  createElement() { return { textContent: "" }; },
};
(0, eval)(inp.script);
process.stdout.write(JSON.stringify(out));
`

func runDepthJS(t *testing.T, node, script, data string) []string {
	t.Helper()
	in, _ := json.Marshal(map[string]string{"script": script, "data": data})
	cmd := exec.Command(node, "-e", depthJSHarness)
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, stderr.String())
	}
	var got []string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("node output is not a JSON list: %v\n%s", err, raw)
	}
	return got
}

// between returns the text after the LAST open and before the next close.
func between(t *testing.T, s, open, close string) string {
	t.Helper()
	i := strings.LastIndex(s, open)
	if i < 0 {
		t.Fatalf("%q not found", open)
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		t.Fatalf("%q not closed", open)
	}
	return rest[:j]
}

func depthJSGet(t *testing.T, path string) (string, *http.Response) {
	t.Helper()
	srv, _, _ := newTestProxy(t, cpageServer(t, depthFmtSecret), "")
	resp := get(t, srv.URL+path, nil)
	return body(t, resp), resp
}

const depthJSSize = "/sz-abc/20000" // depthFmtID at depthFmtBytes

// THE GO↔JAVASCRIPT PAIR: run the served script for real and require it to write
// exactly the lines /depthhtmlpage/ prints below the top marker, with every depth
// code back in order.
func TestDepthJSScriptRebuildsTheLines(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot run the page script")
	}
	lines := depthFormatLines(depthFmtSecret, depthFmtID, depthFmtBytes)
	want := lines[1:]

	inline, _ := depthJSGet(t, depthJSInlinePrefix[:len(depthJSInlinePrefix)-1]+depthJSSize)
	file, _ := depthJSGet(t, depthJSFilePath(depthFmtID, depthFmtBytes))
	data, _ := depthJSGet(t, depthJSDataPrefix[:len(depthJSDataPrefix)-1]+depthJSSize)

	for name, got := range map[string][]string{
		"inline":   runDepthJS(t, node, between(t, inline, "<script>", "</script>"), ""),
		"external": runDepthJS(t, node, file, ""),
		"data": runDepthJS(t, node, between(t, data, "<script>", "</script>"),
			between(t, data, `type="application/json">`, "</script>")),
	} {
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: script wrote %d lines that differ from the %d depth lines", name, len(got), len(want))
			continue
		}
		assertCodesInOrder(t, lines[0]+"\n"+strings.Join(got, "\n"))
	}
}

// Without running anything, the scrambled pages show the top code and nothing
// deeper; the script file shows no code at all; the data page shows every code.
// Scored with the runner's own reader, so this is what an answer quoting the raw
// bytes would score.
func TestDepthJSRawBytesScoreAsIntended(t *testing.T) {
	deepest := probe.DepthPercents[len(probe.DepthPercents)-1]
	for _, c := range []struct {
		path string
		want int
	}{
		{depthJSInlinePrefix[:len(depthJSInlinePrefix)-1] + depthJSSize, 0},
		{depthJSExtPrefix[:len(depthJSExtPrefix)-1] + depthJSSize, 0},
		{depthJSFilePath(depthFmtID, depthFmtBytes), -1},
		{depthJSDataPrefix[:len(depthJSDataPrefix)-1] + depthJSSize, deepest},
	} {
		raw, resp := depthJSGet(t, c.path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", c.path, resp.StatusCode)
		}
		if got := probe.DeepestDepth(raw, depthFmtSecret, depthFmtID, probe.DepthPageRevision); got != c.want {
			t.Errorf("%s: raw bytes score deepest=%d, want %d", c.path, got, c.want)
		}
	}
}

// The external page must point at the script for the SAME id and size, served as
// JavaScript with the normal page headers.
func TestDepthJSExtPagePointsAtItsFile(t *testing.T) {
	page, _ := depthJSGet(t, depthJSExtPrefix[:len(depthJSExtPrefix)-1]+depthJSSize)
	if want := `<script src="` + depthJSFilePath(depthFmtID, depthFmtBytes) + `">`; !strings.Contains(page, want) {
		t.Fatalf("external page does not load %s", want)
	}
	_, resp := depthJSGet(t, depthJSFilePath(depthFmtID, depthFmtBytes))
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("script Content-Type = %q, want text/javascript", ct)
	}
	if resp.Header.Get("ETag") == "" || resp.Header.Get("Cache-Control") != "" {
		t.Errorf("script headers: ETag=%q Cache-Control=%q, want an ETag and no Cache-Control",
			resp.Header.Get("ETag"), resp.Header.Get("Cache-Control"))
	}
}

// The script file answers only at its .js name; a malformed address is never a page.
func TestDepthJSMalformedIs404(t *testing.T) {
	for _, p := range []string{
		"/depthjsfile/sz-abc/20000",
		"/depthjsfile/sz-abc.js",
		"/depthjsinlinepage/sz-abc",
		"/depthjsdatapage/",
	} {
		if _, resp := depthJSGet(t, p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, resp.StatusCode)
		}
	}
}

// The scramble undoes itself and changes every page's bytes differently.
func TestDepthJSScrambleRoundTrip(t *testing.T) {
	in := []byte("TRUNC-abc is the reference marker — with UTF-8")
	seed := depthJSSeed("x", 4096)
	once := depthJSScramble(in, seed)
	if bytes.Equal(once, in) {
		t.Fatal("scramble left the bytes unchanged")
	}
	if !bytes.Equal(depthJSScramble(once, seed), in) {
		t.Fatal("scrambling twice did not give the bytes back")
	}
	if depthJSSeed("x", 4096) == depthJSSeed("y", 4096) || depthJSSeed("x", 4096) == depthJSSeed("x", 8192) {
		t.Error("seed does not depend on both id and size")
	}
}

// A switch time that is not a number, or not after the fixed page date, is a typo.
func TestDepthJSVerMalformedIs404(t *testing.T) {
	for _, p := range []string{
		"/depthjsverpage/sz-abc/soon/20000",
		"/depthjsverpage/sz-abc/1000/20000", // before probe.PageLastModified
		"/depthjsverpage/sz-abc/20000",
		"/depthjsverfile/sz-abc/1893456000/20000", // no .js
	} {
		if _, resp := depthJSGet(t, p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, resp.StatusCode)
		}
	}
}

// JSON-LD: the deeper markers sit only in a valid schema.org Article's articleBody,
// readable in the raw HTML but never displayed. With the block removed, the page
// shows the top marker and nothing deeper; so a quoted deeper marker can only come
// from the fetcher keeping the JSON-LD.
func TestDepthJSLDCarriesTheMarkersOnlyInTheBlock(t *testing.T) {
	raw, resp := depthJSGet(t, depthJSLDPrefix[:len(depthJSLDPrefix)-1]+depthJSSize)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if n := strings.Count(raw, "<script"); n != 1 {
		t.Fatalf("page has %d <script> tags, want exactly the JSON-LD block", n)
	}
	const open = `<script type="application/ld+json">`
	block := between(t, raw, open, "</script>")
	var ld map[string]any
	if err := json.Unmarshal([]byte(block), &ld); err != nil {
		t.Fatalf("JSON-LD block is not valid JSON: %v", err)
	}
	if ld["@context"] != "https://schema.org" || ld["@type"] != "Article" {
		t.Errorf("JSON-LD is not a schema.org Article: @context=%v @type=%v", ld["@context"], ld["@type"])
	}
	lines := depthFormatLines(depthFmtSecret, depthFmtID, depthFmtBytes)
	body, _ := ld["articleBody"].(string)
	if body != strings.Join(lines[1:], "\n\n") {
		t.Error("articleBody is not the depth lines below the top marker")
	}
	assertCodesInOrder(t, lines[0]+"\n"+body)

	deepest := probe.DepthPercents[len(probe.DepthPercents)-1]
	if d := probe.DeepestDepth(raw, depthFmtSecret, depthFmtID, probe.DepthPageRevision); d != deepest {
		t.Errorf("raw bytes score %d, want %d (the block is readable)", d, deepest)
	}
	i := strings.Index(raw, open)
	visible := raw[:i] + raw[i+len(open)+len(block)+len("</script>"):]
	if d := probe.DeepestDepth(visible, depthFmtSecret, depthFmtID, probe.DepthPageRevision); d != 0 {
		t.Errorf("page without the block scores %d, want 0 (top marker only)", d)
	}
}
