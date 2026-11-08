package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"aisearch-probe/internal/probe"
)

// OpenRouter reaches every provider through one OpenAI-compatible endpoint.
// That is worth having for its own sake: with a single code path, "the providers
// behaved differently" can no longer be confounded with "our code differed per
// provider".
//
// It also inserts a party we do not control between us and the provider — the
// client-side twin of the CDN problem on the server side. Two consequences,
// both load-bearing:
//
//  1. `engine` MUST be "native", so the provider's own fetcher retrieves the
//     page. The default is "auto" and there is a free "openrouter" engine;
//     either would have OpenRouter fetch the page itself, and our server would
//     log OpenRouter's user-agent instead of the vendor's. A free engine that
//     silently voids every result is the most expensive option available.
//  2. Verify, don't trust. The probe logs the user-agent of whatever fetches
//     the page. If it does not say ChatGPT-User / Claude-User / PerplexityBot
//     etc., the passthrough is not happening and the run means nothing.
//
// The direct OpenAI and Anthropic accounts are the calibration for this route.

const openRouterBase = "https://openrouter.ai/api/v1"

// openRouterUser is the optional per-end-user id for the current OpenRouter call
// (set by the per-user isolation test; empty otherwise). See its use below.
var openRouterUser string

// defaultOpenRouterModels is the set MEASURED to work through OpenRouter, i.e.
// where the vendor's own fetcher reaches our server. Verified 2026-08-31.
//
// Deliberately excluded, because including them makes every run fail the
// preflight for reasons no flag can fix:
//
//	google/gemini-2.5-pro  — no native fetch path via OpenRouter. web_search
//	                         finds nothing (our URL is unindexed); web_fetch is
//	                         served by OpenRouter's own fetcher, so any result
//	                         would be about OpenRouter. Use the DIRECT Gemini
//	                         API instead — that needs GCP billing enabled.
//	perplexity/sonar-pro   — rejects tools outright and reaches the web only
//	                         through its own index. Out of scope.
//
// Add either back with -openrouterModels if the situation changes, but re-read
// the "who fetched" line before believing the numbers.
//
// THE CHEAP MODEL IS THE DEFAULT, and the point is more runs, not a smaller
// bill. The task is: fetch one page, answer one easy question, quote the code
// at the bottom. It does not need a frontier model, and the study needs every
// square of its grid filled more than once. Prices below are per million
// tokens, read from OpenRouter's model list on 2026-09-13.
//
// The model each one replaced is kept in largeOpenRouterModels: whether a big
// and a small model at one vendor share a stored copy is its own question, and
// answering it needs both. Pass both to -openrouterModels to run that.
//
// VERIFY BEFORE TRUSTING A SWAP. A cheaper model that answers from memory
// instead of calling its tool produces "nothing reached the server", which
// reads as caching and is a lie. claude-opus-5 did exactly that — answered in
// 1.5s without fetching — which is why 4.8 was pinned here before. Run -checkVendors
// after any change to this list and read the who-fetched line.
var defaultOpenRouterModels = []string{
	"openai/gpt-5-mini", // $0.25 in / $2.00 out, replacing gpt-5.5 at $5 / $30
	// NOT claude-haiku-4.5, though it is cheaper. MEASURED 2026-09-13: with
	// haiku-4.5 the page was fetched by `OpenRouter-WebFetch`, not by
	// `Claude-User` — OpenRouter has no native Anthropic fetch path for that
	// model and silently substitutes its own fetcher, so every result would be
	// about OpenRouter's cache rather than Anthropic's. sonnet-5 was checked the
	// same way and `anthropic-query` arrived. This is the failure D9 warns
	// about, caught by -checkVendors exactly as intended.
	//
	// Dots, not dashes: "claude-sonnet-5" happens to be the same either way, but
	// "claude-opus-4-8" is Anthropic's own API style and OpenRouter silently
	// resolves that shape to an unnamed model.
	"anthropic/claude-sonnet-5", // $2 / $10, replacing claude-opus-4.8 at $5 / $25
	"x-ai/grok-4.3",             // $1.25 / $2.50, replacing grok-4.6 at $2 / $6
}

