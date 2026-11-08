package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aisearch-probe/internal/probe"
)

// These tests are the SPEC for the vendor abstraction. They must express exactly
// what the current ask* functions do, because the refactor is a restructure, not a
// behaviour change. They are written first and go red against the stubs in
// vendor.go; the implementation step makes them green one vendor at a time.

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// asMap casts a BuildRequest body to a map for inspection, failing loudly if the
// shape is not the expected JSON object.
func asMap(t *testing.T, body any) map[string]any {
	t.Helper()
	m, ok := body.(map[string]any)
	if !ok {
		t.Fatalf("request body is %T, want map[string]any", body)
	}
	return m
}

// firstToolType returns the "type" of the first declared tool, and whether any
// tools were declared at all. Tools are built as []map[string]any, mirroring the
// old ask* functions.
func firstToolType(t *testing.T, m map[string]any) (string, bool) {
	t.Helper()
	raw, ok := m["tools"]
	if !ok {
		return "", false
	}
	tools, ok := raw.([]map[string]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools has wrong shape %T or is empty", raw)
	}
	typ, _ := tools[0]["type"].(string)
	return typ, true
}

// recordingPoster returns a poster that hands back the canned replies in order
// and records every (endpoint, headers, body) it was called with. It is the
// generic form of the old anthropicPoster seam.
func recordingPoster(replies ...string) (poster, *[]map[string]any, *[]string) {
	var bodies []map[string]any
	var endpoints []string
	i := 0
	p := func(ctx context.Context, endpoint string, headers map[string]string, body any) ([]byte, error) {
		endpoints = append(endpoints, endpoint)
		if m, ok := body.(map[string]any); ok {
			bodies = append(bodies, m)
		} else {
			bodies = append(bodies, nil)
		}
		out := replies[i]
		i++
		return []byte(out), nil
	}
	return p, &bodies, &endpoints
}

// withGlobals saves and restores the blind-mode / openrouter-user globals around a
// closure so one test cannot leak call-shape into another.
func withGlobals(fn func()) {
	na, nt, u := blindNoAddress, blindNoTools, openRouterUser
	defer func() { blindNoAddress, blindNoTools, openRouterUser = na, nt, u }()
	fn()
}

// ---------------------------------------------------------------------------
// Endpoint + Auth — the cheap, exact facts.
// ---------------------------------------------------------------------------

func TestVendorEndpoints(t *testing.T) {
	cases := []struct {
		name  string
		v     Vendor
		model string
		want  string
	}{
		{"openai", openaiVendor{}, "gpt-5-mini", "https://api.openai.com/v1/responses"},
		{"anthropic", anthropicVendor{}, "claude-sonnet-5", "https://api.anthropic.com/v1/messages"},
		{"xai", xaiVendor{}, "grok-4.3", "https://api.x.ai/v1/responses"},
		{"openrouter", openRouterVendor{tool: "openrouter:web_search"}, "openai/gpt-5-mini", "https://openrouter.ai/api/v1/chat/completions"},
		// Gemini is the one that folds the model into the URL.
		{"gemini", geminiVendor{}, "gemini-2.5-flash", "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.v.Endpoint(c.model); got != c.want {
				t.Errorf("Endpoint(%q) = %q, want %q", c.model, got, c.want)
			}
		})
	}
}

