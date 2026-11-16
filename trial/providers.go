package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"aisearch-probe/internal/probe"
)

// Each provider is asked to fetch a URL and answer a question about it. All
// three are called over plain HTTP rather than through vendor SDKs: this repo
// ships as a single dependency-free binary, and pulling in one vendor's SDK
// while hand-rolling the other two would trade that away for nothing.

type Caller struct {
	Name    string
	EnvKey  string
	Model   string
	Ask     func(ctx context.Context, key, model, url, question string, force bool) (string, probe.Usage, error)
	Enabled bool
	// IsFile marks providers whose env var holds a credentials file path
	// rather than a secret (Google), so its existence must be checked.
	IsFile bool
}

// Tag is this provider's identity for ADDRESSING — the vendor route and the
// model together. Name stays the vendor route, because that is what decides
// which fetcher should arrive (ExpectedFetcher is keyed by it).
//
// They differ because one vendor can be measured at two models, and a vendor
// stores one copy per address, not one per model. Ask both models about the
// same address and the second is served the first one's download, so it never
// fetches — which reads as "the second model caches" when it may cache nothing.
// A distinct address per model removes that. See probeurl.go.
func (p Caller) Tag() string {
	m := p.Model
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}
	if m == "" {
		return p.Name
	}
	return p.Name + "-" + m
}

// blindNoAddress and blindNoTools are the two knobs the content-reuse blind step
// sets, both OFF by default so every other run is byte-for-byte unchanged. They
// are globals mutated around a single call and reset straight after, the same
// idiom openRouterUser uses; the driver is sequential, so there is no race.
//
//   - blindNoAddress: buildPrompt sends the question ALONE — no address, no
//     "Fetch …", no footer. This is the whole point of the content-reuse test:
//     the model is named a SUBJECT, never a URL, so a correct answer can only
//     have come from a page it already holds. Reintroducing the address here
//     would turn the finding back into the address-keyed reuse we already know.
//   - blindNoTools: every Ask omits its web tool, so the model answers from its
//     own memory / internal store rather than being sent to the public web
//     (where our page is unindexed and cannot be found anyway).
var (
	blindNoAddress bool
	blindNoTools   bool
)

// withBlind runs fn with the blind-step call shape and restores the defaults
// afterwards, so the knobs can never leak into a later call.
func withBlind(noTools bool, fn func()) {
	blindNoAddress, blindNoTools = true, noTools
	defer func() { blindNoAddress, blindNoTools = false, false }()
	fn()
}

// buildPrompt is the whole prompt every API runner sends: the address on its own
// line, a blank line, then the question, and nothing else (the standard-question rule). Any
// added wording is a deliberate change; TestPromptIsAddressAndQuestion
// pins the bytes.
//
// Until 2026-10-28 it also wrapped the question ("Fetch <url> and answer from that
// page only. Question: …") and closed with "quote the study reference code printed
// at the bottom of that page". That closing line asked depth pages for a code they
// do not carry. Runs before that date were sent the old wrapper.
func buildPrompt(url, question string) string {
	// The blind content-reuse step names a subject, never an address: send the
	// question verbatim (it already carries everything, including the way out for
	// an honest "I don't have it").
	if blindNoAddress {
		return question
	}
	return url + "\n\n" + question
}

