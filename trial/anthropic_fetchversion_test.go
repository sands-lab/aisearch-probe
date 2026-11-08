package main

import "testing"

// The web_fetch VERSION override drives the dynamic-filtering A/B test: both calls on the
// direct key, same PDF, differing ONLY in the tool version — web_fetch_20250910 (no dynamic
// filtering) vs web_fetch_20260209 (has it). The knob must leave the default untouched, apply
// the chosen version under withAnthropicFetchVersion, and reset after.
func TestAnthropicFetchVersionOverride(t *testing.T) {
	toolType := func() any {
		b, err := anthropicVendor{}.BuildRequest("m", "http://x", "q", false)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		return b.(map[string]any)["tools"].([]map[string]any)[0]["type"]
	}

	if toolType() != "web_fetch_20260209" {
		t.Errorf("default tool type = %v, want web_fetch_20260209", toolType())
	}

	withAnthropicFetchVersion("web_fetch_20250910", func() {
		if toolType() != "web_fetch_20250910" {
			t.Errorf("override tool type = %v, want web_fetch_20250910", toolType())
		}
	})

	if toolType() != "web_fetch_20260209" {
		t.Errorf("version override leaked after withAnthropicFetchVersion (got %v)", toolType())
	}
}
