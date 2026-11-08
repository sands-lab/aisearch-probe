package probe

// The standard result record: one faithful line per ASK.
//
// Every runner that asks a model something and reads what came back writes this
// same shape, whether the ask went through the developer interface (the API) or
// a chat product driven by hand in a browser. Before this existed each runner
// invented its own field names for the same ideas — reask_answer, broken_marker,
// a_reask_fetches — so the outputs were not one dataset and, worse, a rebuild of
// one runner once dropped reask_answer, the model's own words, which was the
// evidence for a finding. See doc/standard-record-interface-handover.md.
//
// Two rules make that impossible to repeat:
//
//   - A record holds FACTS, never a verdict. "cached", "diverged",
//     "negative-caching" are judgments; they are computed later from these
//     records, so the rule that draws them can change and be re-run over old
//     data. This is the same discipline the raw server log (probe.jsonl) already
//     follows: it records requests, analyze.go draws the conclusions.
//   - The count of fetches that reached our server is NOT stored here. It is
//     derived at analysis time by joining Token against probe.jsonl over a time
//     window (correlate.go), which is both cleaner and more correct: a crawler
//     can fetch minutes late, and a number the runner snapshots at ask time
//     would miss it. A runner may keep a live snapshot to warn on a broken run,
//     but the authoritative number always comes from the join.
//
// On disk the record is newline-delimited JSON, one file per run under a results
// folder. The "dataset" is that folder read with one glob — DuckDB reads it
// directly with read_json_auto('logs/results/*.jsonl'), so there is no single
// growing file to append to and nothing to regenerate.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// ResultRecord is one ask and what came back. Field names are spelled out and
// experiment-neutral, so a reader coming to the folder cold does not have to
// know which experiment wrote a row to read it.
type ResultRecord struct {
	// Identity and grouping.
	RunID   string `json:"run_id"`   // groups every ask from one invocation
	EventID string `json:"event_id"` // unique id for this one ask
	// Experiment names the study, e.g. "negcache", "sharing", "retention". It is
	// a column, not a filename convention: analysis filters on it.
	Experiment string `json:"experiment"`
	// EpisodeID ties together the asks that form one logical test (the two asks
	// of a warm-then-reask pair share it). A single-ask experiment may leave it
	// blank or set it equal to EventID.
	EpisodeID string `json:"episode_id,omitempty"`
	// Step is this ask's role within its episode: "warm", "reask", "broken",
	// "healthy", "control". The log never interprets it; analysis does.
	Step string `json:"step,omitempty"`

	// Who was asked.
	Surface    string `json:"surface"` // "api" or "web-app"
	Vendor     string `json:"vendor"`  // e.g. "or:anthropic"
	Model      string `json:"model,omitempty"`
	ToolForced bool   `json:"tool_forced"` // was the browse tool forced on

	// What was asked.
	URL   string `json:"url"`
	Token string `json:"token,omitempty"` // the probe token in the URL; join key to probe.jsonl

	SentUTC  string  `json:"sent_utc"`  // when the ask was sent, RFC3339 UTC
	SentUnix float64 `json:"sent_unix"` // same instant, for sorting/joining
	ElapsedS float64 `json:"elapsed_s,omitempty"`

	Question string `json:"question,omitempty"`

	// What came back.
	AnswerText    string `json:"answer_text"`      // the raw model reply, verbatim; never omit
	MarkerPresent bool   `json:"marker_present"`   // did the expected code appear in the answer
	Marker        string `json:"marker,omitempty"` // the code we looked for
	Error         string `json:"error,omitempty"`  // transport/client error if the ask itself failed

	// Cost of the call, as raw token usage from the provider's own usage object.
	// FACTS ONLY — dollars are DERIVED later from PROVIDERS.md prices (which carry
	// dates and change), the same discipline as the fetch count and the verdict.
	// input_tokens also gauges how much fetched content entered the model context.
	// Zero when the provider reported no usage (e.g. an errored call).
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`

	// Rounds is how many request rounds (POSTs) the call took: 1 normally, >1 when a
	// server-tool turn paused and resumed (Anthropic pause_turn), each resume re-sending
	// and re-billing the context. The direct measure of the pause_turn amplification
	// behind the PDF cost blow-up — a fact, not inferred from inflated input tokens.
	Rounds int `json:"rounds,omitempty"`

	// Extra carries experiment-specific facts that do not deserve a shared column
	// (gap, heal_window, edition_seen, robots_armed, a non-authoritative
	// fetches_snapshot for debugging). Facts only, never a verdict.
	Extra map[string]any `json:"extra,omitempty"`
}

// knownSurfaces is the closed set: an api/web-app split query must never miss
// rows because a surface was misspelled.
var knownSurfaces = map[string]bool{"api": true, "web-app": true}

// Validate reports the first way a record is unfit to store. It checks that the
// facts needed to find and join a row are present, and that surface is a known
// value. It does NOT require an answer (an errored ask has none) or any derived
// field.
func (r ResultRecord) Validate() error {
	missing := ""
	switch {
	case r.RunID == "":
		missing = "run_id"
	case r.EventID == "":
		missing = "event_id"
	case r.Experiment == "":
		missing = "experiment"
	case r.Surface == "":
		missing = "surface"
	case r.Vendor == "":
		missing = "vendor"
	case r.URL == "":
		missing = "url"
	case r.SentUTC == "":
		missing = "sent_utc"
	}
	if missing != "" {
		return fmt.Errorf("result record missing required field %q", missing)
	}
	if !knownSurfaces[r.Surface] {
		return fmt.Errorf("result record has unknown surface %q (want api or web-app)", r.Surface)
	}
	return nil
}

// WriteResults writes records as newline-delimited JSON, the on-disk form every
// runner shares.
func WriteResults(w io.Writer, recs []ResultRecord) error {
	enc := json.NewEncoder(w)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// ReadResults reads newline-delimited result records from one stream.
func ReadResults(r io.Reader) ([]ResultRecord, error) {
	dec := json.NewDecoder(r)
	out := []ResultRecord{}
	for {
		var rec ResultRecord
		err := dec.Decode(&rec)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// AppendResultsToFile writes records to a per-run file, creating it or appending
// to it. One run writes one file; the folder of files is the dataset.
func AppendResultsToFile(path string, recs []ResultRecord) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return WriteResults(f, recs)
}

// ReadResultsDir reads every *.jsonl file in a folder and returns the records in
// one deterministic order: by send time, then by event id to break ties. This
// is the Go-side equivalent of the DuckDB glob, used by analysis and tests, so
// regenerating a combined view from the same files always gives the same rows in
// the same order.
func ReadResultsDir(dir string) ([]ResultRecord, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	out := []ResultRecord{}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		recs, err := ReadResults(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, recs...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SentUnix != out[j].SentUnix {
			return out[i].SentUnix < out[j].SentUnix
		}
		return out[i].EventID < out[j].EventID
	})
	return out, nil
}