// largeOpenRouterModels is the expensive half of each pair, kept only so the
// cross-model question can be asked. These are the models every result before
// 2026-09-13 was measured on.
var largeOpenRouterModels = []string{
	"openai/gpt-5.5",
	"anthropic/claude-opus-4.8",
	"x-ai/grok-4.6",
}

// vendorOf turns "anthropic/claude-opus-4-8" into "anthropic", so the report
// separates vendors rather than lumping every OpenRouter call together.
func vendorOf(slug string) string {
	if i := strings.IndexByte(slug, '/'); i > 0 {
		return slug[:i]
	}
	return slug
}

// toolForVendor: the right OpenRouter tool differs per vendor, and getting it
// wrong does not error — it silently produces a result about the wrong party.
// All of this is MEASURED, 2026-08-31, by reading which fetcher reached our
// server. Re-verify whenever a vendor or model changes.
//
//	vendor      tool          outcome
//	---------   -----------   ----------------------------------------------
//	openai      web_search    ChatGPT-User arrived — native passthrough works
//	            web_fetch     OpenRouter-WebFetch arrived — WRONG PARTY
//	anthropic   web_fetch     Claude-User arrived (and fetched robots.txt first)
//	            web_search    nothing arrived — search cannot find an unindexed URL
//	x-ai        web_search    fetched, but undeclared
//	google      web_search    nothing arrived; web_fetch untried
//	perplexity  (none)        nothing arrived; Sonar rejects tools entirely
//
// The pattern: a vendor with a native *fetch* tool needs web_fetch; a vendor
// with only native *search* needs web_search. Our URL is in no search index, so
// the wrong choice means the model looks for a page it can never find.
var toolForVendor = map[string]string{
	"openai":     "openrouter:web_search",
	"anthropic":  "openrouter:web_fetch",
	"x-ai":       "openrouter:web_search",
	"google":     "openrouter:web_fetch", // untried; search found nothing
	"perplexity": "",                     // Sonar reports no tool-use support
}

// openRouterTool is the fallback for a vendor not in the table.
var openRouterTool = "openrouter:web_search"

// toolFor is the fetch tool to declare for a model's underlying vendor. An empty
// result means declare no tool at all: Perplexity's Sonar rejects tools outright
// ("No endpoints found that support tool use", measured 2026-08-31) and searches
// inherently. openRouterCallers bakes this into each variant's tool field.
func toolFor(model string) string {
	if t, ok := toolForVendor[vendorOf(model)]; ok {
		return t
	}
	return openRouterTool
}

// openRouterVendor is the shared chat-completions transport, configured per
// underlying vendor. tool is the fetch tool to declare for that underlying
// ("openrouter:web_fetch" for Anthropic, "openrouter:web_search" for OpenAI/xAI);
// an empty tool means declare none (Perplexity's Sonar rejects tools). That is the
// only per-underlying difference today, so it is one field — not a monolith that
// switches internally, and not N copies.
type openRouterVendor struct {
	tool string
}

func (openRouterVendor) Endpoint(string) string { return openRouterBase + "/chat/completions" }

func (openRouterVendor) Auth(_ context.Context, key string) (map[string]string, error) {
	return map[string]string{
		"Authorization": "Bearer " + key,
		// OpenRouter attributes traffic by these; harmless and good manners.
		"HTTP-Referer": "https://aisearch-probe",
		"X-Title":      "ai-crawler-cache-probe",
	}, nil
}