func TestVendorAuthHeaders(t *testing.T) {
	ctx := context.Background()

	// OpenAI / xAI / Perplexity: plain bearer.
	for _, tc := range []struct {
		name string
		v    Vendor
	}{{"openai", openaiVendor{}}, {"xai", xaiVendor{}}} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := tc.v.Auth(ctx, "KEY")
			if err != nil {
				t.Fatalf("Auth error: %v", err)
			}
			if h["Authorization"] != "Bearer KEY" {
				t.Errorf("Authorization = %q, want %q", h["Authorization"], "Bearer KEY")
			}
		})
	}

	// Anthropic: x-api-key plus the version header.
	t.Run("anthropic", func(t *testing.T) {
		h, err := anthropicVendor{}.Auth(ctx, "KEY")
		if err != nil {
			t.Fatalf("Auth error: %v", err)
		}
		if h["x-api-key"] != "KEY" {
			t.Errorf("x-api-key = %q, want KEY", h["x-api-key"])
		}
		if h["anthropic-version"] == "" {
			t.Error("anthropic-version header missing")
		}
	})

	// Gemini: the key goes in a header, never the query string.
	t.Run("gemini", func(t *testing.T) {
		h, err := geminiVendor{}.Auth(ctx, "KEY")
		if err != nil {
			t.Fatalf("Auth error: %v", err)
		}
		if h["x-goog-api-key"] != "KEY" {
			t.Errorf("x-goog-api-key = %q, want KEY", h["x-goog-api-key"])
		}
	})

	// OpenRouter: bearer plus the attribution headers.
	t.Run("openrouter", func(t *testing.T) {
		h, err := openRouterVendor{tool: "openrouter:web_search"}.Auth(ctx, "KEY")
		if err != nil {
			t.Fatalf("Auth error: %v", err)
		}
		if h["Authorization"] != "Bearer KEY" {
			t.Errorf("Authorization = %q, want Bearer KEY", h["Authorization"])
		}
		if h["X-Title"] == "" || h["HTTP-Referer"] == "" {
			t.Error("OpenRouter attribution headers (HTTP-Referer / X-Title) missing")
		}
	})
}

// ---------------------------------------------------------------------------
// BuildRequest — the tool declaration, the force choice, the blind knobs, and
// that the prompt actually carries the URL and the question.
// ---------------------------------------------------------------------------

// The prompt every non-blind vendor sends is EXACTLY the address, a blank line and
// the question — no wrapper words before or after (the standard-question rule). Checked on
// the bytes inside each vendor's own request body.
func assertNormalPrompt(t *testing.T, prompt, pageURL, question string) {
	t.Helper()
	if want := pageURL + "\n\n" + question; prompt != want {
		t.Errorf("prompt is not exactly the address and the question:\n got %q\nwant %q", prompt, want)
	}
}

func TestOpenAIBuildRequest(t *testing.T) {
	withGlobals(func() {
		v := openaiVendor{}

		// Forced: web_search declared, tool_choice required, prompt well formed.
		m := asMap(t, mustBuild(t, v, "gpt-5-mini", "https://ex/p1", "what is the heading?", true))
		if m["model"] != "gpt-5-mini" {
			t.Errorf("model = %v, want gpt-5-mini", m["model"])
		}
		assertNormalPrompt(t, m["input"].(string), "https://ex/p1", "what is the heading?")
		if typ, ok := firstToolType(t, m); !ok || typ != "web_search" {
			t.Errorf("tool = %q present=%v, want web_search present", typ, ok)
		}
		if m["tool_choice"] != "required" {
			t.Errorf("tool_choice = %v, want required", m["tool_choice"])
		}

		// Not forced: tool still declared, but no forced choice.
		m = asMap(t, mustBuild(t, v, "gpt-5-mini", "https://ex/p1", "q", false))
		if _, ok := firstToolType(t, m); !ok {
			t.Error("tool must still be declared when not forced")
		}
		if _, ok := m["tool_choice"]; ok {
			t.Error("tool_choice must be absent when not forced")
		}

		// Blind (no tools): the web tool is omitted entirely.
		blindNoTools = true
		m = asMap(t, mustBuild(t, v, "gpt-5-mini", "https://ex/p1", "q", true))
		if _, ok := m["tools"]; ok {
			t.Error("blindNoTools must omit the tools key")
		}
		blindNoTools = false

		// Blind (no address): the prompt is the bare question, no URL, no footer.
		blindNoAddress = true
		m = asMap(t, mustBuild(t, v, "gpt-5-mini", "https://ex/p1", "just the question", true))
		if got := m["input"].(string); got != "just the question" {
			t.Errorf("blindNoAddress input = %q, want the bare question", got)
		}
	})
}

