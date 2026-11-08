package main

import (
	"fmt"
	"os"

	"aisearch-probe/internal/probe"
)

// A token-cost anomaly guard. Some calls bill far more than the task should cost —
// exp A caught Claude on docx billing 3k–23k OUTPUT tokens for a task that costs ~1k
// elsewhere (long/erratic generation, NOT pause_turn — its input was flat). The
// Anthropic PDF pause_turn bomb is the input-side version (one 8 KB PDF → 512k input).
// When a call trips either threshold we do two things: keep the FULL answer instead of
// the short snippet (so a runaway leaves forensics we can read later), and print a WARN
// so it is visible in the run log. Thresholds are absolute so they apply inline, per
// call, with no run-wide median needed; tune them as the size ladder (exp B) grows.
const (
	anomalyOutputTokens = 4000
	anomalyInputTokens  = 20000
)

// answerIsAnomalous reports whether a call's usage is a runaway worth full forensics.
func answerIsAnomalous(u probe.Usage) bool {
	return u.OutputTokens > anomalyOutputTokens || u.InputTokens > anomalyInputTokens
}

// logAnswer is how a runner stores a call's answer: the FULL text when the call's
// usage is anomalous (keep the evidence), otherwise the compact snippet at normalCap.
func logAnswer(ans string, u probe.Usage, normalCap int) string {
	if answerIsAnomalous(u) {
		return ans
	}
	return truncate(ans, normalCap)
}

// warnIfAnomalous prints a one-line WARN to stderr when a call's usage is a runaway,
// naming the label so the offending (format, provider) cell is obvious in the run log.
func warnIfAnomalous(label string, u probe.Usage) {
	if answerIsAnomalous(u) {
		fmt.Fprintf(os.Stderr, "WARN anomalous token cost: %s in=%d out=%d (kept full answer for forensics)\n",
			label, u.InputTokens, u.OutputTokens)
	}
}
