package main

// Shared emission of the standard result record (internal/probe.ResultRecord).
//
// Every runner in this package used to hand-roll its own JSON with its own field
// names for the same ideas — reask_answer here, broken_marker there — so the
// outputs were not one dataset, and a rebuild once dropped reask_answer, the
// model's own words, which was the evidence for a finding. This file is the one
// place a runner turns "I asked a page and read the reply" into a record, so no
// runner hand-rolls JSON again and the answer text can never be forgotten.
//
// The record holds FACTS only: what we sent, what came back, and the token that
// links it to the raw server log. It carries no verdict and no authoritative
// fetch count — those are DERIVED later by joining Token against probe.jsonl.
// See internal/probe/result.go.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"aisearch-probe/internal/probe"
)

// resultsDir is where per-run standard result files land, one file per run named
// <experiment>-<run_id>.jsonl. The folder IS the dataset: DuckDB reads it whole
// with read_json_auto('logs/results/*.jsonl'), so there is no single growing
// file. A test points this at a temp dir.
var resultsDir = "logs/results"

// askOutcome is the raw facts from one ask, before it becomes a record. It has
// no verdict. FetchesSnapshot is what the runner happened to see by the time it
// looked — kept for a live sanity warning and for debugging, never the answer to
// "how many fetches"; that comes from the probe.jsonl join at analysis time.
type askOutcome struct {
	URL, Token, Question string
	Answer               string
	ErrStr               string
	MarkerPresent        bool
	SentUnix             float64
	ElapsedS             float64
	FetchesSnapshot      int
	// Token usage = the call's cost (a fact; dollars derived later from
	// the vendors' prices). Every runner should set these from the provider's Usage.
	InputTokens  int
	OutputTokens int
	// Rounds is the number of request rounds the call took (1 normally; >1 is an
	// Anthropic pause_turn resume loop). Set from the provider Usage.
	Rounds int
}

// resultMeta is what the runner knows that a single ask does not: which study
// this is, how to name the party, and where this ask sits in its episode.
type resultMeta struct {
	RunID      string
	Experiment string
	EpisodeID  string
	Step       string // this ask's role: warm / reask / broken / healthy / control
	Surface    string // "api" for these runners; "web-app" for the browser console
	Vendor     string
	Model      string
	ToolForced bool
	Marker     string // the code looked for, so a reader knows what marker_present tested
}

// resultFromAsk builds one standard record from one ask. It copies facts only —
// notably the raw answer text — and never a verdict. extra carries
// experiment-specific facts (gap, heal_window, a labelled fetches snapshot).
func resultFromAsk(m resultMeta, eventID string, o askOutcome, extra map[string]any) probe.ResultRecord {
	return probe.ResultRecord{
		RunID:         m.RunID,
		EventID:       eventID,
		Experiment:    m.Experiment,
		EpisodeID:     m.EpisodeID,
		Step:          m.Step,
		Surface:       m.Surface,
		Vendor:        m.Vendor,
		Model:         m.Model,
		ToolForced:    m.ToolForced,
		URL:           o.URL,
		Token:         o.Token,
		SentUTC:       time.Unix(0, int64(o.SentUnix*1e9)).UTC().Format(time.RFC3339Nano),
		SentUnix:      o.SentUnix,
		ElapsedS:      o.ElapsedS,
		Question:      o.Question,
		AnswerText:    o.Answer,
		MarkerPresent: o.MarkerPresent,
		Marker:        m.Marker,
		Error:         o.ErrStr,
		InputTokens:   o.InputTokens,
		OutputTokens:  o.OutputTokens,
		Rounds:        o.Rounds,
		Extra:         extra,
	}
}

// emitResults validates every record, then appends them to this run's file. It
// validates FIRST so a malformed record fails the write loudly instead of
// quietly polluting the shared dataset.
func emitResults(experiment, runID string, recs []probe.ResultRecord) error {
	for _, r := range recs {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("refusing to write invalid result record: %w", err)
		}
	}
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(resultsDir, fmt.Sprintf("%s-%s.jsonl", experiment, runID))
	return probe.AppendResultsToFile(path, recs)
}
