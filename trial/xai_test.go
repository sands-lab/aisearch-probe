package main

import (
	"os"
	"strings"
	"testing"
)

// clearProviderEnv wipes every provider credential so a test starts from a known
// empty environment and sets only what it means to.
func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "PERPLEXITY_API_KEY",
		"OPENROUTER_API_KEY", "GEMINI_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS",
		"XAI_API_KEY",
	} {
		t.Setenv(v, "")
	}
}

// The direct xAI provider must be a first-class account, distinct from the
// OpenRouter x-ai route: its Name is exactly "x-ai" so -sharingApi-firstCaller x-ai
// resolves, and Tag() carries the model so it never shares a stored copy with
// the OpenRouter route in the sharing test.
func TestXAIProviderRegistration(t *testing.T) {
	clearProviderEnv(t)

	find := func(ps []Caller, name string) *Caller {
		for i := range ps {
			if ps[i].Name == name {
				return &ps[i]
			}
		}
		return nil
	}

	// Without the key, x-ai is not in the active set.
	if p := find(callers("m", "m", "m", "grok-4.3", nil), "x-ai"); p == nil {
		t.Fatal("x-ai provider missing from the list entirely")
	} else if p.Enabled {
		t.Error("x-ai must be disabled without XAI_API_KEY")
	}

	// With the key, it is enabled and reads its own credential.
	t.Setenv("XAI_API_KEY", "k")
	p := find(callers("m", "m", "m", "grok-4.3", nil), "x-ai")
	if p == nil || !p.Enabled {
		t.Fatal("x-ai should be enabled when XAI_API_KEY is set")
	}
	if p.EnvKey != "XAI_API_KEY" {
		t.Errorf("x-ai reads %q, want XAI_API_KEY", p.EnvKey)
	}
	if p.Model != "grok-4.3" {
		t.Errorf("x-ai model = %q, want the -xaiModel value grok-4.3", p.Model)
	}
	if p.Ask == nil {
		t.Error("x-ai has no Ask function wired")
	}

	// The direct route and the OpenRouter route must be addressable apart in the
	// sharing test: same vendor, different Tag(). If they collapsed to one Tag,
	// the second caller would be served the first's stored copy and its silence
	// would read as caching it never did.
	or := openRouterCallers([]string{"x-ai/grok-4.3"})
	if len(or) != 1 {
		t.Fatalf("expected one or:x-ai provider, got %d", len(or))
	}
	if p.Name == or[0].Name {
		t.Errorf("direct and OpenRouter routes share a Name %q — the sharing test cannot tell them apart", p.Name)
	}
	if p.Tag() == or[0].Tag() {
		t.Errorf("direct and OpenRouter routes share a Tag %q — they would share a stored copy", p.Tag())
	}
}

// The env loader's known-key list must recognise XAI_API_KEY, or the key sits in
// the file doing nothing and the "no credentials" message lies about what it
// looked for.
func TestEnvLoaderRecognisesXAIKey(t *testing.T) {
	clearProviderEnv(t)

	if strings.Contains(strings.Join(credentialSummary(), " "), "XAI_API_KEY") {
		t.Fatal("XAI_API_KEY reported present when it is not set")
	}
	t.Setenv("XAI_API_KEY", "k")
	found := false
	for _, name := range credentialSummary() {
		if name == "XAI_API_KEY" {
			found = true
		}
	}
	if !found {
		t.Error("credentialSummary does not recognise XAI_API_KEY — the key would be read but never reported")
	}

	// The no-credentials help text must also name the key it looks for, so an
	// operator who set only XAI_API_KEY is not told it was never among them.
	if !strings.Contains(noCredentialsText(), "XAI_API_KEY") {
		t.Error("the no-credentials help text does not mention XAI_API_KEY")
	}
}

// A guard against the wiring silently reverting to search-only or the wrong
// endpoint. The direct xAI provider must call xAI's own Responses API and use
// the live web_search tool — the one that fetches a named URL so a request
// reaches our server — not an index-only search or someone else's endpoint.
func TestXAIUsesResponsesAPIWithWebSearch(t *testing.T) {
	src, err := os.ReadFile("xai.go")
	if err != nil {
		t.Fatalf("read xai.go: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, "https://api.x.ai/v1/responses") {
		t.Error(`xai.go does not call xAI's own Responses API endpoint (https://api.x.ai/v1/responses)`)
	}
	if !strings.Contains(body, `"web_search"`) {
		t.Error(`xai.go does not declare the web_search tool — the live fetch tool that reaches our server`)
	}
}