func TestXAIBuildRequest(t *testing.T) {
	withGlobals(func() {
		v := xaiVendor{}

		// Forced: web_search + tool_choice required.
		m := asMap(t, mustBuild(t, v, "grok-4.3", "https://ex/p", "q", true))
		assertNormalPrompt(t, m["input"].(string), "https://ex/p", "q")
		if typ, ok := firstToolType(t, m); !ok || typ != "web_search" {
			t.Errorf("tool = %q present=%v, want web_search", typ, ok)
		}
		if m["tool_choice"] != "required" {
			t.Errorf("forced tool_choice = %v, want required", m["tool_choice"])
		}

		// Not forced but tool present: xAI must pin "auto" so it does not silently
		// skip the tool and answer from memory (which would read as a cache hit).
		m = asMap(t, mustBuild(t, v, "grok-4.3", "https://ex/p", "q", false))
		if m["tool_choice"] != "auto" {
			t.Errorf("unforced tool_choice = %v, want auto", m["tool_choice"])
		}

		// Blind: no tool, and therefore no tool_choice at all.
		blindNoTools = true
		m = asMap(t, mustBuild(t, v, "grok-4.3", "https://ex/p", "q", false))
		if _, ok := m["tools"]; ok {
			t.Error("blindNoTools must omit the tools key")
		}
		if _, ok := m["tool_choice"]; ok {
			t.Error("blindNoTools must omit tool_choice")
		}
	})
}

func TestAnthropicBuildRequest(t *testing.T) {
	withGlobals(func() {
		v := anthropicVendor{}

		m := asMap(t, mustBuild(t, v, "claude-sonnet-5", "https://ex/p", "q", true))
		if m["model"] != "claude-sonnet-5" {
			t.Errorf("model = %v", m["model"])
		}
		if m["max_tokens"] == nil {
			t.Error("max_tokens must be set")
		}
		msgs, ok := m["messages"].([]any)
		if !ok || len(msgs) != 1 {
			t.Fatalf("messages = %v, want one user message", m["messages"])
		}
		user := msgs[0].(map[string]any)
		if user["role"] != "user" {
			t.Errorf("first message role = %v, want user", user["role"])
		}
		assertNormalPrompt(t, user["content"].(string), "https://ex/p", "q")

		// web_fetch declared; forced turns on tool_choice type "any".
		if typ, ok := firstToolType(t, m); !ok || typ != "web_fetch_20260209" {
			t.Errorf("tool = %q present=%v, want web_fetch_20260209", typ, ok)
		}
		tc, ok := m["tool_choice"].(map[string]any)
		if !ok || tc["type"] != "any" {
			t.Errorf("forced tool_choice = %v, want {type:any}", m["tool_choice"])
		}

		// Blind: no tools.
		blindNoTools = true
		m = asMap(t, mustBuild(t, v, "claude-sonnet-5", "https://ex/p", "q", true))
		if _, ok := m["tools"]; ok {
			t.Error("blindNoTools must omit the tools key")
		}
	})
}

func TestGeminiBuildRequest(t *testing.T) {
	withGlobals(func() {
		m := asMap(t, mustBuild(t, geminiVendor{}, "gemini-2.5-flash", "https://ex/p", "q", true))
		// Gemini's tools live under urlContext, and the prompt is inside contents.
		tools, ok := m["tools"].([]map[string]any)
		if !ok || len(tools) == 0 {
			t.Fatalf("tools = %v, want urlContext", m["tools"])
		}
		if _, ok := tools[0]["urlContext"]; !ok {
			t.Errorf("first tool = %v, want urlContext", tools[0])
		}
		contents, ok := m["contents"].([]map[string]any)
		if !ok || len(contents) == 0 {
			t.Fatalf("contents = %v", m["contents"])
		}
		parts := contents[0]["parts"].([]map[string]any)
		assertNormalPrompt(t, parts[0]["text"].(string), "https://ex/p", "q")
	})
}

