package probe

// Token-usage parsing — a SHARED module for every run, not just one experiment.
//
// Every model call has a cost, and the standing requirement is that every
// event records it. The cost is raw token usage (a fact);
// dollars are derived later from the vendors' prices. Providers report usage in
// different shapes and some omit it, so this is a small general interface — one
// UsageParser per response shape — that any runner (or the analyzer, or a new
// script) can reuse rather than re-parsing vendor JSON by hand.

import "encoding/json"

// Usage is the token cost of one model call. Zero fields mean the provider reported
// none (e.g. an errored call). Raw facts only — no dollars.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// Rounds is how many request rounds (POSTs) the call took. 1 is a normal call;
	// >1 means a server-tool turn paused and was resumed — Anthropic's pause_turn,
	// where each resume re-sends the accumulated context and re-bills it. It is the
	// direct measure of the pause_turn amplification behind the PDF cost blow-up, so
	// it is recorded as a fact rather than inferred from inflated input tokens.
	Rounds int `json:"rounds,omitempty"`
	// FetchStatus is the provider's OWN report of what happened to each fetch, in
	// its own words, "<STATUS> <url>" joined by "; ". Not a token count, but it is
	// per-call and must travel with the call like one: every runner keeps the
	// provider's fetch report (a bare answer cannot say WHY a page was not read).
	// Set today by the Gemini parse (urlRetrievalStatus, success included).
	FetchStatus string `json:"fetch_status,omitempty"`
}

// Add sums two usages (e.g. across the rounds of a paused Anthropic turn). Per-round
// parses never set Rounds, so it stays 0 here and the driver sets the final count.
// Fetch statuses are joined in order, never overwritten.
func (u Usage) Add(o Usage) Usage {
	fs := u.FetchStatus
	if o.FetchStatus != "" {
		if fs != "" {
			fs += "; "
		}
		fs += o.FetchStatus
	}
	return Usage{
		InputTokens:  u.InputTokens + o.InputTokens,
		OutputTokens: u.OutputTokens + o.OutputTokens,
		Rounds:       u.Rounds + o.Rounds,
		FetchStatus:  fs,
	}
}

// UsageParser extracts token usage from a raw provider response body. found is
// true only when at least one non-zero token count was read — so feeding the WRONG
// parser (a mismatched shape yields all zeros) reports found=false instead of a
// silent spurious 0-cost, and a caller can fall back or flag it.
type UsageParser func(raw []byte) (u Usage, found bool)

func found(u Usage) (Usage, bool) { return u, u.InputTokens > 0 || u.OutputTokens > 0 }

// UsageResponses parses the OpenAI / xAI Responses API and the Anthropic Messages
// API shape: {"usage":{"input_tokens":N,"output_tokens":M}}.
func UsageResponses(raw []byte) (Usage, bool) {
	var r struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(raw, &r)
	return found(Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens})
}

// UsageChat parses the chat-completions shape used by OpenRouter and Perplexity:
// {"usage":{"prompt_tokens":N,"completion_tokens":M}}.
func UsageChat(raw []byte) (Usage, bool) {
	var r struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(raw, &r)
	return found(Usage{InputTokens: r.Usage.PromptTokens, OutputTokens: r.Usage.CompletionTokens})
}

// UsageGemini parses the Gemini shape:
// {"usageMetadata":{"promptTokenCount":N,"candidatesTokenCount":M}}.
func UsageGemini(raw []byte) (Usage, bool) {
	var r struct {
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	_ = json.Unmarshal(raw, &r)
	return found(Usage{InputTokens: r.UsageMetadata.PromptTokenCount, OutputTokens: r.UsageMetadata.CandidatesTokenCount})
}
