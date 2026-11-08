package main

import (
	"crypto/sha256"
	"fmt"
	"html"
	"net/http"
	"strings"
	"sync"

	"aisearch-probe/internal/probe"
)

// Coded pages: like /page/, but each page prints two per-page UNFORGEABLE codes
// (see internal/probe/cpage.go) so a code in an answer proves a real fetch of THIS
// page. A SEPARATE route from /page/ on purpose — the retention run depends on
// /page/ bytes not moving, so this route never touches them. The codes need a
// server secret; with none set the route refuses (503) rather than emit a
// guessable page. See doc/content-reuse-cpage-needs.md.

const cpagePrefix = "/cpage/"

// cpageRevision is the content version. v1 for now; codes key on it, so a later
// mutation (v2) yields different codes without changing this route's shape.
const cpageRevision = 1

// cpageRoute is the /cpage/ route. It needs the secret (the codes are HMACs), so
// the shared spine 503s without one. See genpage.go.
type cpageRoute struct{}

func (cpageRoute) prefix() string    { return cpagePrefix }
func (cpageRoute) needsSecret() bool { return true }

// renderCpage builds the HTML for one coded page. The status line matches /page/
// so it reads as an ordinary live page; the subject and the two labelled codes are
// what the two experiments key on. A body of REAL study prose follows, so the page
// reads as genuine content rather than a thin stub — a fetcher's readability
// extractor keeps real prose and can discard a near-empty page, and the content-
// reuse test needs a page a provider would actually store and index by content.
// The prose reuses the sanitised findings corpus embedded for the depth probe
// (depthCorpus): by construction it carries no code-shaped string and narrates no
// probe, so it can neither fake a code nor explain the experiment to the model.
//
// Every field the earlier stub carried is still present and in the same labelled
// form, so consumers that scan for "reference code" (webtrial) are unaffected.
func renderCpage(id, subject, ref, rec string, revision int) string {
	return fmt.Sprintf(`<!doctype html>
<meta charset="utf-8">
<title>coded page %s — %s</title>
<h1>%s</h1>
<p>status line: this page is live and serving normally.</p>
<p>page id: %s</p>
<p>subject: %s</p>
<p>reference code: %s</p>
<p>record number: %s</p>
<p>revision: %d</p>
<hr>
<h2>Background from the study</h2>
%s
`, id, subject, subject, id, subject, ref, rec, revision, cpageProse(id))
}

// cpageMaxProse bounds the real-prose body so a coded page stays small.
const cpageMaxProse = 2500

// cpageDropTerms name the coded-page scheme and this very experiment. The shared
// depthCorpus is sanitised for the DEPTH probe, but NOT for content-reuse — and
// our own study notes describe the content-reuse method in detail (the warm, the
// blind ask, the record number as proof). Serving that prose would hand the model
// the trick and reveal that the code is a secret. So cpageCorpus drops any
// paragraph mentioning these before the prose is ever used as a body.
var cpageDropTerms = []string{
	"content-reuse", "content-level", "cpage", "record number", "reference code",
	"never named", "twelfth question", "unforgeable", "hmac", "page-code-secret",
	"probe_page_code", "retrieval-bootstrap", "blind step", "internal store",
}

var (
	cpageCorpusOnce sync.Once
	cpageCorpusText string
)

// cpageCorpus is the depth corpus with every self-describing paragraph removed,
// built once. Paragraph-based (blank-line-delimited) so a matched term takes its
// whole block, never leaving a dangling sentence that half-explains the scheme. A
// test asserts the result contains none of cpageDropTerms.
func cpageCorpus() string {
	cpageCorpusOnce.Do(func() {
		var b strings.Builder
		for _, block := range strings.Split(depthCorpus, "\n\n") {
			low := strings.ToLower(block)
			drop := false
			for _, t := range cpageDropTerms {
				if strings.Contains(low, t) {
					drop = true
					break
				}
			}
			if !drop {
				b.WriteString(block)
				b.WriteString("\n\n")
			}
		}
		cpageCorpusText = b.String()
	})
	return cpageCorpusText
}

// cpageProse returns a bounded excerpt of the sanitised study corpus for this id,
// wrapped as one escaped paragraph. The start offset is a stable hash of the id,
// so different pages show different but FIXED excerpts — keeping the page a pure
// function of its id, so the ETag over the bytes stays stable and revalidation
// works. It wraps around the end of the corpus, so any id resolves to a
// full-length excerpt.
func cpageProse(id string) string {
	corpus := cpageCorpus()
	if len(corpus) == 0 {
		return ""
	}
	h := sha256.Sum256([]byte("cpage-prose|" + id))
	start := (int(h[0])<<8 | int(h[1])) % len(corpus)
	n := cpageMaxProse
	if n > len(corpus) {
		n = len(corpus)
	}
	var chunk string
	if start+n <= len(corpus) {
		chunk = corpus[start : start+n]
	} else {
		chunk = corpus[start:] + corpus[:start+n-len(corpus)]
	}
	return "<p>" + html.EscapeString(chunk) + "</p>"
}

// build answers a request for a coded page. The spine has already 503'd if no
// secret is set. An unknown register index is a 404, so a typo cannot read as a
// real coded page. Normal headers (pageHeaders{}), like every route but /ccdepthpage/.
func (cpageRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	id := strings.TrimPrefix(r.URL.Path, cpagePrefix)
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return nil, "", "", pageHeaders{}, false
	}
	subject, ok := probe.CpageSubjectForID(id)
	if !ok {
		http.NotFound(w, r)
		return nil, "", "", pageHeaders{}, false
	}
	ref := probe.CpageCode(s.pageCodeSecret, id, probe.CpageFieldRef, cpageRevision)
	rec := probe.CpageCode(s.pageCodeSecret, id, probe.CpageFieldRec, cpageRevision)
	return []byte(renderCpage(id, subject, ref, rec, cpageRevision)), "cpage.html", "text/html; charset=utf-8", pageHeaders{}, true
}