func (v openRouterVendor) BuildRequest(model, pageURL, question string, force bool) (any, error) {
	body := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": buildPrompt(pageURL, question)},
		},
	}
	if !blindNoTools && v.tool != "" {
		// engine:native is the entire point — see the note above.
		body["tools"] = []map[string]any{{
			"type":       v.tool,
			"parameters": map[string]any{"engine": "native"},
		}}
		if force {
			body["tool_choice"] = "required"
		}
	}

	// openRouterUser, when set, is sent as the request's `user` field. OpenRouter
	// folds it into a hashed per-end-user identity forwarded upstream (see its
	// user-tracking docs), which is what the per-user isolation test needs to
	// present two distinct end-users on one account. Empty = omit it entirely.
	if openRouterUser != "" {
		body["user"] = openRouterUser
	}
	return body, nil
}

func (openRouterVendor) ParseResponse(raw []byte) (string, probe.Usage, error) {
	usage, _ := probe.UsageChat(raw)

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", usage, fmt.Errorf("decoding response: %w", err)
	}
	// OpenRouter can return a 200 with an error object in the body.
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", usage, fmt.Errorf("openrouter: %s", truncate(parsed.Error.Message, 300))
	}
	if len(parsed.Choices) == 0 {
		return "", usage, fmt.Errorf("no choices in response: %s", truncate(string(raw), 300))
	}
	return parsed.Choices[0].Message.Content, usage, nil
}

func (openRouterVendor) Continue(any, []byte) (any, error) { return nil, nil }

// openRouterCallers makes one Caller per model, named "or:<vendor>" so the
// report keeps the vendors apart and distinguishes them from a direct account.
// Each holds an openRouterVendor configured with the fetch tool its underlying
// vendor needs.
func openRouterCallers(models []string) []Caller {
	out := make([]Caller, 0, len(models))
	for _, m := range models {
		out = append(out, Caller{
			Name:   "or:" + vendorOf(m),
			EnvKey: "OPENROUTER_API_KEY",
			Model:  m,
			Ask:    vendorAsk(openRouterVendor{tool: toolFor(m)}),
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Credit precheck
// ---------------------------------------------------------------------------

// OpenRouterCredit is what the key has left to spend.
type OpenRouterCredit struct {
	Usage     float64  // dollars spent on this key
	Limit     *float64 // nil means no limit set on the key
	Remaining *float64 // nil means unlimited
	IsFree    bool
}

// openRouterCredit reads the key's own usage and limit. Worth doing before a
// long run: a 90-minute schedule that dies at minute 70 because the credit ran
// out is not a partial result, it is a void one that has to be redone.
func openRouterCredit(ctx context.Context, key string) (*OpenRouterCredit, error) {
	raw, err := httpGet(ctx, openRouterBase+"/key", map[string]string{
		"Authorization": "Bearer " + key,
	})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Data struct {
			Usage      float64  `json:"usage"`
			Limit      *float64 `json:"limit"`
			IsFreeTier bool     `json:"is_free_tier"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decoding key info: %w", err)
	}
	c := &OpenRouterCredit{
		Usage:  parsed.Data.Usage,
		Limit:  parsed.Data.Limit,
		IsFree: parsed.Data.IsFreeTier,
	}
	if parsed.Data.Limit != nil {
		rem := *parsed.Data.Limit - parsed.Data.Usage
		c.Remaining = &rem
	}
	return c, nil
}

func (c *OpenRouterCredit) String() string {
	if c.Limit == nil {
		return fmt.Sprintf("$%.2f spent, no limit set on this key", c.Usage)
	}
	return fmt.Sprintf("$%.2f spent of $%.2f — $%.2f left",
		c.Usage, *c.Limit, *c.Remaining)
}

// FitsRun reports whether the remaining credit plausibly covers a run of n
// calls, using a deliberately pessimistic per-call figure. Returns ok=false
// only when we can be confident it will NOT fit; an unset limit always fits.
func (c *OpenRouterCredit) FitsRun(calls int, worstCasePerCall float64) (ok bool, need float64) {
	need = float64(calls) * worstCasePerCall
	if c.Remaining == nil {
		return true, need
	}
	return *c.Remaining >= need, need
}
