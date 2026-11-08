package probe

// Reading the multi-day retention run's saved state into the per-trial table.
//
// The retention run does not write an ordinary trial log; it keeps one JSON
// state file it rewrites after every tick, with one entry per scheduled check.
// This turns the checks that have actually fired into the same TrialRow shape as
// every other run, so the retention results sit in one table with the rest. It
// reads only the fields it needs, so the full state struct in
// trial/retentionrun.go can grow without touching this.

import (
	"encoding/json"
	"os"
)

type retentionState struct {
	Actions []retentionAction `json:"actions"`
}

type retentionAction struct {
	ID        string  `json:"id"`
	Arm       string  `json:"arm"`
	Provider  string  `json:"provider"`
	Model     string  `json:"model"`
	URL       string  `json:"url"`
	Status    string  `json:"status"`
	FiredAt   string  `json:"fired_at"`
	FiredUnix float64 `json:"fired_unix"`
	Marker    bool    `json:"marker"`
	Error     string  `json:"error"`
}

// RowsFromRetention reads the retention state file and returns one TrialRow per
// check that has fired, correlated against the request log so the fetcher name,
// the robots.txt read and the byte count are filled in. Checks still pending are
// skipped — they have no outcome yet.
func RowsFromRetention(statePath string, records []Record, windowSeconds float64, phase string) ([]TrialRow, error) {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return nil, err
	}
	var st retentionState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}

	var trials []Trial
	var models []string
	for _, a := range st.Actions {
		if a.Status != "done" {
			continue
		}
		trials = append(trials, Trial{
			TrialID:     a.ID,
			Arm:         a.Arm,
			Provider:    a.Provider,
			URL:         a.URL,
			SentAt:      a.FiredAt,
			SentUnix:    a.FiredUnix,
			FoundMarker: a.Marker,
			Error:       a.Error,
		})
		models = append(models, a.Model)
	}

	cors := Correlate(trials, records, windowSeconds)
	rows := make([]TrialRow, 0, len(cors))
	for i, c := range cors {
		rows = append(rows, RowFrom(c, records, windowSeconds, RowMeta{
			Phase:      phase,
			Surface:    "api",
			ToolForced: true,
			Vendor:     trials[i].Provider,
			Model:      models[i],
		}))
	}
	return rows, nil
}
