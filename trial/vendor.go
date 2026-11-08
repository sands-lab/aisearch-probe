package main

// The vendor abstraction: one small per-vendor contract so every difference
// between vendors sits behind it, and adding a vendor means implementing the
// interface rather than copy-pasting a bespoke ~100-line ask function.
//
// This generalises the pattern usage.go already uses for token counting (one
// UsageParser per response shape) to the WHOLE call. Identity/registration stays
// on Provider (see providers.go): Caller HOLDS a Vendor. Caller.Name is still
// the vendor route ExpectedFetcher keys off, and Tag() is still the address
// identity — those are deliberately NOT on Vendor, because a transport like
// OpenRouter fetches AS the underlying vendor (or:anthropic → Claude-User).
//
// Caller.Ask stays a function field (main.go's dry-run backstop and main_test's
// fake both assign it): for a modern vendor it is just vendorAsk(v), a one-line
// adapter to the shared driver below. The old google-vertex route keeps its own
// legacy Ask and is deliberately left off this interface.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"aisearch-probe/internal/probe"
)

// rawOutPath is where every vendor reply is kept, whole, one JSON line per POST
// (-raw-out; on by default, "" = off). Runners keep only the answer text and the
// token counts; everything else a vendor sends (Anthropic's per-fetch result and
// error_code, OpenRouter's annotations, xAI's tool calls) is in here, for any
// later analysis to take. Append-only, like every log.
var rawOutPath string

var rawOutMu sync.Mutex

// RawReply is one line of the raw-reply file.
type RawReply struct {
	TS       string          `json:"ts"`
	TSUnix   float64         `json:"ts_unix"`
	Endpoint string          `json:"endpoint"` // which vendor (its API address)
	Model    string          `json:"model"`
	URL      string          `json:"url"`   // the page the question named
	Round    int             `json:"round"` // 1, or more when a paused turn resumed
	Err      string          `json:"err,omitempty"`
	Raw      json.RawMessage `json:"raw,omitempty"`      // the reply, as sent, when it is JSON
	RawText  string          `json:"raw_text,omitempty"` // the reply when it is not JSON
}

// keepRaw appends one reply to rawOutPath. A failed write is reported on stderr and
// never stops the run: the answer itself is still recorded by the runner.
func keepRaw(endpoint, model, pageURL string, round int, raw []byte, postErr error) {
	if rawOutPath == "" {
		return
	}
	now := time.Now()
	rec := RawReply{TS: now.UTC().Format(time.RFC3339Nano), TSUnix: unixf(now),
		Endpoint: endpoint, Model: model, URL: pageURL, Round: round}
	if postErr != nil {
		rec.Err = postErr.Error()
	}
	if len(raw) > 0 {
		if json.Valid(raw) {
			rec.Raw = json.RawMessage(raw)
		} else {
			rec.RawText = string(raw)
		}
	}
	line, err := json.Marshal(rec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[raw-out] cannot encode a reply: %v\n", err)
		return
	}
	rawOutMu.Lock()
	defer rawOutMu.Unlock()
	f, err := os.OpenFile(rawOutPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[raw-out] cannot open %s: %v\n", rawOutPath, err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil { // one write per line: lines never interleave
		fmt.Fprintf(os.Stderr, "[raw-out] cannot write %s: %v\n", rawOutPath, err)
	}
}