func httpPost(ctx context.Context, endpoint string, headers map[string]string, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// 10 min, not 5: web_fetch dynamic filtering runs code execution server-side,
	// which on a big/slow draw can take several minutes before the first header —
	// a 5 min deadline silently dropped one such call (csv 16k) mid-work.
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		// Surface the provider's own error text: model ids and tool names move,
		// and the body says exactly which one was rejected.
		return nil, fmt.Errorf("%s returned %d: %s", endpoint, resp.StatusCode, truncate(string(out), 600))
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// Anthropic — Messages API with the web_fetch server tool.
//
// web_fetch only retrieves URLs already present in the conversation, so the URL
// goes in the user message. It is a SERVER-side tool: Anthropic runs the fetch on
// its own servers inside the turn, and when that turn runs out of its per-request
// step budget it comes back with stop_reason "pause_turn" and no final text yet.
// The API is stateless, so Continue resumes by appending the partial assistant
// turn (which carries the web_fetch_tool_result verbatim), and the shared driver
// POSTs again until "end_turn". That resend is intra-call plumbing, NOT a cache
// rung: the content is reused only because we resend it.
// ---------------------------------------------------------------------------

type anthropicVendor struct{}

func (anthropicVendor) Endpoint(string) string { return "https://api.anthropic.com/v1/messages" }

func (anthropicVendor) Auth(_ context.Context, key string) (map[string]string, error) {
	return map[string]string{
		"x-api-key":         key,
		"anthropic-version": "2023-06-01",
	}, nil
}

// anthropicUseCacheOverride drives the real-page cache on/off PROOF. nil = default
// behaviour (web_fetch_20260209, no use_cache field — the main study is untouched).
// Non-nil switches the tool to web_fetch_20260309 (the first version that accepts
// use_cache; verified 2026-10-03) and sets use_cache explicitly. It is a package global
// toggled around a single call via withAnthropicCache, exactly like blindNoTools; the
// driver is sequential so there is no race. A later cleanup could fold both into a proper per-call option on the Vendor layer.
var anthropicUseCacheOverride *bool

func withAnthropicCache(on bool, fn func()) {
	anthropicUseCacheOverride = &on
	defer func() { anthropicUseCacheOverride = nil }()
	fn()
}

// anthropicFetchVersionOverride pins the web_fetch tool VERSION for the dynamic-filtering
// cost investigation (handoff: the PDF "bomb" hunt). nil = default (web_fetch_20260209,
// which HAS dynamic filtering). Set it to an older version (web_fetch_20250910 = NO dynamic
// filtering) to run the A/B that isolates dynamic filtering as the cost driver — same key,
// same PDF, only the version differs. Same global-around-a-call pattern as withAnthropicCache;
// the -anthropicFetchTool flag sets it for a whole run. Do NOT combine with
// withAnthropicCache (that path needs 20260309). TODO: fold into a per-call Vendor option.
var anthropicFetchVersionOverride *string

func withAnthropicFetchVersion(ver string, fn func()) {
	anthropicFetchVersionOverride = &ver
	defer func() { anthropicFetchVersionOverride = nil }()
	fn()
}

func (anthropicVendor) BuildRequest(model, pageURL, question string, force bool) (any, error) {
	body := map[string]any{
		"model":      model,
		"max_tokens": 8192,
		"messages": []any{
			map[string]any{"role": "user", "content": buildPrompt(pageURL, question)},
		},
	}
	if !blindNoTools {
		tool := map[string]any{"type": "web_fetch_20260209", "name": "web_fetch", "max_uses": 3}
		if anthropicUseCacheOverride != nil {
			tool["type"] = "web_fetch_20260309" // only this version accepts use_cache
			tool["use_cache"] = *anthropicUseCacheOverride
		}
		// Version override (dynamic-filtering A/B) has the final say on the tool version.
		if anthropicFetchVersionOverride != nil {
			tool["type"] = *anthropicFetchVersionOverride
		}
		body["tools"] = []map[string]any{tool}
		if force {
			// Without this the model may answer from memory and never fetch,
			// which produces a "no request arrived" result that means nothing.
			body["tool_choice"] = map[string]any{"type": "any"}
		}
	}
	return body, nil
}

// anthropicReply is the part of a Messages response the vendor reads. Content is
// kept raw so Continue can resend it verbatim to resume a paused turn.
type anthropicReply struct {
	Content    json.RawMessage `json:"content"`
	StopReason string          `json:"stop_reason"`
}

func (anthropicVendor) ParseResponse(raw []byte) (string, probe.Usage, error) {
	usage, _ := probe.UsageResponses(raw)
	var parsed anthropicReply
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", usage, fmt.Errorf("decoding response: %w", err)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(parsed.Content, &blocks)
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	if parsed.StopReason == "refusal" {
		return sb.String(), usage, fmt.Errorf("model refused the request")
	}
	return sb.String(), usage, nil
}

func (anthropicVendor) Continue(prevBody any, raw []byte) (any, error) {
	var parsed anthropicReply
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	if parsed.StopReason != "pause_turn" {
		return nil, nil
	}
	// Resume by appending the partial assistant turn as a new message, on a COPY
	// of the previous body so each round's body stays a distinct record.
	prev, _ := prevBody.(map[string]any)
	next := make(map[string]any, len(prev))
	for k, v := range prev {
		next[k] = v
	}
	oldMsgs, _ := prev["messages"].([]any)
	newMsgs := make([]any, len(oldMsgs), len(oldMsgs)+1)
	copy(newMsgs, oldMsgs)
	newMsgs = append(newMsgs, map[string]any{"role": "assistant", "content": parsed.Content})
	next["messages"] = newMsgs
	return next, nil
}

// ---------------------------------------------------------------------------
// OpenAI — Responses API with the web_search tool.
// ---------------------------------------------------------------------------

type openaiVendor struct{}

func (openaiVendor) Endpoint(string) string { return "https://api.openai.com/v1/responses" }

func (openaiVendor) Auth(_ context.Context, key string) (map[string]string, error) {
	return map[string]string{"Authorization": "Bearer " + key}, nil
}

func (openaiVendor) BuildRequest(model, pageURL, question string, force bool) (any, error) {
	body := map[string]any{
		"model": model,
		"input": buildPrompt(pageURL, question),
	}
	if !blindNoTools {
		body["tools"] = []map[string]any{{"type": "web_search"}}
		if force {
			// web_search is the only tool declared, so requiring a tool call
			// requires a search. Without it the model can answer from memory and
			// never touch the network, which is not the thing being measured.
			body["tool_choice"] = "required"
		}
	}
	return body, nil
}

func (openaiVendor) ParseResponse(raw []byte) (string, probe.Usage, error) {
	usage, _ := probe.UsageResponses(raw)
	var parsed struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", usage, fmt.Errorf("decoding response: %w", err)
	}
	if parsed.OutputText != "" {
		return parsed.OutputText, usage, nil
	}
	var sb strings.Builder
	for _, o := range parsed.Output {
		for _, c := range o.Content {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), usage, nil
}

// OpenAI never pauses: one request, one answer.
func (openaiVendor) Continue(any, []byte) (any, error) { return nil, nil }

// ---------------------------------------------------------------------------

// callers returns the set to run: those whose API key is present in the
// environment. A missing key skips that provider rather than failing the run.
func callers(anthropicModel, openaiModel, googleModel, xaiModel string, openRouterModels []string) []Caller {
	list := []Caller{
		{Name: "openai", EnvKey: "OPENAI_API_KEY", Model: openaiModel, Ask: vendorAsk(openaiVendor{})},
		// Two Google routes. The Developer API key is the primary one; Vertex is
		// a fallback for when no key is available.
		//   google        — Gemini Developer API, a plain key, free tier, no admin
		//   google-vertex — Vertex AI, a service-account FILE, needs GCP billing
		// Both use the url_context tool. GEMINI_API_KEY WINS when both are
		// present — see the supersede rule below.
		//
		// google (Developer API) is on the Vendor interface; google-vertex is the
		// OLD path, deliberately left on its own askGoogle (the JWT-bearer exchange
		// and project-in-URL that the Vendor interface no longer carries). It stays
		// as the no-key fallback only — see google.go.
		{Name: "google", EnvKey: "GEMINI_API_KEY", Model: googleModel, Ask: vendorAsk(geminiVendor{})},
		{Name: "google-vertex", EnvKey: "GOOGLE_APPLICATION_CREDENTIALS", Model: googleModel, Ask: askGoogle, IsFile: true},
		{Name: "anthropic", EnvKey: "ANTHROPIC_API_KEY", Model: anthropicModel, Ask: vendorAsk(anthropicVendor{})},
		// Direct xAI account, separate from the OpenRouter x-ai route (openrouter.go).
		// Name is exactly "x-ai" so -sharingApi-firstCaller x-ai resolves; Tag() adds the model
		// so it never shares a stored copy with the OpenRouter route. See xai.go.
		{Name: "x-ai", EnvKey: "XAI_API_KEY", Model: xaiModel, Ask: vendorAsk(xaiVendor{})},
	}
	// OpenRouter contributes one entry per model, so vendors stay separate in
	// the report and are distinguishable from a direct account ("or:openai" vs
	// "openai").
	list = append(list, openRouterCallers(openRouterModels)...)

	for i := range list {
		v := os.Getenv(list[i].EnvKey)
		if v == "" {
			continue
		}
		if list[i].IsFile {
			if _, err := os.Stat(v); err != nil {
				fmt.Fprintf(os.Stderr, "[trial] %s points at %s, which cannot be read: %v\n", list[i].EnvKey, v, err)
				continue
			}
		}
		list[i].Enabled = true
	}

	// Precedence: a Gemini Developer API key supersedes the Vertex
	// service-account route. Both would measure the same vendor, so running
	// both would double-count Google in the report; and Vertex additionally
	// needs GCP billing, which the key does not. Keep Vertex only as the
	// fallback for when no key exists.
	if os.Getenv("GEMINI_API_KEY") != "" {
		for i := range list {
			if list[i].Name == "google-vertex" && list[i].Enabled {
				list[i].Enabled = false
				fmt.Fprintf(os.Stderr,
					"[trial] google-vertex disabled — GEMINI_API_KEY takes precedence "+
						"(same vendor; the key needs no GCP billing)\n")
			}
		}
	}
	return list
}

// googleTunnelWarning is printed, highlighted, whenever a Google route is in a
// run. Google documents that its url_context tool does not support tunneling
// services, localhost or private addresses. On such an address Google's fetcher
// still downloads the page (HTTP 200) while the model reports it blocked, which
// would read as "Google never keeps a copy": a false result. The tool cannot tell
// a tunnel address from a real domain, so it always warns.
const googleTunnelWarning = "WARNING: Google's URL tool does not support localhost, private networks or\n" +
	"         tunneling services (https://aistudio.google.com/docs/url-context). Even on a\n" +
	"         real domain it may refuse a new site for days to weeks. Check Google with\n" +
	"         -checkVendors before trusting its results (see docs/setup-cdn.md)."

// warnGoogle prints googleTunnelWarning in bold yellow when any Google route is
// active. It reports whether it printed.
func warnGoogle(active []Caller) bool {
	for _, p := range active {
		if strings.HasPrefix(p.Name, "google") {
			fmt.Fprintf(os.Stderr, "\033[1;33m[trial] %s\033[0m\n", googleTunnelWarning)
			return true
		}
	}
	return false
}
