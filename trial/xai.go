package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"aisearch-probe/internal/probe"
)

// ---------------------------------------------------------------------------
// xAI (Grok) — DIRECT account, its own Responses API with the web_search tool.
// ---------------------------------------------------------------------------
//
// This is xAI as a first-class customer, separate from the OpenRouter x-ai
// route (openrouter.go). The two are UNRELATED accounts reaching one vendor,
// which is what the cross-account sharing test needs: does one account's
// download serve another account's question? See doc/xai-direct-provider-handover.md.
//
// The shape is OpenAI's Responses API: xAI serves an OpenAI-compatible
// /v1/responses, so this mirrors openaiVendor. The one tool that matters is
// web_search — xAI's LIVE search-and-browse tool, which fetches a named URL so
// a request actually reaches our server. An index-only search would never find
// our unindexed page, and the run would measure nothing.
//
// Forcing the fetch: tool_choice "required" makes the model call the tool
// instead of answering from memory (verified against xAI's Responses gateway,
// which takes the untagged "auto"|"none"|"required"). Without a forced call,
// "no request arrived" is ambiguous between "the page was cached" and "the model
// never looked" — which is the whole reason -forceFetch defaults on. When the
// tool is present but not forced we still pin "auto", because xAI's API may
// otherwise leave the tool unused and answer from its own knowledge silently.
//
// Attribution caveat, MEASURED (PROVIDERS.md § "xAI's fetcher does not identify
// itself"): xAI's fetcher forges ordinary-browser user-agents and fires several
// requests per ask. So a fetch is attributed by the per-provider probe TOKEN in
// the URL, never by user-agent. -checkVendors must confirm a request actually reaches
// us before any result is believed.

type xaiVendor struct{}

func (xaiVendor) Endpoint(string) string { return "https://api.x.ai/v1/responses" }

func (xaiVendor) Auth(_ context.Context, key string) (map[string]string, error) {
	return map[string]string{"Authorization": "Bearer " + key}, nil
}

func (xaiVendor) BuildRequest(model, pageURL, question string, force bool) (any, error) {
	body := map[string]any{
		"model": model,
		"input": buildPrompt(pageURL, question),
	}
	if !blindNoTools {
		// web_search is xAI's live fetch-and-browse tool. It is the only tool
		// declared, so requiring a tool call requires a fetch.
		body["tools"] = []map[string]any{{"type": "web_search"}}
		if force {
			body["tool_choice"] = "required"
		} else {
			// Not forced, but the tool is present: pin "auto" so xAI actually
			// considers it. Left unset, its API can skip the tool and answer from
			// memory, which would look like a cache hit that never happened.
			body["tool_choice"] = "auto"
		}
	}
	return body, nil
}

func (xaiVendor) ParseResponse(raw []byte) (string, probe.Usage, error) {
	usage, _ := probe.UsageResponses(raw)

	// OpenAI Responses shape, which xAI mirrors: prefer the flattened
	// output_text, fall back to walking the output blocks.
	var parsed struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", usage, fmt.Errorf("decoding response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", usage, fmt.Errorf("xai: %s", truncate(parsed.Error.Message, 300))
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
	if sb.Len() == 0 {
		return "", usage, fmt.Errorf("no text in response: %s", truncate(string(raw), 300))
	}
	return sb.String(), usage, nil
}

func (xaiVendor) Continue(any, []byte) (any, error) { return nil, nil }
