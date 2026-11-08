package probe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// Per-page UNFORGEABLE codes, so a code in an answer proves the model actually
// read THAT page. This file is the single source of truth for both
// the origin (which prints the codes) and the driver (which recomputes them to
// check an answer, and reads the subject register to pick a unique subject).
//
// A code is HMAC(secret, id|field|vN). The secret lives only on the server, so the
// code cannot be produced from the id in the URL — only by fetching the page.
// Keying on the field gives two independent codes per page (one quoted in the
// warm, one never asked); keying on the version lets a mutated page (v2) carry a
// different code, so "stale copy vs re-fetched" is decidable.

// Field labels for the two codes. Ref is quoted during the warm; Rec is the
// never-asked code the blind content-lookup test goes after.
const (
	CpageFieldRef = "ref"
	CpageFieldRec = "rec"
)

// FormatPageRevision is the code revision the object-type format routes
// (/htmlpage/, /jsonpage/, /pdfpage/, …) key their per-page HMAC code on. It is a
// shared constant so the origin that prints the code and the driver that recomputes
// it to check an answer cannot drift. Bump it only if a format body's shape changes
// in a way that should invalidate stored copies.
const FormatPageRevision = 1

// cpageSubjects is the register of subject handles a coded page can be "about".
// These are REAL terms from this study's own findings, not invented phrases: the
// project prefers promoting its own work to making up names. A real handle is
// still a fair content cue, because the per-page CODE (published nowhere) is what
// actually proves a fetch — a handle a model already knows can never let it produce
// the code without having the page.
//
// Uniqueness still matters, but only for reading a NEGATIVE, so the driver vets
// each handle by search before any paid call. A handle that also appears in our
// public repo merely softens a negative (a model could recognise the term); it can
// never manufacture a positive. Each reads as a named thing (a space, not a stub),
// as TestCpageSubjectRegisterIsUniqueAndPlausible enforces.
var cpageSubjects = []string{
	"the ReturnUnstamped visit bucket",
	"the FirstSight visit category",
	"the Judgeable revisit test",
	"the wasted-bytes headline figure",
	"the Googlebot positive control",
	"the single untouched copy lifetime",
	"the cross-model shared copy finding",
	"the two-user one-account sharing result",
	"the negative-caching own-timer result",
	"the CDN bypass cache witness",
	"the robots.txt cache lifetime measurement",
	"the conditional-request revalidation metric",
}

// CpageSubjects returns a copy of the register, so a driver can read it to pick a
// unique subject and verify uniqueness up front without mutating it.
func CpageSubjects() []string {
	out := make([]string, len(cpageSubjects))
	copy(out, cpageSubjects)
	return out
}

// CpageSubjectForID resolves the subject an id points at. The id carries the
// register index as cr<NN>-<nonce>, so the driver picks the phrase and the server
// serves that exact one. A malformed or out-of-range id does not resolve.
func CpageSubjectForID(id string) (string, bool) {
	idx, ok := parseCpageIndex(id)
	if !ok || idx < 0 || idx >= len(cpageSubjects) {
		return "", false
	}
	return cpageSubjects[idx], true
}

// parseCpageIndex reads the register index from an id of the form cr<digits>-...
func parseCpageIndex(id string) (int, bool) {
	if !strings.HasPrefix(id, "cr") {
		return 0, false
	}
	rest := id[2:]
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(rest) || rest[i] != '-' {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:i])
	if err != nil {
		return 0, false
	}
	return n, true
}

// CpageCode derives one unforgeable code from the server secret, the page id, a
// field, and a revision. Empty secret returns "" so the route can refuse rather
// than emit a guessable code.
func CpageCode(secret, id, field string, revision int) string {
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(id + "|" + field + "|v" + strconv.Itoa(revision)))
	return hex.EncodeToString(mac.Sum(nil))[:12]
}
