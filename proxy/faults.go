package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A DELIBERATELY MISBEHAVING CORNER OF THE ORIGIN, for measuring how a fetcher
// reacts to failure.
//
// The caching experiments ask what a provider does when a fetch SUCCEEDS. This
// asks what it does when a fetch FAILS: does it retry, how many times, how fast,
// does it honour the back-off the server asks for, does it wander to other
// addresses, and does it retry things it should leave alone. The answer is read
// entirely from our own request log — the faults simply produce the failures to
// react to.
//
// It exists because of a behaviour measured on 2026-09-14: Gemini's url_context
// fetched our page (HTTP 200, full body) and then fetched it again, and again,
// and fetched the site root we never named — three fetches in three seconds for
// one question. No retry standard covers that, because from the server's side
// nothing failed. That has the shape of a reflection-and-amplification vector: a
// cheap API call turns into several fetches aimed at a target the caller chose,
// arriving from the provider's addresses rather than the caller's. Whether the
// other providers do the same is measurable, and this is what measures it.
//
// WHY THE FAULT IS IN THE PATH, NOT THE QUERY. httpbin and similar tools trigger
// faults from a query parameter. We cannot: a provider that strips the query
// (Gemini does — measured) would silently disable a
// query-triggered fault and the server would serve a normal page instead, which
// is the one failure that would fabricate a result. A path trigger survives
// query-stripping. The query is left to carry the ?probe= token for attribution.
//
// WHY THESE FAULTS. Injecting only "500" gives shallow confidence; real
// dependencies fail at the transport, protocol, timing and body layers. This set
// spans all four, and each fault is labelled by whether a well-behaved client
// SHOULD retry it — which turns the result from "what it did" into "whether that
// was correct", the same shape as the address-variation result.
//
// References for the method are in docs/experiments/fault.md, for the write-up.
//
//	path                should a good client retry?   the standard it tests against
//	-----------------   ---------------------------   ------------------------------
//	/fault/404          NO  — the page is gone         retrying a permanent error is waste
//	/fault/500          yes, then give up              5xx is transient; back off, don't hammer
//	/fault/503          yes, after the asked delay     honour Retry-After (RFC 9110 §10.2.3)
//	/fault/slow         (timing, not an error)         a slow 200 is still a success
//	/fault/empty        NO  — an empty 200 is valid    a 200 is a success whatever its length
//	/fault/reset        yes — transport failure        a dropped connection is retryable
//	/fault/flaky/N      yes — fails N times, succeeds  does it retry ENOUGH to recover
//	/fault/429          yes, after the asked delay     the rate-limit signal; honour Retry-After
//	/fault/429date      yes, after the asked delay     same, Retry-After as an HTTP-date (the
//	                                                   other form RFC 9110 §10.2.3 allows)
//	/fault/403          NO  — refused                  permanent; and does it come back under
//	                                                   another identity (user agent, address)?
//	/fault/loop/a|b     follow a few hops, then stop   a↔b redirect forever; hop count = amplification
//	/fault/cut          yes, or report failure         promises the whole page, sends ~40%, drops;
//	                                                   does the fetcher pass a partial page on as whole?
//
// THE CONTENT-ENDING FAULTS SERVE THE DEPTH PAGE. slow, flaky/N's success,
// heal's healed page and cut all end in real content, and that content is the
// line-based depth page (depthformats.go): HMAC markers at 0/25/50/75/95%, keyed
// on the caller's ?probe= token. So the runner can read HOW MUCH of the page the
// model got — which on /fault/cut is the whole finding. They need the page-code
// secret and refuse without it, rather than serve a page with no markers. Each
// keeps the "study reference RCP-2026-08" line at the top, which the
// negative-caching runner (heal) and the flaky verdict look for.
//
// NO FAULT RESPONSE CARRIES A VALIDATOR OR CACHE-CONTROL. The depth page's own
// route stamps ETag and Last-Modified; the fault corner reuses only its BODY and
// writes the bytes itself, so a fault page can never be cached and leak into a
// caching run. serveFault also strips the three headers up front, so nothing
// set earlier in the chain survives. Cloudflare drops ETag but not Last-Modified,
// so the origin must not rely on the edge for this. A test covers every path.
//
// State: flaky/N, heal/<sec> and loop remember something per caller (keyed on
// the ?probe= token). Everything else is a pure function of the path, so two
// callers never interfere. The state lives in memory and resets when the server
// restarts, which is a feature: every run starts from a clean slate — and the
// reason the origin must never restart mid-run.