func TestOpenRouterBuildRequestPerUnderlying(t *testing.T) {
	withGlobals(func() {
		// A variant configured with a tool declares exactly that tool, engine
		// native, and forces via tool_choice required.
		v := openRouterVendor{tool: "openrouter:web_fetch"}
		m := asMap(t, mustBuild(t, v, "anthropic/claude-sonnet-5", "https://ex/p", "q", true))
		tools, ok := m["tools"].([]map[string]any)
		if !ok || len(tools) == 0 {
			t.Fatalf("tools = %v", m["tools"])
		}
		if tools[0]["type"] != "openrouter:web_fetch" {
			t.Errorf("tool type = %v, want openrouter:web_fetch", tools[0]["type"])
		}
		params, _ := tools[0]["parameters"].(map[string]any)
		if params["engine"] != "native" {
			t.Errorf("engine = %v, want native (the whole point of the route)", params["engine"])
		}
		if m["tool_choice"] != "required" {
			t.Errorf("forced tool_choice = %v, want required", m["tool_choice"])
		}
		// The prompt lives in a chat-completions messages array.
		msgs := m["messages"].([]map[string]any)
		assertNormalPrompt(t, msgs[0]["content"].(string), "https://ex/p", "q")

		// A variant configured with NO tool (Perplexity's Sonar) declares none.
		none := openRouterVendor{tool: ""}
		m = asMap(t, mustBuild(t, none, "perplexity/sonar", "https://ex/p", "q", true))
		if _, ok := m["tools"]; ok {
			t.Error("an empty-tool variant must declare no tool")
		}

		// openRouterUser, when set, is forwarded as the request's user field.
		openRouterUser = "user-A"
		m = asMap(t, mustBuild(t, v, "anthropic/claude-sonnet-5", "https://ex/p", "q", true))
		if m["user"] != "user-A" {
			t.Errorf("user field = %v, want user-A", m["user"])
		}
	})
}

// mustBuild is BuildRequest with a fatal on error, to keep the cases above short.
func mustBuild(t *testing.T, v Vendor, model, url, question string, force bool) any {
	t.Helper()
	b, err := v.BuildRequest(model, url, question, force)
	if err != nil {
		t.Fatalf("BuildRequest error: %v", err)
	}
	return b
}

// ---------------------------------------------------------------------------
// ParseResponse — the answer, the usage, and the failure guards.
// ---------------------------------------------------------------------------

func TestParseResponseAnswerAndUsage(t *testing.T) {
	cases := []struct {
		name      string
		v         Vendor
		raw       string
		wantText  string
		wantUsage probe.Usage
	}{
		{
			name:      "openai output_text + responses usage",
			v:         openaiVendor{},
			raw:       `{"output_text":"the heading is X","usage":{"input_tokens":100,"output_tokens":20}}`,
			wantText:  "the heading is X",
			wantUsage: probe.Usage{InputTokens: 100, OutputTokens: 20},
		},
		{
			name:      "openai falls back to output blocks",
			v:         openaiVendor{},
			raw:       `{"output":[{"content":[{"type":"output_text","text":"a"},{"type":"output_text","text":"b"}]}],"usage":{"input_tokens":5,"output_tokens":1}}`,
			wantText:  "ab",
			wantUsage: probe.Usage{InputTokens: 5, OutputTokens: 1},
		},
		{
			name:      "xai output_text + responses usage",
			v:         xaiVendor{},
			raw:       `{"output_text":"grok says X","usage":{"input_tokens":7,"output_tokens":3}}`,
			wantText:  "grok says X",
			wantUsage: probe.Usage{InputTokens: 7, OutputTokens: 3},
		},
		{
			name:      "anthropic text blocks + responses-shaped usage",
			v:         anthropicVendor{},
			raw:       `{"content":[{"type":"text","text":"the code is TRUNC-abc"}],"stop_reason":"end_turn","usage":{"input_tokens":40,"output_tokens":9}}`,
			wantText:  "the code is TRUNC-abc",
			wantUsage: probe.Usage{InputTokens: 40, OutputTokens: 9},
		},
		{
			name:      "gemini candidates + gemini usage",
			v:         geminiVendor{},
			raw:       `{"candidates":[{"content":{"parts":[{"text":"hello from gemini"}]}}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":4}}`,
			wantText:  "hello from gemini",
			wantUsage: probe.Usage{InputTokens: 12, OutputTokens: 4},
		},
		{
			name:      "openrouter choices + chat usage",
			v:         openRouterVendor{tool: "openrouter:web_search"},
			raw:       `{"choices":[{"message":{"content":"or answer"}}],"usage":{"prompt_tokens":30,"completion_tokens":6}}`,
			wantText:  "or answer",
			wantUsage: probe.Usage{InputTokens: 30, OutputTokens: 6},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, usage, err := c.v.ParseResponse([]byte(c.raw))
			if err != nil {
				t.Fatalf("ParseResponse error: %v", err)
			}
			if got != c.wantText {
				t.Errorf("answer = %q, want %q", got, c.wantText)
			}
			if usage != c.wantUsage {
				t.Errorf("usage = %+v, want %+v", usage, c.wantUsage)
			}
		})
	}
}

