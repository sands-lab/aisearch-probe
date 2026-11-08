package probe

import (
	gobytes "bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// goldenResult is a fully-populated record used to prove nothing is lost on the
// way to disk and back. Extra deliberately holds a string, a float and a bool,
// the three JSON scalar shapes that survive a round-trip unchanged.
func goldenResult() ResultRecord {
	return ResultRecord{
		RunID:         "run20260922-101010-abcde",
		EventID:       "run20260922-101010-abcde-ev1",
		Experiment:    "negcache",
		EpisodeID:     "run20260922-101010-abcde-ep1",
		Step:          "reask",
		Surface:       "api",
		Vendor:        "or:anthropic",
		Model:         "anthropic/claude-sonnet-5",
		ToolForced:    true,
		URL:           "https://probe.attentionhub.org/fault/heal/60?probe=tok",
		Token:         "probe=tok",
		SentUTC:       "2026-09-22T10:10:10Z",
		SentUnix:      1789999810.5,
		ElapsedS:      3.25,
		Question:      "What is the study reference code at the bottom of the page?",
		AnswerText:    "I couldn't fetch that page — the request failed with 503.",
		MarkerPresent: false,
		Marker:        "RCP-2026-08",
		Error:         "",
		Extra: map[string]any{
			"gap":          "15m",
			"heal_window":  "1m0s",
			"heal_seconds": float64(60),
			"armed":        true,
		},
	}
}

// A golden record must survive writer -> reader unchanged.
func TestResultRecordRoundTrips(t *testing.T) {
	want := goldenResult()

	var buf gobytes.Buffer
	if err := WriteResults(&buf, []ResultRecord{want}); err != nil {
		t.Fatalf("WriteResults: %v", err)
	}
	got, err := ReadResults(&buf)
	if err != nil {
		t.Fatalf("ReadResults: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 record back, got %d", len(got))
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("round-trip changed the record:\n want %+v\n got  %+v", want, got[0])
	}
}

// The field that got silently dropped from the paired negcache runner. A
// realistic reask row must carry the model's own words to disk, under the
// standard name, and read them back verbatim.
func TestResultCarriesAnswerText(t *testing.T) {
	rec := goldenResult()
	rec.AnswerText = `I attempted to fetch the page but the tool returned an error (503 Service Unavailable). reference code: <unavailable>`

	var buf gobytes.Buffer
	if err := WriteResults(&buf, []ResultRecord{rec}); err != nil {
		t.Fatalf("WriteResults: %v", err)
	}
	onDisk := buf.String()
	if !strings.Contains(onDisk, `"answer_text"`) {
		t.Fatalf("serialized record has no answer_text field:\n%s", onDisk)
	}

	got, err := ReadResults(strings.NewReader(onDisk))
	if err != nil {
		t.Fatalf("ReadResults: %v", err)
	}
	if got[0].AnswerText != rec.AnswerText {
		t.Fatalf("answer_text not preserved:\n want %q\n got  %q", rec.AnswerText, got[0].AnswerText)
	}
}

// Validate names the fields a record cannot be without. It records facts, not
// verdicts, so there is no fetch count or verdict to require here.
func TestResultValidateRequiresIdentity(t *testing.T) {
	ok := goldenResult()
	if err := ok.Validate(); err != nil {
		t.Fatalf("a complete record should validate, got: %v", err)
	}

	cases := map[string]func(*ResultRecord){
		"run_id":     func(r *ResultRecord) { r.RunID = "" },
		"event_id":   func(r *ResultRecord) { r.EventID = "" },
		"experiment": func(r *ResultRecord) { r.Experiment = "" },
		"surface":    func(r *ResultRecord) { r.Surface = "" },
		"vendor":     func(r *ResultRecord) { r.Vendor = "" },
		"url":        func(r *ResultRecord) { r.URL = "" },
		"sent_utc":   func(r *ResultRecord) { r.SentUTC = "" },
	}
	for field, break_ := range cases {
		rec := goldenResult()
		break_(&rec)
		err := rec.Validate()
		if err == nil {
			t.Errorf("missing %s should fail validation, but it passed", field)
			continue
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error for missing %s should name the field, got: %v", field, err)
		}
	}
}

// Surface must be one of the two known values, so an api/web-app split query
// can never silently miss rows with a typo'd surface.
func TestResultValidateChecksSurface(t *testing.T) {
	rec := goldenResult()
	rec.Surface = "webapp" // not "web-app"
	if err := rec.Validate(); err == nil {
		t.Fatalf("an unknown surface should fail validation")
	}
	for _, s := range []string{"api", "web-app"} {
		rec := goldenResult()
		rec.Surface = s
		if err := rec.Validate(); err != nil {
			t.Errorf("surface %q should be valid, got: %v", s, err)
		}
	}
}

// The dataset is a folder of per-run files read by one glob. Regeneration must
// be deterministic: the same raw files in must give the same ordered records
// out, no matter what order the directory hands back the filenames.
func TestReadResultsDirIsDeterministic(t *testing.T) {
	dir := t.TempDir()

	early := goldenResult()
	early.EventID = "ev-early"
	early.SentUnix = 100

	mid := goldenResult()
	mid.EventID = "ev-mid"
	mid.SentUnix = 200

	late := goldenResult()
	late.EventID = "ev-late"
	late.SentUnix = 300

	// Two runs, written to two files, in an order that does NOT match time.
	writeRunFile(t, filepath.Join(dir, "negcache-runB.jsonl"), []ResultRecord{late, mid})
	writeRunFile(t, filepath.Join(dir, "negcache-runA.jsonl"), []ResultRecord{early})

	got, err := ReadResultsDir(dir)
	if err != nil {
		t.Fatalf("ReadResultsDir: %v", err)
	}
	order := []string{}
	for _, r := range got {
		order = append(order, r.EventID)
	}
	want := []string{"ev-early", "ev-mid", "ev-late"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("records not in deterministic time order:\n want %v\n got  %v", want, order)
	}

	// Same inputs, read again -> identical result.
	again, err := ReadResultsDir(dir)
	if err != nil {
		t.Fatalf("ReadResultsDir (again): %v", err)
	}
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("two reads of the same folder disagreed")
	}
}

func writeRunFile(t *testing.T, path string, recs []ResultRecord) {
	t.Helper()
	var buf gobytes.Buffer
	if err := WriteResults(&buf, recs); err != nil {
		t.Fatalf("WriteResults: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
