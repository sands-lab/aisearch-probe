package main

import (
	"strings"
	"testing"

	"aisearch-probe/internal/probe"
)

// The shared builder must copy the model's own words to the record and produce a
// schema-valid record. This is the guard on the field a bespoke runner dropped.
func TestResultFromAskCarriesAnswer(t *testing.T) {
	m := resultMeta{
		RunID:      "run20260922-abcde",
		Experiment: "negcache-paired",
		EpisodeID:  "run20260922-abcde-or:anthropic",
		Step:       "broken",
		Surface:    "api",
		Vendor:     "or:anthropic",
		Model:      "anthropic/claude-sonnet-5",
		ToolForced: true,
		Marker:     "RCP-2026-08",
	}
	o := askOutcome{
		URL:             "https://probe.example.org/fault/heal/60?probe=tok",
		Token:           "probe=tok",
		Question:        "Caching check 15m. What is the study reference code on this page?",
		Answer:          "I attempted to fetch the page but the tool returned a 503 error. reference code: <unavailable>",
		MarkerPresent:   false,
		SentUnix:        1789999810.5,
		ElapsedS:        2.1,
		FetchesSnapshot: 0,
		InputTokens:     262144,
		OutputTokens:    48,
	}

	rec := resultFromAsk(m, "run20260922-abcde-anth-broken-15m", o, map[string]any{
		"gap":              "15m",
		"fetches_snapshot": float64(o.FetchesSnapshot),
	})

	if err := rec.Validate(); err != nil {
		t.Fatalf("built record should be schema-valid, got: %v", err)
	}
	if rec.AnswerText != o.Answer {
		t.Fatalf("answer_text not carried:\n want %q\n got  %q", o.Answer, rec.AnswerText)
	}
	if rec.EventID == "" || rec.Step != "broken" || rec.EpisodeID != m.EpisodeID {
		t.Fatalf("identity/grouping not set through: %+v", rec)
	}
	if rec.Extra["gap"] != "15m" {
		t.Fatalf("experiment-specific fact not carried in extra: %+v", rec.Extra)
	}
	// The call's cost (token usage) must ride through into the record.
	if rec.InputTokens != 262144 || rec.OutputTokens != 48 {
		t.Fatalf("token usage not carried: in=%d out=%d, want 262144/48", rec.InputTokens, rec.OutputTokens)
	}
}

// emitResults writes a schema-valid run file to the results folder that
// ReadResultsDir can read back — the runner-to-dataset path end to end.
func TestEmitResultsWritesReadableRunFile(t *testing.T) {
	dir := t.TempDir()
	old := resultsDir
	resultsDir = dir
	defer func() { resultsDir = old }()

	m := resultMeta{
		RunID: "runX", Experiment: "negcache-paired", EpisodeID: "runX-ep",
		Step: "healthy", Surface: "api", Vendor: "or:openai", Model: "openai/gpt-5-mini",
		ToolForced: true, Marker: "RCP-2026-08",
	}
	o := askOutcome{URL: "https://h/page/x?probe=t", Token: "probe=t", Answer: "status line: live", MarkerPresent: true, SentUnix: 200}
	rec := resultFromAsk(m, "runX-oai-healthy-warm", o, map[string]any{"gap": "warm"})

	if err := emitResults(m.Experiment, m.RunID, []probe.ResultRecord{rec}); err != nil {
		t.Fatalf("emitResults: %v", err)
	}

	got, err := probe.ReadResultsDir(dir)
	if err != nil {
		t.Fatalf("ReadResultsDir: %v", err)
	}
	if len(got) != 1 || got[0].EventID != "runX-oai-healthy-warm" || got[0].AnswerText != o.Answer {
		t.Fatalf("run file did not round-trip: %+v", got)
	}
}

// A record missing a required field must be refused, so a coding slip in a runner
// fails loudly instead of writing a broken row into the shared dataset.
func TestEmitResultsRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	old := resultsDir
	resultsDir = dir
	defer func() { resultsDir = old }()

	bad := probe.ResultRecord{RunID: "r", Experiment: "x", Surface: "api", Vendor: "v", URL: "u", SentUTC: "t"} // no event_id
	err := emitResults("x", "r", []probe.ResultRecord{bad})
	if err == nil {
		t.Fatalf("expected emitResults to reject a record with no event_id")
	}
	if !strings.Contains(err.Error(), "event_id") {
		t.Fatalf("error should name the missing field, got: %v", err)
	}
}