// A failure the response itself declares must come back as an error, never as a
// silent empty answer — otherwise a broken route reads as "the model chose not to
// fetch" or "it reused a cached copy".
func TestParseResponseSurfacesFailures(t *testing.T) {
	cases := []struct {
		name string
		v    Vendor
		raw  string
	}{
		{
			// The silent-and-wrong guard: Gemini says per-URL it could not retrieve.
			name: "gemini urlRetrievalStatus not SUCCESS",
			v:    geminiVendor{},
			raw:  `{"candidates":[{"content":{"parts":[{"text":"I cannot access it"}]},"urlContextMetadata":{"urlMetadata":[{"retrievedUrl":"https://ex/p","urlRetrievalStatus":"URL_RETRIEVAL_STATUS_ERROR"}]}}]}`,
		},
		{"gemini no candidates", geminiVendor{}, `{"candidates":[]}`},
		{"xai error object in body", xaiVendor{}, `{"error":{"message":"bad model"}}`},
		{"openrouter error object in a 200", openRouterVendor{tool: "openrouter:web_search"}, `{"error":{"message":"upstream failed"}}`},
		{"openrouter no choices", openRouterVendor{tool: "openrouter:web_search"}, `{"choices":[]}`},
		{"anthropic refusal", anthropicVendor{}, `{"content":[],"stop_reason":"refusal"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := c.v.ParseResponse([]byte(c.raw))
			if err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

// A Gemini response that reports SUCCESS (or reports nothing at all, on older API
// versions) must NOT be treated as a failure.
func TestGeminiSuccessIsNotAnError(t *testing.T) {
	raw := `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"urlContextMetadata":{"urlMetadata":[{"retrievedUrl":"https://ex/p","urlRetrievalStatus":"URL_RETRIEVAL_STATUS_SUCCESS"}]}}]}`
	got, _, err := geminiVendor{}.ParseResponse([]byte(raw))
	if err != nil {
		t.Fatalf("a successful retrieval must not error: %v", err)
	}
	if got != "ok" {
		t.Errorf("answer = %q, want ok", got)
	}
}

// ---------------------------------------------------------------------------
// Continue — only Anthropic pauses; everyone else is always done.
// ---------------------------------------------------------------------------

func TestContinueNilForNonPausingVendors(t *testing.T) {
	end := []byte(`{"output_text":"done","usage":{"input_tokens":1,"output_tokens":1}}`)
	for _, tc := range []struct {
		name string
		v    Vendor
	}{
		{"openai", openaiVendor{}},
		{"xai", xaiVendor{}},
		{"gemini", geminiVendor{}},
		{"openrouter", openRouterVendor{tool: "openrouter:web_search"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, err := tc.v.Continue(map[string]any{}, end)
			if err != nil {
				t.Fatalf("Continue error: %v", err)
			}
			if next != nil {
				t.Errorf("Continue = %v, want nil (this vendor never pauses)", next)
			}
		})
	}
}

func TestAnthropicContinue(t *testing.T) {
	v := anthropicVendor{}
	prev := map[string]any{
		"model":    "claude-sonnet-5",
		"messages": []any{map[string]any{"role": "user", "content": "fetch it"}},
	}

	// end_turn: nothing more to do.
	next, err := v.Continue(prev, []byte(`{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`))
	if err != nil {
		t.Fatalf("Continue error: %v", err)
	}
	if next != nil {
		t.Error("end_turn must return nil (finished)")
	}

	// pause_turn: resume by appending the partial assistant turn as a new message.
	raw := []byte(`{"content":[{"type":"server_tool_use","id":"su_1","name":"web_fetch"}],"stop_reason":"pause_turn"}`)
	next, err = v.Continue(prev, raw)
	if err != nil {
		t.Fatalf("Continue error: %v", err)
	}
	nm, ok := next.(map[string]any)
	if !ok {
		t.Fatalf("resumed body is %T, want map[string]any", next)
	}
	msgs, ok := nm["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("resumed messages = %v, want 2 (user + assistant partial)", nm["messages"])
	}
	if last := msgs[1].(map[string]any); last["role"] != "assistant" {
		t.Errorf("appended message role = %v, want assistant", last["role"])
	}
	// The original body must not be mutated — the driver records each round's body.
	if orig := prev["messages"].([]any); len(orig) != 1 {
		t.Errorf("Continue mutated the previous body's messages (now %d), want it left at 1", len(orig))
	}
}

// ---------------------------------------------------------------------------
// askVendor — the shared driver. The behaviour that matters most is the
// pause-turn rule: sum the cost and stitch the answer across every round.
// ---------------------------------------------------------------------------

func TestAskVendorSingleTurn(t *testing.T) {
	post, bodies, endpoints := recordingPoster(
		`{"output_text":"the heading is X","usage":{"input_tokens":10,"output_tokens":2}}`,
	)
	got, usage, err := askVendor(context.Background(), openaiVendor{}, "KEY", "gpt-5-mini", "https://ex/p", "q", true, post)
	if err != nil {
		t.Fatalf("askVendor error: %v", err)
	}
	if got != "the heading is X" {
		t.Errorf("answer = %q", got)
	}
	if usage != (probe.Usage{InputTokens: 10, OutputTokens: 2, Rounds: 1}) {
		t.Errorf("usage = %+v (a normal single turn is Rounds:1)", usage)
	}
	if len(*bodies) != 1 {
		t.Errorf("posted %d times, want 1", len(*bodies))
	}
	if (*endpoints)[0] != "https://api.openai.com/v1/responses" {
		t.Errorf("posted to %q", (*endpoints)[0])
	}
}

// Every reply is kept whole (-replyLog): one line per POST with the vendor address,
// model, page and round; a failed POST is kept too; the key never reaches the file.
func TestAskVendorKeepsRawReplies(t *testing.T) {
	old := rawOutPath
	rawOutPath = filepath.Join(t.TempDir(), "raw.jsonl")
	defer func() { rawOutPath = old }()

	reply := `{"output_text":"the heading is X","usage":{"input_tokens":10,"output_tokens":2},"extra":{"kept":true}}`
	post, _, _ := recordingPoster(reply)
	if _, _, err := askVendor(context.Background(), openaiVendor{}, "SECRET-KEY", "gpt-5-mini", "https://ex/p", "q", true, post); err != nil {
		t.Fatal(err)
	}
	failing := func(context.Context, string, map[string]string, any) ([]byte, error) {
		return []byte("upstream said no"), fmt.Errorf("HTTP 502")
	}
	askVendor(context.Background(), openaiVendor{}, "SECRET-KEY", "gpt-5-mini", "https://ex/q", "q", true, failing)

	data, err := os.ReadFile(rawOutPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SECRET-KEY") {
		t.Fatal("the API key reached the raw-reply file")
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (one per POST), got %d:\n%s", len(lines), data)
	}
	var ok, bad RawReply
	if err := json.Unmarshal([]byte(lines[0]), &ok); err != nil {
		t.Fatal(err)
	}
	if ok.Endpoint != "https://api.openai.com/v1/responses" || ok.Model != "gpt-5-mini" || ok.URL != "https://ex/p" || ok.Round != 1 || ok.TSUnix == 0 {
		t.Errorf("first line labels wrong: %+v", ok)
	}
	var back map[string]any
	if err := json.Unmarshal(ok.Raw, &back); err != nil || back["extra"] == nil {
		t.Errorf("the reply must be kept whole, including fields the runner ignores: %s", ok.Raw)
	}
	if err := json.Unmarshal([]byte(lines[1]), &bad); err != nil {
		t.Fatal(err)
	}
	if bad.Err != "HTTP 502" || bad.RawText != "upstream said no" || bad.URL != "https://ex/q" {
		t.Errorf("a failed POST must be kept with its error and body: %+v", bad)
	}
}

// The pause-turn rule, end to end: the driver resumes the paused turn, and the
// returned cost is the SUM of both rounds while the answer is stitched across
// them (the final round carries the text here, as in practice).
func TestAskVendorAnthropicSumsUsageAndStitchesAnswer(t *testing.T) {
	post, bodies, _ := recordingPoster(
		`{"content":[{"type":"server_tool_use","id":"su_1","name":"web_fetch"}],"stop_reason":"pause_turn","usage":{"input_tokens":100,"output_tokens":10}}`,
		`{"content":[{"type":"text","text":"the code is TRUNC-abc123"}],"stop_reason":"end_turn","usage":{"input_tokens":50,"output_tokens":20}}`,
	)
	got, usage, err := askVendor(context.Background(), anthropicVendor{}, "KEY", "claude-sonnet-5", "https://ex/p", "q", true, post)
	if err != nil {
		t.Fatalf("askVendor error: %v", err)
	}
	if !strings.Contains(got, "TRUNC-abc123") {
		t.Errorf("final answer not returned: %q", got)
	}
	// 100+50 in, 10+20 out — a paused turn bills each round separately.
	if want := (probe.Usage{InputTokens: 150, OutputTokens: 30, Rounds: 2}); usage != want {
		t.Errorf("usage = %+v, want %+v (cost summed across rounds, Rounds counts the POSTs)", usage, want)
	}
	if usage.Rounds != 2 {
		t.Errorf("Rounds = %d, want 2 (one pause + one resume)", usage.Rounds)
	}
	if len(*bodies) != 2 {
		t.Fatalf("posted %d times, want 2 (one pause + one resume)", len(*bodies))
	}
	// The resume must carry the round-1 partial turn back as an assistant message.
	msgs, ok := (*bodies)[1]["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("resume messages = %v, want 2", (*bodies)[1]["messages"])
	}
	if msgs[1].(map[string]any)["role"] != "assistant" {
		t.Error("resume did not append the assistant partial turn")
	}
}

// A turn can pause more than once. This path runs rarely in the wild, so it is
// pinned hard: every round's cost is summed, every round's text is stitched in
// order, each resume appends exactly one assistant turn, and the resent turns
// carry the web_fetch_tool_result blocks VERBATIM — that carry-over is the whole
// reason we resend, since it is how the server resumes where it left off.
func TestAskVendorAnthropicMultiRoundPause(t *testing.T) {
	post, bodies, _ := recordingPoster(
		`{"content":[{"type":"text","text":"A "},{"type":"server_tool_use","id":"su_1","name":"web_fetch"},{"type":"web_fetch_tool_result","tool_use_id":"su_1"}],"stop_reason":"pause_turn","usage":{"input_tokens":100,"output_tokens":1}}`,
		`{"content":[{"type":"text","text":"B "},{"type":"server_tool_use","id":"su_2","name":"web_fetch"}],"stop_reason":"pause_turn","usage":{"input_tokens":200,"output_tokens":2}}`,
		`{"content":[{"type":"text","text":"C-final"}],"stop_reason":"end_turn","usage":{"input_tokens":300,"output_tokens":3}}`,
	)
	got, usage, err := askVendor(context.Background(), anthropicVendor{}, "KEY", "claude-sonnet-5", "https://ex/p", "q", true, post)
	if err != nil {
		t.Fatalf("askVendor error: %v", err)
	}
	if len(*bodies) != 3 {
		t.Fatalf("posted %d times, want 3 (two pauses + one end)", len(*bodies))
	}
	if got != "A B C-final" {
		t.Errorf("answer = %q, want %q (text must stitch across every round, in order)", got, "A B C-final")
	}
	if want := (probe.Usage{InputTokens: 600, OutputTokens: 6, Rounds: 3}); usage != want {
		t.Errorf("usage = %+v, want %+v (summed across all three rounds)", usage, want)
	}
	if usage.Rounds != 3 {
		t.Errorf("Rounds = %d, want 3 (two pauses + one end)", usage.Rounds)
	}
	// Round 2 resends user + the round-1 assistant turn; round 3 adds the round-2
	// assistant turn on top.
	if n := len((*bodies)[1]["messages"].([]any)); n != 2 {
		t.Errorf("round-2 request has %d messages, want 2", n)
	}
	msgs3 := (*bodies)[2]["messages"].([]any)
	if len(msgs3) != 3 {
		t.Fatalf("round-3 request has %d messages, want 3", len(msgs3))
	}
	// The resent assistant turns must carry both prior partials, including the
	// web_fetch_tool_result block, verbatim.
	blob, _ := json.Marshal(msgs3)
	for _, must := range []string{"web_fetch_tool_result", "su_1", "su_2"} {
		if !strings.Contains(string(blob), must) {
			t.Errorf("resume dropped %q; the server cannot resume without the prior turns verbatim", must)
		}
	}
}

// Text that arrives BEFORE the pause must not be dropped — stitching across rounds
// is the whole reason "read the final response only" was rejected.
func TestAskVendorAnthropicStitchesTextAcrossRounds(t *testing.T) {
	post, _, _ := recordingPoster(
		`{"content":[{"type":"text","text":"partial before pause. "}],"stop_reason":"pause_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"content":[{"type":"text","text":"final after resume."}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
	)
	got, _, err := askVendor(context.Background(), anthropicVendor{}, "KEY", "claude-sonnet-5", "https://ex/p", "q", true, post)
	if err != nil {
		t.Fatalf("askVendor error: %v", err)
	}
	if !strings.Contains(got, "partial before pause.") || !strings.Contains(got, "final after resume.") {
		t.Errorf("answer dropped a round's text: %q", got)
	}
}

