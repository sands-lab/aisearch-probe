package main

import (
	"strings"
	"testing"

	"aisearch-probe/internal/probe"
)

// The guard must trip on EITHER a runaway output (the Claude×docx case) or a runaway
// input (the Anthropic PDF pause_turn bomb), and leave a normal call alone.
func TestAnswerIsAnomalous(t *testing.T) {
	cases := []struct {
		name string
		u    probe.Usage
		want bool
	}{
		{"normal call", probe.Usage{InputTokens: 4000, OutputTokens: 1200}, false},
		{"runaway output (docx-style)", probe.Usage{InputTokens: 9200, OutputTokens: 23000}, true},
		{"runaway input (pdf bomb)", probe.Usage{InputTokens: 512000, OutputTokens: 200}, true},
		{"right at the output edge is not over", probe.Usage{InputTokens: 100, OutputTokens: anomalyOutputTokens}, false},
		{"just over the output edge trips", probe.Usage{InputTokens: 100, OutputTokens: anomalyOutputTokens + 1}, true},
	}
	for _, c := range cases {
		if got := answerIsAnomalous(c.u); got != c.want {
			t.Errorf("%s: answerIsAnomalous = %v, want %v", c.name, got, c.want)
		}
	}
}

// logAnswer keeps the full text on an anomalous call (forensics) and the short snippet
// otherwise — the whole reason the docx forensics were lost in exp A.
func TestLogAnswer(t *testing.T) {
	long := strings.Repeat("x", 5000)

	// Normal usage → truncated to the cap.
	got := logAnswer(long, probe.Usage{InputTokens: 3000, OutputTokens: 1000}, 500)
	if len([]rune(got)) > 501 { // 500 + the ellipsis rune
		t.Errorf("normal call should be truncated near 500, got %d chars", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("a truncated answer should carry the ellipsis marker")
	}

	// Anomalous usage → full text kept, no truncation.
	full := logAnswer(long, probe.Usage{InputTokens: 9200, OutputTokens: 23000}, 500)
	if full != long {
		t.Errorf("anomalous call must keep the FULL answer; got %d chars, want %d", len(full), len(long))
	}

	// A short answer is returned whole regardless (nothing to cut).
	if s := logAnswer("hi", probe.Usage{InputTokens: 1, OutputTokens: 1}, 500); s != "hi" {
		t.Errorf("short answer should pass through unchanged, got %q", s)
	}
}