const faultPrefix = "/fault/"

// slowFaultDelay is how long /fault/slow holds the connection before answering.
// Long enough to sit past a typical fetch timeout, short enough not to stall a
// run. Exported-ish as a var so a test can shrink it.
var slowFaultDelay = 25 * time.Second

// faultDepthBytes is the size of the depth part of a content-ending fault's
// body. 2048 matches the cache-header test's default page (-cacheHeaders-pageSize), so a
// fault page is the same page the caching runs serve and only the failure
// differs. Small because the faults are about retrieval, not size.
const faultDepthBytes = 2048

// cutFraction is where /fault/cut stops sending: past the 25% marker, short of
// the 50% one, so "deepest quoted = 25" means the model got the cut page.
const cutFraction = 0.40

// cutStallDelay is how long /fault/cut holds the connection open after sending
// the partial body and before dropping it, so the bytes reach the fetcher (and
// any proxy in between) as a delivered prefix rather than racing the close.
var cutStallDelay = 1 * time.Second

// loopHopCap bounds /fault/loop for one caller. Common clients stop well below it
// (browsers ~20, curl 50, Go 10), so it never shapes a normal reading; it only
// stops a client that would follow forever from hammering the origin. Past it the
// loop answers 508 Loop Detected. A var so a test can shrink it.
var loopHopCap = 100

// faultStudyLine is the line every content-ending fault starts with. The
// negative-caching runner and the flaky verdict look for the code in it.
const faultStudyLine = "study reference RCP-2026-08"

// faultDepthPage is the body a content-ending fault serves: its own label, the
// study line, then the line-based depth page keyed on id — all rendered as HTML
// by renderDepthHTML, the same renderer and size as /cacheHeaders/ (the cache-
// header test), so the setup is uniform across experiments.
func faultDepthPage(secret, id, label string) []byte {
	lines := append([]string{label, faultStudyLine}, depthFormatLines(secret, id, faultDepthBytes)...)
	return renderDepthHTML(lines)
}

// faultSecretMissing writes the refusal a content-ending fault gives when the
// server has no page-code secret. Not 503: on flaky or heal a 503 would read as
// "still failing" and fabricate a retry finding. Reports whether it refused.
func (s *staticServer) faultSecretMissing(w http.ResponseWriter) bool {
	if s.pageCodeSecret != "" {
		return false
	}
	http.Error(w, "fault: this fault serves a depth page and needs the page-code secret "+
		"(-pageCodeSecret); refusing rather than serving a page without markers",
		http.StatusInternalServerError)
	return true
}

// faultDepthType is the Content-Type of a content-ending fault's body — the same
// as /cacheHeaders/ sends.
const faultDepthType = "text/html; charset=utf-8"

// writeFaultDepth serves a content-ending fault's 200. Written by hand, never via
// ServeContent or the genPage spine, so no validator is added.
func writeFaultDepth(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", faultDepthType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// flakyState counts attempts per caller for /fault/flaky/N. The key is the
// ?probe= token, so each provider is counted on its own; a caller that strips
// the token (Gemini) collapses into one bucket, which is noted at the call site.
type flakyState struct {
	mu       sync.Mutex
	attempts map[string]int
}

func newFlakyState() *flakyState {
	return &flakyState{attempts: map[string]int{}}
}

// healState records when each caller first saw a /fault/heal/<sec> URL, so the
// page can be "broken for the first <sec> seconds, then fixed" — a TIME-based
// recovery model, not an attempt-based one.
//
// WHY TIME-BASED. The dominant transient failures on the web recover on a clock,
// not on how many times you knock: a busy or rate-limited server returns 503/429
// with a Retry-After telling you a delay (RFC 9110 §10.2.3), and the circuit-
// breaker resilience pattern (Nygard, "Release It!") reopens after a cooldown
// timer, not a request count. Attempt-based recovery (see flaky/N) models a
// different, narrower cause — cold start / autoscaling, where the requests
// themselves trigger the warm-up. Both are real; this one is the mainstream, and
// it fails uniformly THROUGH a fetcher's retry burst (which finishes in seconds)
// so the warm always sees the failure, whatever the provider's retry policy.
//
// This is what the negative-caching experiment needs: the page fails during the
// warm and heals before the re-ask, so a provider that re-fetches gets the healed
// page (marker present) while one that serves the remembered failure does not.
type healState struct {
	mu        sync.Mutex
	firstSeen map[string]time.Time
}

func newHealState() *healState {
	return &healState{firstSeen: map[string]time.Time{}}
}

// remaining returns how many whole seconds the URL is still "broken" for this
// caller, recording the first sighting on first call. <= 0 means healed.
func (h *healState) remaining(key string, window time.Duration) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	first, ok := h.firstSeen[key]
	if !ok {
		h.firstSeen[key] = time.Now()
		return window
	}
	return window - time.Since(first)
}

