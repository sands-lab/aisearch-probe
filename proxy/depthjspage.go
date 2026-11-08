package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
)

// JAVASCRIPT variants of the depth page: does a fetcher RUN the page's script, and
// does the model get content that only a script puts on the page? Real pages often
// write their text in with JavaScript; a fetcher that only downloads the HTML never
// sees it. This is the "JavaScript SEO" test (Vercel/MERJ 2024 found the AI
// TRAINING crawlers do not run scripts) applied to the question-time fetchers we
// study. See doc/js-depth-page.md.
//
// Every variant serves the SAME depth lines as /depth/html/ (the plain baseline),
// so truncrun reads them back unchanged. What differs is how lines past the top get
// onto the page:
//
//   - The 0% marker is ALWAYS plain HTML. It proves the page reached the model, so
//     "only the top code" reads as "arrived, script content not seen", never as
//     "never had the page".
//   - /depth/jsInline/ — the rest is SCRAMBLED inside an inline <script> that
//     unscrambles it and writes it in. The deeper codes are not in the served bytes
//     in any readable form, so quoting one means the script RAN (a model reading the
//     raw script cannot run thousands of keystream steps in its head).
//   - /depth/jsExternal/ — same, but the script lives in /depth/jsExternalScript/<id>/<bytes>.js.
//     Our log then also shows whether the fetcher even REQUESTED the script —
//     downloading a script is not running it (ClaudeBot does the first, not the second).
//   - /depth/jsData/ — the rest sits UNSCRAMBLED as JSON data in the page and a
//     script writes it in, the way many site frameworks ship their text. A fetcher
//     that keeps the raw page text hands the codes over without running anything.
//     This answers "did the model get the content", not "did the script run".
//
// Size in the path means the CONTENT size, as for the PDF/DOCX variants: the
// scrambled page is about a third larger because of base64.
//
// The scramble is a xorshift32 keystream XORed over the UTF-8 bytes. It is not
// secrecy — it only has to defeat reading by eye. Go writes it and JavaScript
// undoes it, two languages the compiler cannot reconcile, so
// TestDepthJSScriptRebuildsTheLines runs the served script under node and compares.

const (
	depthJSInlinePrefix = "/depth/jsInline/"
	depthJSExtPrefix    = "/depth/jsExternal/"
	depthJSDataPrefix   = "/depth/jsData/"
	depthJSFilePrefix   = "/depth/jsExternalScript/"
)

// depthJSSeed is the keystream's starting state, a pure function of (id, bytes) so
// the page and its script file agree and the ETag is stable. xorshift32 must never
// start at zero (it would stay zero), so zero becomes one.
func depthJSSeed(id string, nbytes int) uint32 {
	h := sha256.Sum256([]byte("depthjs|" + id + "|" + strconv.Itoa(nbytes)))
	if s := binary.BigEndian.Uint32(h[:4]); s != 0 {
		return s
	}
	return 1
}

// depthJSScramble XORs b with the xorshift32 keystream from seed, one low byte per
// step. Applying it twice gives b back. The JavaScript in depthJSDecoder is the
// exact mirror.
func depthJSScramble(b []byte, seed uint32) []byte {
	out := make([]byte, len(b))
	s := seed
	for i := range b {
		s ^= s << 13
		s ^= s >> 17
		s ^= s << 5
		out[i] = b[i] ^ byte(s)
	}
	return out
}

// depthJSWriter appends each line to #depth-body as its own <p>, matching
// renderDepthHTML's one-line-per-paragraph layout.
const depthJSWriter = `function depthWrite(t){var m=document.getElementById("depth-body");` +
	`for(var j=0;j<t.length;j++){var p=document.createElement("p");p.textContent=t[j];m.appendChild(p);}}`

