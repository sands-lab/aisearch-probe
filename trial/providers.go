package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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
// line, a blank line, then the question, and nothing else (DECISIONS.md D14). Any
// added wording needs the user's approval first; TestPromptIsAddressAndQuestion
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
// rung: the content is reused only because we resend it. See
// doc/cache-scope-framework.md § pause_turn.
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
// driver is sequential so there is no race. TODO (same cleanup as withBlind, see
// TODO.md § Instrument): fold both into a proper per-call option on the Vendor layer.
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
// the -anthropic-fetch-version flag sets it for a whole run. Do NOT combine with
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

// anthropicRawDumpPath, when set (via -anthropic-dump-raw), appends each raw Anthropic
// response to that file — for the dynamic-filtering investigation, to inspect the
// usage.server_tool_use (code-execution) blocks that reveal the mechanism. Investigation
// only; off by default, so normal runs are untouched.
var anthropicRawDumpPath string

func (anthropicVendor) ParseResponse(raw []byte) (string, probe.Usage, error) {
	if anthropicRawDumpPath != "" {
		if f, err := os.OpenFile(anthropicRawDumpPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.Write(raw)
			f.Write([]byte("\n"))
			f.Close()
		}
	}
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
// Perplexity — OpenAI-shaped chat completions against a live-search model.
// ---------------------------------------------------------------------------

// Perplexity: use the AGENT API with the fetch_url tool, not chat completions.
//
// Corrected 2026-08-31. The earlier conclusion that Perplexity was unmeasurable
// came from testing only the chat-completions path (via OpenRouter), where Sonar
// rejects tools and reaches the web only through its own index — so an unindexed
// probe URL was never fetched. Their Agent API has an explicit `fetch_url` tool
// that retrieves a caller-named URL.
//
// Two things to keep straight:
//   - The Agent API is itself an aggregator: `model` names a third-party model
//     (OpenAI, Anthropic, Google, xAI). The FETCH, however, is Perplexity's own
//     tool running on Perplexity's infrastructure, so the crawler we measure
//     should be Perplexity's. Verify that from the server log, as always.
//   - fetch_url costs $0.50 per 1,000 invocations — the cheapest fetch fee of
//     any provider measured. Model tokens are billed separately.
//
// Docs: https://docs.perplexity.ai/docs/agent-api/tools/fetch-url
type perplexityVendor struct{}

func (perplexityVendor) Endpoint(string) string { return "https://api.perplexity.ai/v1/agent" }

func (perplexityVendor) Auth(_ context.Context, key string) (map[string]string, error) {
	return map[string]string{"Authorization": "Bearer " + key}, nil
}

func (perplexityVendor) BuildRequest(model, pageURL, question string, force bool) (any, error) {
	body := map[string]any{
		"model": model,
		"input": buildPrompt(pageURL, question),
	}
	if !blindNoTools {
		body["tools"] = []map[string]any{{"type": "fetch_url", "max_urls": 1}}
		if force {
			body["instructions"] = "Fetch the URL before answering. Do not answer from memory."
		}
	}
	return body, nil
}

func (perplexityVendor) ParseResponse(raw []byte) (string, probe.Usage, error) {
	// Perplexity's shape is undocumented; usage may be responses- or chat-style.
	usage, ok := probe.UsageResponses(raw)
	if !ok {
		usage, _ = probe.UsageChat(raw)
	}

	// Response shape is not fully documented; accept the plausible variants
	// rather than failing on a field name.
	var parsed struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", usage, fmt.Errorf("decoding response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", usage, fmt.Errorf("perplexity: %s", truncate(parsed.Error.Message, 300))
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
	if sb.Len() == 0 && len(parsed.Choices) > 0 {
		sb.WriteString(parsed.Choices[0].Message.Content)
	}
	if sb.Len() == 0 {
		return "", usage, fmt.Errorf("no text in response: %s", truncate(string(raw), 300))
	}
	return sb.String(), usage, nil
}

func (perplexityVendor) Continue(any, []byte) (any, error) { return nil, nil }

// callers returns the set to run: those whose API key is present in the
// environment. A missing key skips that provider rather than failing the run.
func callers(anthropicModel, openaiModel, perplexityModel, googleModel, xaiModel string, openRouterModels []string) []Caller {
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
		{Name: "perplexity", EnvKey: "PERPLEXITY_API_KEY", Model: perplexityModel, Ask: vendorAsk(perplexityVendor{})},
		// Direct xAI account, separate from the OpenRouter x-ai route (openrouter.go).
		// Name is exactly "x-ai" so -sharing-first x-ai resolves; Tag() adds the model
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

// ---------------------------------------------------------------------------
// Google is parked until the study has a real hostname
// ---------------------------------------------------------------------------

// PROJECT DECISION, 2026-08-31: Google is out of the run until the probe is
// served from an ordinary domain name.
//
// Google documents that url_context does not support "tunneling services", and
// every host this study has had so far is one (`*.trycloudflare.com`). The
// failure is silent in the worst possible way: the fetcher DOES arrive and take
// the page with HTTP 200, so the server log looks like a healthy measurement,
// while the model answers that the URL is blocked. Left enabled, that produces
// a Google row showing a fetch every single round — which reads as "Google
// never caches" and is a false finding, not a null one.
//
// So this is not a preference to be remembered and re-litigated. It is checked
// from the URL, so Google comes back BY ITSELF the moment the hostname is real,
// and cannot be re-enabled by accident before then.
//
// Source and the full argument: PROVIDERS.md § "Google blocked by tunnel domains"
// and FINDINGS.md § "Google is blocked by the hostname, not by caching".

// tunnelHostSuffixes: hosts that hand out throwaway public names for a local
// process. The first two are the ones Google names; the rest are the same class
// of service, listed so a future host swap does not quietly reintroduce the bug.
var tunnelHostSuffixes = []string{
	"trycloudflare.com",
	"ngrok.io", "ngrok-free.app", "ngrok.app", "ngrok.dev",
	"pinggy.link", "pinggy.io",
	"loca.lt", "localhost.run", "serveo.net",
	"devtunnels.ms", "bore.pub", "telebit.io",
}

// isTunnelHost reports whether a URL's host is a throwaway tunnel name.
// Localhost and private addresses count too — Google excludes those in the same
// sentence, and neither is reachable by any provider anyway.
func isTunnelHost(rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return host, true
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return host, true
		}
	}
	for _, s := range tunnelHostSuffixes {
		if host == s || strings.HasSuffix(host, "."+s) {
			return host, true
		}
	}
	return host, false
}

