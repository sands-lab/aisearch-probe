package probe

// The truncation-depth probe's shared spec: the marker depths, their byte offsets,
// their unforgeable codes, and how to read the deepest one back. It lives here, in
// the single source of truth, because THREE parties must agree byte-for-byte or the
// measurement is wrong: the origin that PRINTS the markers (proxy/depthpage.go), the
// runner that ASKS for them (trial/truncrun.go), and the analysis that READS them.
// If any two drifted, a returned code would map to the wrong depth and the cut would
// be misread. See docs/experiments/readDepth.md.
//
// A marker is DepthMarkerPrefix + DepthCode(...). The code is HMAC(secret, id|dP|vN)
// — the same construction as the coded pages (CpageCode), keyed on the DEPTH via the
// field slot. It is unforgeable ON PURPOSE: with a guessable pattern a model that had
// read the shallow markers could INFER a deeper code and quote it without reading
// that far, making the cut look deeper than it is. The deepest code a model can
// actually quote is the deepest it truly read.

import "math"

// DepthPercents are the marker depths down the page, as percentages of the total
// size. 0% sits at byte 0 so Part B can warm on the top marker; 95% is the bottom.
var DepthPercents = []int{0, 25, 50, 75, 95}

// DepthPageRevision versions the codes, like the coded pages: a later content
// change (v2) yields different codes without changing the route shape.
const DepthPageRevision = 1

// DepthMarkerPrefix is the fixed signpost before every code. It is HEX-FREE on
// purpose: the page filler is hex (or 'x'), so filler can never spell the prefix and
// a search for a marker is unambiguous.
const DepthMarkerPrefix = "TRUNC-"

// DepthMarkerLabel is the words the depth page prints right after each code
// ("TRUNC-<code> is <label>, byte offset=…"), so a question can name one code the
// way the page does. proxy/depthpage.go writes the same words; a proxy test pins
// the pairing against the served bytes.
func DepthMarkerLabel(pct int) string {
	return "the reference marker for the section at depth=" + itoaDepth(pct) + "%"
}

// DepthField is the code's field slot for a depth, e.g. "d25".
func DepthField(pct int) string {
	return "d" + itoaDepth(pct)
}

// DepthCode is the unforgeable code for one depth of one page. Empty secret returns
// "" so the route can refuse rather than emit a guessable page, unless a page-code
// table is loaded (LoadCodeTable): then the code is looked up there.
func DepthCode(secret, id string, pct, revision int) string {
	if secret == "" && codeTable != nil {
		return codeTable[codeKey(id, pct, revision)]
	}
	return CpageCode(secret, id, DepthField(pct), revision)
}

// DepthOffset is the byte offset of a depth on a page of nbytes: 0% -> 0, 100% ->
// nbytes, rounded in between. The offsets are strictly increasing over DepthPercents
// so markers never share a spot.
func DepthOffset(pct, nbytes int) int {
	return int(math.Round(float64(pct) / 100 * float64(nbytes)))
}

// DeepestDepth is the deepest marker whose code appears in an answer — the cut for
// this fetch, in percent. It returns -1 when no code appears (a distinct finding:
// the model quoted none, NOT depth 0), and it only counts codes THIS page carried,
// so a forged or foreign code cannot inflate the reading.
func DeepestDepth(answer, secret, id string, revision int) int {
	deepest := -1
	for _, pct := range DepthPercents {
		code := DepthCode(secret, id, pct, revision)
		if code != "" && containsFoldDepth(answer, code) && pct > deepest {
			deepest = pct
		}
	}
	return deepest
}

// itoaDepth is a tiny int->string without pulling strconv into this file's surface.
func itoaDepth(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// containsFoldDepth is a case-insensitive substring test. HMAC codes are lowercase
// hex, but a model may echo them upper-cased, so fold the case.
func containsFoldDepth(haystack, needle string) bool {
	return indexFoldDepth(haystack, needle) >= 0
}

func indexFoldDepth(s, sub string) int {
	if sub == "" {
		return 0
	}
	lower := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + ('a' - 'A')
		}
		return b
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		ok := true
		for j := 0; j < len(sub); j++ {
			if lower(s[i+j]) != lower(sub[j]) {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}