// A refusal is an error, not a silent empty answer. The driver must actually POST
// once and read the refusal — asserting the post count keeps this from passing
// merely because some earlier step errored.
func TestAskVendorAnthropicRefusal(t *testing.T) {
	post, bodies, _ := recordingPoster(`{"content":[],"stop_reason":"refusal"}`)
	_, _, err := askVendor(context.Background(), anthropicVendor{}, "KEY", "claude-sonnet-5", "https://ex/p", "q", true, post)
	if err == nil {
		t.Error("a refusal must return an error")
	}
	if len(*bodies) != 1 {
		t.Errorf("driver posted %d times, want 1 (it must reach the POST to see the refusal)", len(*bodies))
	}
}

// An endless pause must stop at the continuation cap rather than loop forever.
func TestAskVendorCapsContinuations(t *testing.T) {
	// More paused rounds than the cap allows.
	replies := make([]string, maxPauseContinuations+3)
	for i := range replies {
		replies[i] = `{"content":[{"type":"server_tool_use","id":"s","name":"web_fetch"}],"stop_reason":"pause_turn","usage":{"input_tokens":1,"output_tokens":0}}`
	}
	post, bodies, _ := recordingPoster(replies...)
	_, _, err := askVendor(context.Background(), anthropicVendor{}, "KEY", "claude-sonnet-5", "https://ex/p", "q", true, post)
	if err == nil {
		t.Error("hitting the continuation cap without an end_turn must report an error")
	}
	// It must actually loop (more than one post) and then stop at the cap — not
	// bail before ever posting, and not run away.
	if n := len(*bodies); n < 2 || n > maxPauseContinuations+1 {
		t.Errorf("posted %d times, want between 2 and %d (it must loop, then cap out)", n, maxPauseContinuations+1)
	}
}

// A transport (HTTP) error is returned as-is — the driver never hides a failed
// round. The driver must reach the POST and surface that exact error, so the test
// checks both, not just that "some error" came back.
func TestAskVendorReturnsPostError(t *testing.T) {
	called := 0
	boom := func(ctx context.Context, endpoint string, headers map[string]string, body any) ([]byte, error) {
		called++
		return nil, context.DeadlineExceeded
	}
	_, _, err := askVendor(context.Background(), openaiVendor{}, "KEY", "gpt-5-mini", "https://ex/p", "q", true, boom)
	if called != 1 {
		t.Errorf("driver posted %d times, want 1 (it must reach the POST)", called)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want the POST error surfaced, got %v", err)
	}
}