// allowGoogleOnTunnel overrides the park. Exists for exactly one experiment:
// testing whether a NAMED Cloudflare tunnel on a real domain clears Google's
// restriction, where the hostname is real but the transport is still a tunnel.
// Do not set it for a measurement run.
var allowGoogleOnTunnel = false

// parkGoogle disables the Google routes when the page URL is a tunnel host, and
// says why. Returns the number parked so the caller can report it.
func parkGoogle(list []Caller, pageURL string) int {
	host, tunnel := isTunnelHost(pageURL)
	if !tunnel || allowGoogleOnTunnel {
		return 0
	}
	var parked []string
	for i := range list {
		if !strings.HasPrefix(list[i].Name, "google") || !list[i].Enabled {
			continue
		}
		list[i].Enabled = false
		parked = append(parked, list[i].Name)
	}
	n := len(parked)
	if n > 0 {
		fmt.Fprintf(os.Stderr,
			"[trial] PARKED %s — %s is not a public domain name, and Google documents\n"+
				"        that url_context supports neither tunneling services nor localhost\n"+
				"        and private addresses. Its fetcher would\n"+
				"        still take the page (HTTP 200) while the model reports it blocked,\n"+
				"        which would look like \"Google never caches\" — a false finding.\n"+
				"        Google re-enables itself once -url uses a real domain. See\n"+
				"        PROVIDERS.md for the citation. Override: -allow-tunnel-google.\n",
			strings.Join(parked, " and "), host)
	}
	return n
}
