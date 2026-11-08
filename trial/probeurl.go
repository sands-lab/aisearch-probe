package main

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// runNonce returns a per-run tag to fold into the address token, so a repeat run
// never reuses a previous run's URL.
//
// WHY THIS EXISTS. Experiments that re-run the same battery (faults, and the
// negative-caching test) gave a static per-provider token, so a second run asked
// about the IDENTICAL URLs as the first. Two failures followed, both measured
// 2026-09-14: a caching provider (Anthropic) served the prior run's copy and
// never re-fetched (false silence), and the server's stateful flaky counter,
// keyed on the token, had already advanced past its threshold (the fault
// "recovered" on the first attempt). A fresh token per run removes both, and
// because the flaky counter keys on the token it resets per run automatically
// while still accumulating correctly within a run.
//
// Shape: UTC compact date-time to the second, plus a short random tail. The
// date-time makes a raw probe.jsonl line self-explanatory weeks later (you can
// read when the run happened); the random tail stops two same-second runs
// colliding and keeps the token unguessable — which matters where a forged-UA
// fetcher (xAI, DeepSeek) is attributed only by the token being ours.
func runNonce() string {
	return "run" + time.Now().UTC().Format("20060102-150405") + "-" + randTail(5)
}

func randTail(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz23456789" // no look-alikes
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("x%d", time.Now().UnixNano()%1000000)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// A per-provider query token on the URL we ask about.
//
// Two problems it solves, and one it could create if used carelessly.
//
// SOLVED 1 — cross-provider cache contamination. Two routes to the same vendor
// (say "anthropic" and "or:anthropic") share that vendor's cache, so the second
// legitimately does not fetch. Without distinct URLs that reads as a cache hit
// caused by the provider, when it was caused by us.
//
// SOLVED 2 — attribution when a fetcher lies about who it is. xAI's fetches
// arrive with a forged Chrome user-agent from a pool of residential addresses:
// measured 2026-08-31, seven requests in three seconds from seven addresses in
// four countries. Nothing in the request identifies xAI. The token does: it was
// in a URL given to xAI and to nobody else, and no scanner can guess it.
//
// THE RISK — a fetcher that normalises the URL and drops the query would arrive
// without its token. If a missing token were treated as "no fetch", that would
// turn our own measurement gap into a fabricated caching result, which is the
// one failure this whole instrument exists to avoid. So the correlation reports
// untokenised arrivals separately and calls them unattributable, never absent;
// see Correlation.OtherHits.
//
// page_key strips the query in both collection modes, so the token does not
// fragment a page into many pages in the analysis.
//
// THE MODEL IS PART OF THE IDENTITY, not just the vendor. Two models at one
// vendor share that vendor's store, so asking both about the same address means
// the second one is answered from the first one's download and never fetches —
// which reads as "this model caches" when the model may cache nothing at all.
// Pass Caller.Tag(), never Caller.Name, for anything that measures one
// caller in isolation. The exception is a sharing experiment, where the
// collision IS the measurement; those pass the bare address on purpose.
func probeToken(identity string) string {
	return "probe=" + sanitizeTokenField(identity)
}

// sanitizeTokenField reduces any identity to one tidy shape — lowercase, only
// letters, digits and single hyphens, no hyphen at either end — so every
// producer emits the same kind of code no matter what messy string it was
// handed (a route colon, a dotted model version, mixed case, a stray trailing
// punctuation mark). The code is an OPAQUE HANDLE: its meaning — vendor, model,
// run, step — is recorded in the standard result record's own fields
// (internal/probe/result.go), never read back out of the code. So the shape is
// free to be tidy without losing anything. See DECISIONS.md § probe-code-shape.
func sanitizeTokenField(identity string) string {
	var b strings.Builder
	lastDash := true // start "on a dash" so any leading run is trimmed
	for _, r := range strings.ToLower(identity) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// probeURLFor returns the URL to ask this provider about, plus the token it
// carries. identity is normally Caller.Tag().
func probeURLFor(base, identity string) (url, token string) {
	token = probeToken(identity)
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + token, token
}