// Vendor is the per-vendor contract. Everything a caller varies between vendors
// lives behind these five methods.
type Vendor interface {
	// Endpoint is where to POST. It takes the model because Gemini puts the
	// model in the URL (.../models/{model}:generateContent); every other vendor
	// ignores the argument and returns its fixed endpoint.
	Endpoint(model string) string

	// Auth returns the request headers for this secret. It is a function, not a
	// static bearer string, so a vendor that must exchange the secret for a
	// short-lived token could do it here (that is why it takes a context and can
	// error). None of the current vendors need the exchange — Vertex, the one that
	// did, is off this interface.
	Auth(ctx context.Context, secret string) (map[string]string, error)

	// BuildRequest builds the request body: the prompt, the URL-fetch tool
	// declaration, and the force-tool choice. It reads the blind-mode knobs
	// (blindNoTools / blindNoAddress) the same way the old ask* functions did,
	// so the content-reuse step keeps working unchanged.
	BuildRequest(model, pageURL, question string, force bool) (body any, err error)

	// ParseResponse reads ONE response into an answer and its token cost. It also
	// surfaces a response that MEANS failure as an error — a Gemini per-URL
	// retrieval failure, or a vendor error object returned inside a 200 — so a
	// broken route is never scored as "the model chose not to fetch". It returns
	// the usage it parsed even alongside an error, so the driver can still bill it.
	ParseResponse(raw []byte) (answer string, usage probe.Usage, err error)

	// Continue decides whether a paused turn must be resumed. It returns the next
	// body to POST (built from the previous body plus this reply), or nil when the
	// turn is finished. Only Anthropic's server-side web_fetch pauses; every other
	// vendor always returns nil.
	Continue(prevBody any, raw []byte) (nextBody any, err error)
}

// maxPauseContinuations caps how many times a paused turn is resumed, so a server
// tool that never settles cannot loop forever.
const maxPauseContinuations = 5

// poster is the HTTP seam the driver posts through. The real driver passes
// httpPost; tests pass a fake that returns canned replies and records the calls,
// the way the old anthropicPoster seam did.
type poster func(ctx context.Context, endpoint string, headers map[string]string, body any) ([]byte, error)

// askVendor is the ONE shared driver. It logs in once, builds the first request,
// then POSTs in a loop: each round it reads the reply (summing the cost and
// stitching the answer text across rounds — the pause-turn rule), and asks the
// vendor whether to continue. The continuation cap stops an endless pause.
func askVendor(ctx context.Context, v Vendor, secret, model, pageURL, question string, force bool, post poster) (string, probe.Usage, error) {
	headers, err := v.Auth(ctx, secret)
	if err != nil {
		return "", probe.Usage{}, err
	}
	endpoint := v.Endpoint(model)
	body, err := v.BuildRequest(model, pageURL, question, force)
	if err != nil {
		return "", probe.Usage{}, err
	}

	var answer strings.Builder
	var usage probe.Usage // summed across pause_turn continuation rounds
	for i := 0; ; i++ {
		raw, err := post(ctx, endpoint, headers, body)
		keepRaw(endpoint, model, pageURL, i+1, raw, err)
		usage.Rounds = i + 1 // POSTs so far; >1 means the turn paused and resumed
		if err != nil {
			return answer.String(), usage, err
		}
		// Read this round: sum its cost and stitch its text, before deciding
		// whether the response means failure or the turn must continue.
		ans, u, perr := v.ParseResponse(raw)
		usage = usage.Add(u)
		usage.Rounds = i + 1 // Add() summed a zero-Rounds parse; restore the count
		answer.WriteString(ans)
		if perr != nil {
			return answer.String(), usage, perr
		}
		next, cerr := v.Continue(body, raw)
		if cerr != nil {
			return answer.String(), usage, cerr
		}
		if next == nil {
			return answer.String(), usage, nil
		}
		if i >= maxPauseContinuations {
			return answer.String(), usage, fmt.Errorf("gave up after %d pause_turn continuations without an end_turn", maxPauseContinuations)
		}
		body = next
	}
}

// vendorAsk adapts a Vendor to the Caller.Ask function-field signature, posting
// over real HTTP. This is the whole bridge between the interface and the existing
// call convention: every runner still calls p.Ask(...) unchanged.
func vendorAsk(v Vendor) func(ctx context.Context, secret, model, url, question string, force bool) (string, probe.Usage, error) {
	return func(ctx context.Context, secret, model, url, question string, force bool) (string, probe.Usage, error) {
		return askVendor(ctx, v, secret, model, url, question, force, httpPost)
	}
}

// Compile-time check that every vendor satisfies the interface. The concrete
// types and their methods live in each vendor's own file (providers.go, xai.go,
// google.go, openrouter.go).
var (
	_ Vendor = openaiVendor{}
	_ Vendor = anthropicVendor{}
	_ Vendor = xaiVendor{}
	_ Vendor = geminiVendor{}
	_ Vendor = perplexityVendor{}
	_ Vendor = openRouterVendor{}
)
