package main

import (
	_ "embed"
	"fmt"

	"aisearch-probe/internal/probe"
)

// depthCorpus is the frozen filler: a SNAPSHOT of this project's findings prose,
// built by scripts/build-depth-corpus.py and sanitised so it never contains the
// marker prefix. Real prose (not repeated filler characters) is what a fetcher's
// readability extractor keeps, so the markers actually reach the model. It is
// embedded so the page is a pure function with no runtime file dependency. See
// doc/truncation-depth-probe-design.md.
//
//go:embed depthcorpus.txt
var depthCorpus string

// Depth pages: a long page carrying UNFORGEABLE markers at known byte depths, so
// the deepest one a model can quote tells us where its fetcher CUT the page — the
// truncation-depth probe (doc/truncation-depth-probe-handover.md).
//
// A SEPARATE route from /page/ and /sizepage/ on purpose: it never touches their
// bytes. Like /cpage/ it needs the server secret (the codes are HMACs) and refuses
// with 503 without one, so it never serves a guessable page. The depth spec — the
// percentages, offsets, codes and marker prefix — lives in internal/probe so the
// origin, the runner and the analysis cannot drift; this file only lays the markers
// into the page.
//
// Two invariants carry the experiment:
//   - Size in the PATH (/depthpage/<id>/<bytes>), never a query, per the standing
//     gotcha: a stripped query must not change the page.
//   - Markers + filler are a pure function of (secret, id, bytes), so the ETag over
//     the body is stable and revalidation works — exactly like /page/ and /sizepage/.

const depthPagePrefix = "/depthpage/"

// maxDepthPageBytes caps what the origin will build on request. The ladder tops out
// at 4 MiB; 16 MiB is headroom for a higher second pass and still bounded.
const maxDepthPageBytes = 16 << 20

// depthMarkerBlock is the exact text written at a depth's offset: the prefix, the
// unforgeable code, then a human depth label and the byte offset (for debugging and
// the DepthMarkersMatchThePage guard). The code is what the analysis matches on.
func depthMarkerBlock(secret, id string, pct, off int) string {
	return depthMarkerBlockRev(secret, id, pct, off, probe.DepthPageRevision)
}

// depthMarkerBlockRev is depthMarkerBlock with the code made at an explicit
// revision. Every depth route uses probe.DepthPageRevision; only the changeable
// page (verpage.go) passes its current version, so a flip changes the codes and
// nothing else. The code is always 12 characters, so the block's length — and with
// it every offset on the page — is the same at every revision.
func depthMarkerBlockRev(secret, id string, pct, off, revision int) string {
	code := probe.DepthCode(secret, id, pct, revision)
	// Starts with prefix+code (so it sits exactly at the offset and the guard can
	// find it) then reads as a sentence, so it survives readability extraction.
	return fmt.Sprintf("%s%s is the reference marker for the section at depth=%d%%, byte offset=%d. ",
		probe.DepthMarkerPrefix, code, pct, off)
}

// depthPageFits reports whether a page of nbytes can hold every marker at its offset
// without overlap and without running past the end. Filler is otherwise cheap, so
// this only fails on genuinely tiny pages.
func depthPageFits(secret, id string, nbytes int) bool {
	prevEnd := 0
	for _, pct := range probe.DepthPercents {
		off := probe.DepthOffset(pct, nbytes)
		if off < prevEnd {
			return false // would overlap the previous marker
		}
		block := depthMarkerBlock(secret, id, pct, off)
		if off+len(block) > nbytes {
			return false // runs past the end
		}
		prevEnd = off + len(block)
	}
	return true
}

// renderDepthPage builds a page of EXACTLY nbytes: deterministic filler with each
// depth's marker written in at its byte offset. Assumes depthPageFits(secret, id,
// nbytes); callers check first.
//
// Deprecated: force-replaces markers onto a continuous byte stream, so a marker can
// land mid-word and read oddly. Prefer the line-based generator (depthFormatLines in
// depthformats.go). Kept unchanged for Result-16 / retention comparability.
func renderDepthPage(secret, id string, nbytes int) string {
	buf := make([]byte, nbytes)
	// Fill with the frozen findings-prose corpus, cycled to length, so the page
	// reads as real content. The corpus never contains the marker prefix (enforced
	// by a test), so the filler can never spell a marker.
	src := depthCorpus
	for i := 0; i < nbytes; i++ {
		buf[i] = src[i%len(src)]
	}
	// Overwrite the corpus with each depth's marker at its byte offset.
	for _, pct := range probe.DepthPercents {
		off := probe.DepthOffset(pct, nbytes)
		copy(buf[off:], depthMarkerBlock(secret, id, pct, off))
	}
	return string(buf)
}