// nextAttempt returns how many times this key has now been seen, starting at 1.
func (f *flakyState) nextAttempt(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[key]++
	return f.attempts[key]
}

// isFaultPath reports whether a request is aimed at the fault corner.
func isFaultPath(p string) bool {
	return strings.HasPrefix(p, faultPrefix)
}

// serveFault produces the misbehaviour named by the path. It reports whether it
// handled the request; ServeHTTP falls through to the normal file server when it
// did not, so an unknown /fault/… path is an ordinary 404 rather than a silent
// success that would read as a page that exists.
func (s *staticServer) serveFault(w http.ResponseWriter, r *http.Request) bool {
	kind := strings.TrimPrefix(r.URL.Path, faultPrefix)

	// None of these carry an ETag or Last-Modified. They are about retrieval,
	// not caching, and stamping them would let a fault path pollute a caching
	// experiment if one were ever reused. Strip all three up front so nothing
	// set earlier in the chain can survive onto a fault response.
	for _, h := range []string{"ETag", "Last-Modified", "Cache-Control"} {
		w.Header().Del(h)
	}
	switch {
	case kind == "404":
		http.Error(w, "fault: not found", http.StatusNotFound)
		return true

	case kind == "500":
		http.Error(w, "fault: internal server error", http.StatusInternalServerError)
		return true

	case kind == "503":
		// The one back-off signal the HTTP standard defines. A well-behaved
		// client waits this long; measuring whether the fetcher does is the
		// point of this path.
		w.Header().Set("Retry-After", "10")
		http.Error(w, "fault: service unavailable", http.StatusServiceUnavailable)
		return true

	case kind == "429":
		// The rate-limit signal, with the same seconds-form delay as 503 so the
		// two rows compare directly.
		w.Header().Set("Retry-After", "10")
		http.Error(w, "fault: too many requests", http.StatusTooManyRequests)
		return true

	case kind == "429date":
		// Retry-After's other legal form: an HTTP-date ~10 s ahead. Tests whether
		// a fetcher parses the date form at all, or only the seconds form.
		w.Header().Set("Retry-After", time.Now().Add(10*time.Second).UTC().Format(http.TimeFormat))
		http.Error(w, "fault: too many requests", http.StatusTooManyRequests)
		return true

	case kind == "403":
		// The common real "go away", the one bot blockers send. Permanent: a good
		// client stops. The runner also checks whether the fetcher came back
		// under a different user agent.
		http.Error(w, "fault: forbidden", http.StatusForbidden)
		return true

	case kind == "loop/a" || kind == "loop/b":
		// a → b → a … forever. Keeps the query, so every hop carries the
		// caller's ?probe= token and is counted. Bounded by loopHopCap.
		if s.loops.nextAttempt("loop|"+faultToken(r)) > loopHopCap {
			http.Error(w, "fault: redirect loop stopped by the server's hop cap", http.StatusLoopDetected)
			return true
		}
		next := faultPrefix + "loop/b"
		if kind == "loop/b" {
			next = faultPrefix + "loop/a"
		}
		if r.URL.RawQuery != "" {
			next += "?" + r.URL.RawQuery
		}
		w.Header().Set("Location", next)
		w.WriteHeader(http.StatusFound)
		return true

	case kind == "cut":
		// Promise the whole depth page in Content-Length, send the part before
		// the cut, pause, then drop the connection. A careful fetcher sees fewer
		// bytes than promised and treats it as a failure; a careless one passes
		// the prefix on as the page. The depth markers in the answer tell which.
		// Like reset, the hijack means THIS hit may log without a status.
		// Whether cloudflared preserves the cut is a live check, not a given:
		// see docs/experiments/fault.md.
		if s.faultSecretMissing(w) {
			return true
		}
		body := faultDepthPage(s.pageCodeSecret, faultToken(r), "fault: cut")
		cut := int(float64(len(body)) * cutFraction)
		w.Header().Set("Content-Type", faultDepthType)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body[:cut])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-time.After(cutStallDelay):
		case <-r.Context().Done():
			return true
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			return true // cannot drop; the handler returning short is the fallback
		}
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
		return true

	case kind == "slow":
		if s.faultSecretMissing(w) {
			return true
		}
		select {
		case <-time.After(slowFaultDelay):
		case <-r.Context().Done():
			// The caller hung up before we answered — itself an observation, and
			// nothing more to write.
			return true
		}
		writeFaultDepth(w, faultDepthPage(s.pageCodeSecret, faultToken(r),
			fmt.Sprintf("fault: slow response after %s", slowFaultDelay)))
		return true

	case kind == "empty":
		// A valid 200 with no body. A fetcher should treat this as success, not
		// as a failure to retry.
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return true

	case kind == "reset":
		// Drop the connection mid-exchange, the way a crashing server does.
		// Hijack takes the socket away from the recorder, so THIS hit may log
		// with status 0 and no byte count — the arrival is still logged, which
		// is what a retry count needs. Documented so the missing status is not
		// read as a bug.
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "fault: cannot hijack to reset", http.StatusInternalServerError)
			return true
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close() // no response at all: the client sees a reset
		}
		return true

	case strings.HasPrefix(kind, "heal/"):
		// Broken (503, transient) for the first <sec> after this caller first
		// sees the URL, then healed (200 + marker). Time-based recovery — the
		// negative-caching probe: the warm sees the failure, and by the re-ask
		// (after a gap longer than <sec>) the page works, so a re-fetch gets the
		// marker while a remembered failure does not. See healState.
		sec, err := strconv.Atoi(strings.TrimPrefix(kind, "heal/"))
		if err != nil || sec < 0 {
			http.Error(w, "fault: heal needs a non-negative seconds value, e.g. /fault/heal/60", http.StatusBadRequest)
			return true
		}
		key := kind + "|" + faultToken(r)
		rem := s.heal.remaining(key, time.Duration(sec)*time.Second)
		if rem > 0 {
			// Still broken. Tell the client exactly how long to wait, per
			// RFC 9110 §10.2.3 (Retry-After as a delay in seconds) — the same
			// signal a real busy server sends, rounded up to whole seconds.
			wait := int(rem.Seconds())
			if wait < 1 {
				wait = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(wait))
			http.Error(w, "fault: temporarily unavailable (healing)", http.StatusServiceUnavailable)
			return true
		}
		if s.faultSecretMissing(w) {
			return true
		}
		writeFaultDepth(w, faultDepthPage(s.pageCodeSecret, faultToken(r),
			"fault: healed and now serving normally"))
		return true

	case strings.HasPrefix(kind, "flaky/"):
		n, err := strconv.Atoi(strings.TrimPrefix(kind, "flaky/"))
		if err != nil || n < 0 {
			http.Error(w, "fault: flaky needs a non-negative count, e.g. /fault/flaky/2", http.StatusBadRequest)
			return true
		}
		// Count this caller's attempts. Keyed by the probe token so providers do
		// not share a counter; a token-less caller (one that stripped it) shares
		// the "untokenised" bucket, which is the right failure mode — visible,
		// not silent.
		key := kind + "|" + faultToken(r)
		attempt := s.flaky.nextAttempt(key)
		if attempt <= n {
			http.Error(w,
				fmt.Sprintf("fault: flaky attempt %d of %d — failing on purpose", attempt, n),
				http.StatusServiceUnavailable)
			return true
		}
		if s.faultSecretMissing(w) {
			return true
		}
		writeFaultDepth(w, faultDepthPage(s.pageCodeSecret, faultToken(r),
			fmt.Sprintf("fault: flaky recovered on attempt %d (after %d failures)", attempt, n)))
		return true
	}

	// Unknown fault name: not handled, so the caller 404s it like any missing
	// page rather than treating it as a success.
	return false
}

// faultToken pulls the ?probe= value out of the request, or "untokenised" when
// there is none — so a stripped token is a named bucket, never a missing count.
func faultToken(r *http.Request) string {
	if v := r.URL.Query().Get("probe"); v != "" {
		return v
	}
	return "untokenised"
}