// depthJSDecoder unscrambles the blob and writes the lines. The ">>>0" casts keep
// the state an unsigned 32-bit value, so every step matches Go's uint32 arithmetic.
const depthJSDecoder = `(function(){` + depthJSWriter +
	`var s=%d>>>0,b=atob("%s"),o=new Uint8Array(b.length);` +
	`for(var i=0;i<b.length;i++){s=(s^(s<<13))>>>0;s=(s^(s>>>17))>>>0;s=(s^(s<<5))>>>0;o[i]=b.charCodeAt(i)^(s&255);}` +
	`depthWrite(new TextDecoder("utf-8").decode(o).split("\n"));})();`

// depthJSScript is the script that rebuilds lines[1:] for one page.
func depthJSScript(id string, nbytes int, rest []string) string {
	seed := depthJSSeed(id, nbytes)
	blob := base64.StdEncoding.EncodeToString(depthJSScramble([]byte(strings.Join(rest, "\n")), seed))
	return fmt.Sprintf(depthJSDecoder, seed, blob)
}

// depthJSHead is the part every variant shares: the document start and the 0%
// marker as plain HTML, then the empty box the script fills.
func depthJSHead(lines []string) string {
	return "<!doctype html>\n<meta charset=\"utf-8\">\n<title>probe document</title>\n" +
		"<p>" + html.EscapeString(lines[0]) + "</p>\n<div id=\"depth-body\"></div>\n"
}

func renderDepthJSInline(id string, nbytes int, lines []string) []byte {
	return []byte(depthJSHead(lines) + "<script>" + depthJSScript(id, nbytes, lines[1:]) + "</script>\n")
}

// depthJSFilePath is the script address the external variant points at. The page
// and the file route both derive it from (id, bytes), so they cannot disagree.
func depthJSFilePath(id string, nbytes int) string {
	return depthJSFilePrefix + id + "/" + strconv.Itoa(nbytes) + ".js"
}

func renderDepthJSExt(id string, nbytes int, lines []string) []byte {
	return []byte(depthJSHead(lines) + "<script src=\"" + html.EscapeString(depthJSFilePath(id, nbytes)) + "\"></script>\n")
}

// renderDepthJSData puts lines[1:] in a JSON data block. json.Marshal escapes <, >
// and & as < etc., so the data can never close the <script> early.
func renderDepthJSData(lines []string) []byte {
	data, err := json.Marshal(struct {
		Document []string `json:"document"`
	}{Document: lines[1:]})
	if err != nil {
		data = []byte(`{"document":[]}`)
	}
	return []byte(depthJSHead(lines) +
		"<script id=\"depth-data\" type=\"application/json\">" + string(data) + "</script>\n" +
		"<script>(function(){" + depthJSWriter +
		`depthWrite(JSON.parse(document.getElementById("depth-data").textContent).document);})();</script>` + "\n")
}

// --- /depth/jsonLd/: the deeper markers in JSON-LD -------------------------
//
// JSON-LD is the structured data sites give search engines: a
// <script type="application/ld+json"> block in schema.org terms, which a browser
// never displays. /depth/jsData/ showed that the fetchers drop a plain JSON
// data block; this asks whether they drop JSON-LD too, which some text
// extractors keep on purpose. It differs from the data page only in the label,
// the schema.org wrapping and having no script to copy it in (a real JSON-LD
// block is never rendered). The deeper markers sit in articleBody, readable in
// the raw HTML, so a quoted deeper marker means the fetcher kept the JSON-LD.

const depthJSLDPrefix = "/depth/jsonLd/"

// renderDepthJSLD puts lines[1:] in a schema.org Article's articleBody, one line
// per paragraph. json.Marshal escapes <, > and & as < etc., so the data can
// never close the <script> early. Field order is fixed by the struct, so the
// bytes and the ETag are stable.
func renderDepthJSLD(lines []string) []byte {
	data, err := json.Marshal(struct {
		Context     string `json:"@context"`
		Type        string `json:"@type"`
		Headline    string `json:"headline"`
		ArticleBody string `json:"articleBody"`
	}{"https://schema.org", "Article", "probe document", strings.Join(lines[1:], "\n\n")})
	if err != nil {
		data = []byte(`{}`)
	}
	return []byte(depthJSHead(lines) +
		"<script type=\"application/ld+json\">" + string(data) + "</script>\n")
}

