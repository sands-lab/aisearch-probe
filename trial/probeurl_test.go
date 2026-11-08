package main

import (
	"regexp"
	"strings"
	"testing"
)

// tidyToken is the one shape every code must come out in: "probe=" then only
// lowercase letters, digits and single hyphens, no hyphen at either end.
var tidyToken = regexp.MustCompile(`^probe=[a-z0-9]+(-[a-z0-9]+)*$`)

// The point of the cleanup: whatever messy identity a caller hands in, the code
// comes out in one tidy shape. These inputs are the real weird shapes seen in
// the logs — a route colon, a dotted model version, mixed case, a trailing
// colon, spaces and underscores.
func TestProbeTokenIsAlwaysTidy(t *testing.T) {
	inputs := []string{
		"or:x-ai",
		"google/gemini-3.1-flash-lite",
		"OpenAI-GPT-5-Mini:",
		"openai-gpt-5-mini-ctl1",
		"or:anthropic-b-run20260922-150405-ab7kq",
		"weird token__with  spaces",
		"trailing---dashes--",
	}
	for _, in := range inputs {
		got := probeToken(in)
		if !tidyToken.MatchString(got) {
			t.Errorf("probeToken(%q) = %q, not a tidy code", in, got)
		}
	}
}

// Concrete shapes, so the intent is pinned, not just the pattern.
func TestProbeTokenShapes(t *testing.T) {
	cases := map[string]string{
		"or:x-ai":                      "probe=or-x-ai",
		"google/gemini-3.1-flash-lite": "probe=google-gemini-3-1-flash-lite",
		"OpenAI-GPT-5-Mini:":           "probe=openai-gpt-5-mini",
	}
	for in, want := range cases {
		if got := probeToken(in); got != want {
			t.Errorf("probeToken(%q) = %q, want %q", in, got, want)
		}
	}
}

// The code is a join key matched as a whole substring of the URL, so the URL the
// caller sends must literally contain the token it was handed back.
func TestProbeURLForContainsItsOwnToken(t *testing.T) {
	url, token := probeURLFor("https://x.test/method.html", "or:x-ai-grok-4")
	if !strings.Contains(url, token) {
		t.Errorf("URL %q does not contain its token %q", url, token)
	}
	if !tidyToken.MatchString(token) {
		t.Errorf("token %q is not tidy", token)
	}
}

// Cleaning the shape must not merge codes that mean different things: the
// variations experiment relies on each variation carrying a distinct code so no
// two share a stored copy.
func TestTidyingKeepsDistinctCodesDistinct(t *testing.T) {
	if probeToken("or-openai-v1") == probeToken("or-openai-v2") {
		t.Error("two variations collapsed to the same code")
	}
	if probeToken("or:anthropic-b-run1") == probeToken("or:anthropic-h-run1") {
		t.Error("the broken and healthy arms collapsed to the same code")
	}
}