type depthJSLDRoute struct{}

func (depthJSLDRoute) prefix() string    { return depthJSLDPrefix }
func (depthJSLDRoute) needsSecret() bool { return true }
func (depthJSLDRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	_, _, lines, ok := depthJSLines(s, w, r, depthJSLDPrefix)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	return renderDepthJSLD(lines), "depth.html", depthJSHTMLType, pageHeaders{}, true
}

// depthJSLines is the shared head of every variant: parse /<prefix><id>/<bytes>,
// refuse a too-small page, and build the same lines as /depth/html/.
func depthJSLines(s *staticServer, w http.ResponseWriter, r *http.Request, prefix string) (string, int, []string, bool) {
	id, nbytes, ok := parseSizePath(w, r, prefix, maxDepthPageBytes)
	if !ok {
		return "", 0, nil, false
	}
	if nbytes < minDepthFormatBytes {
		http.Error(w, fmt.Sprintf("depth page too small: %d bytes cannot hold the markers", nbytes), http.StatusBadRequest)
		return "", 0, nil, false
	}
	return id, nbytes, depthFormatLines(s.pageCodeSecret, id, nbytes), true
}

const depthJSHTMLType = "text/html; charset=utf-8"

type depthJSInlineRoute struct{}

func (depthJSInlineRoute) prefix() string    { return depthJSInlinePrefix }
func (depthJSInlineRoute) needsSecret() bool { return true }
func (depthJSInlineRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	id, nbytes, lines, ok := depthJSLines(s, w, r, depthJSInlinePrefix)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	return renderDepthJSInline(id, nbytes, lines), "depth.html", depthJSHTMLType, pageHeaders{}, true
}

type depthJSExtRoute struct{}

func (depthJSExtRoute) prefix() string    { return depthJSExtPrefix }
func (depthJSExtRoute) needsSecret() bool { return true }
func (depthJSExtRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	id, nbytes, lines, ok := depthJSLines(s, w, r, depthJSExtPrefix)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	return renderDepthJSExt(id, nbytes, lines), "depth.html", depthJSHTMLType, pageHeaders{}, true
}

type depthJSDataRoute struct{}

func (depthJSDataRoute) prefix() string    { return depthJSDataPrefix }
func (depthJSDataRoute) needsSecret() bool { return true }
func (depthJSDataRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	_, _, lines, ok := depthJSLines(s, w, r, depthJSDataPrefix)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	return renderDepthJSData(lines), "depth.html", depthJSHTMLType, pageHeaders{}, true
}

// depthJSFileRoute serves the external variant's script at /depth/jsExternalScript/<id>/<bytes>.js.
// The ".js" is required: a fetcher may decide whether to download a subresource by
// its name, and a real site's script ends in .js. Normal headers, like the page, so
// a later run can compare how the script and the page are cached.
type depthJSFileRoute struct{}

func (depthJSFileRoute) prefix() string    { return depthJSFilePrefix }
func (depthJSFileRoute) needsSecret() bool { return true }
func (depthJSFileRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	if !strings.HasSuffix(r.URL.Path, ".js") {
		http.NotFound(w, r)
		return nil, "", "", pageHeaders{}, false
	}
	u := *r.URL
	u.Path = strings.TrimSuffix(u.Path, ".js")
	r2 := r.Clone(r.Context())
	r2.URL = &u
	id, nbytes, lines, ok := depthJSLines(s, w, r2, depthJSFilePrefix)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	return []byte(depthJSScript(id, nbytes, lines[1:]) + "\n"), "depth.js", "text/javascript; charset=utf-8", pageHeaders{}, true
}
