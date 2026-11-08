package probe

// The per-trial results table.
//
// The rest of this package answers "how did a bot behave across many visits."
// This file answers a flatter, more portable question: "for each single trial
// we ran, what happened?" — one row per trial, the same fixed columns whether
// the trial went through the developer interface (the API) or, later, through a
// chat product driven by hand in a browser.
//
// It is DERIVED, read-only. It computes nothing new about the world; it reshapes
// what Correlate already worked out (plus the retention run's saved state) into
// one table that the web-app phase and the API runs can both write into. That
// shared shape is the whole point: a scripted API call and a hand-typed web
// prompt meet here, so the two stay comparable no matter how the prompt was
// sent. See doc/web-app-automation-plan.md.
//
// The row is newline-delimited JSON with optional fields, so a new column added
// later costs nothing: old rows simply lack it and read back as the zero value.
// The compact table shows the core columns; the JSONL carries all of them. The
// raw request firehose stays in probe.jsonl — this is the outcome view on top,
// not a second copy of every header.

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// TrialRow is one trial's outcome. Field names are spelled out on disk so a
// reader coming to logs/trials.jsonl cold does not have to decode them.
type TrialRow struct {
	TrialID string `json:"trial_id"`
	// RunID groups the trials from one run, so a run and its states can be joined
	// (e.g. in DuckDB). Optional: the API-side writer leaves it blank today.
	RunID    string  `json:"run_id,omitempty"`
	SentUTC  string  `json:"sent_utc"`  // when the prompt was sent, RFC3339 UTC
	SentUnix float64 `json:"sent_unix"` // same instant, for sorting

	Phase   string `json:"phase"`   // which experiment, e.g. "api-caching", "retention", "app-fetch"
	Surface string `json:"surface"` // "api" or "web-app"
	Vendor  string `json:"vendor"`  // who was asked, e.g. "or:anthropic"
	Model   string `json:"model"`   // which model, e.g. "anthropic/claude-sonnet-5"; blank if unknown
	// ToolForced: did we force the web tool on? Yes over the API, where "no
	// request arrived" then means "it had a stored copy". No in the web apps,
	// where we measure whether it chooses to browse at all.
	ToolForced bool `json:"tool_forced"`

	PageURL string `json:"page_url"`

	// What the server actually saw for this trial.
	RobotsFetched bool   `json:"robots_fetched"` // did the fetcher pull /robots.txt in the window
	PageFetches   int    `json:"page_fetches"`   // how many times the page itself was fetched (a count)
	FetcherName   string `json:"fetcher_name"`   // the fetcher's declared user-agent, if any arrived
	FetcherIP     string `json:"fetcher_ip"`

	// What the answer showed.
	CodeInAnswer bool `json:"code_in_answer"` // did the page's secret code come back

	// Web-app only: did the app ask a question / for permission instead of acting,
	// and did we then give the one fixed confirmation ("when permitted" condition).
	AskedBack bool `json:"asked_back,omitempty"`
	Confirmed bool `json:"confirmed,omitempty"`
	// ReaskOf: the trial_id whose same page this trial re-asked (holds-a-copy test).
	ReaskOf string `json:"reask_of,omitempty"`

	Verdict string `json:"verdict"` // see verdictOf

	// Optional, present only when a fetch arrived. Room to grow without a schema
	// change: a reader that predates these sees the zero value.
	AskedIfChanged bool  `json:"asked_if_changed,omitempty"` // sent a conditional "has this changed?" request
	Status         int   `json:"status,omitempty"`           // HTTP status the fetcher got
	PageBytes      int64 `json:"page_bytes,omitempty"`       // bytes served to the fetcher (hook for the size sweep)

	Notes string `json:"notes,omitempty"`
}

// RowMeta is what the caller knows that a single Correlation does not: which
// experiment this is, whether the tool was forced, and how to name the party.
type RowMeta struct {
	Phase      string
	Surface    string // "api" or "web-app"
	ToolForced bool
	Vendor     string // if empty, the trial's Provider is used
	Model      string
}

// verdictOf collapses a correlation into one fixed word, following the same
// order of cases as WriteCorrelationReport so the table never disagrees with the
// long report about what a trial showed.
func verdictOf(c Correlation) string {
	t := c.Trial
	switch {
	case t.Error != "":
		return "error"
	case c.Fetched:
		return "fetched"
	case t.PageLive.CheckedAtAll() && !t.PageLive.OK:
		return "not-scored"
	case len(c.OtherHits) > 0:
		return "unattributable"
	case t.FoundMarker:
		return "served-from-store"
	default:
		return "no-evidence"
	}
}

// robotsFetchedFor reports whether the fetcher that took this page also pulled
// /robots.txt in the trial's window. We match on the fetcher's IDENTITY (the
// classified bot name of the page fetches), not the raw client IP: a fetcher may
// draw from a pool of addresses, so the robots read and the page read can come
// from different IPs, and matching on the bot name survives that. With no page
// fetch there is no identity to link to, so it returns false rather than
// pinning a stray robots read on a party that never fetched.
func robotsFetchedFor(c Correlation, records []Record, windowSeconds float64) bool {
	bots := map[string]bool{}
	for _, h := range c.Hits {
		if h.Bot != "" && h.Bot != "unknown" {
			bots[h.Bot] = true
		}
	}
	if len(bots) == 0 {
		return false
	}
	t := c.Trial
	for _, r := range records {
		if r.PageKey != "/robots.txt" {
			continue
		}
		if r.TSUnix < t.SentUnix || r.TSUnix > t.SentUnix+windowSeconds {
			continue
		}
		if bots[r.Bot] {
			return true
		}
	}
	return false
}

// RowFrom builds one table row from a correlation and the records it was
// correlated against (needed only to spot the robots.txt read).
func RowFrom(c Correlation, records []Record, windowSeconds float64, meta RowMeta) TrialRow {
	t := c.Trial
	vendor := meta.Vendor
	if vendor == "" {
		vendor = t.Provider
	}
	model := meta.Model
	if model == "" {
		model = t.Model
	}
	row := TrialRow{
		TrialID:       t.TrialID,
		SentUTC:       t.SentAt,
		SentUnix:      t.SentUnix,
		Phase:         meta.Phase,
		Surface:       meta.Surface,
		Vendor:        vendor,
		Model:         model,
		ToolForced:    meta.ToolForced,
		PageURL:       t.URL,
		RobotsFetched: robotsFetchedFor(c, records, windowSeconds),
		PageFetches:   len(c.Hits),
		CodeInAnswer:  t.FoundMarker,
		Verdict:       verdictOf(c),
	}
	if len(c.Hits) > 0 {
		h := c.Hits[0]
		row.FetcherName = h.UserAgent
		row.FetcherIP = h.ClientIP
		row.AskedIfChanged = h.AskedIfChanged()
		row.Status = h.StatusCode
		row.PageBytes = h.BytesSent
	}
	if t.Error != "" {
		row.Notes = truncateStr(t.Error, 120)
	}
	return row
}

// RowsFrom builds a table from many correlations, oldest first.
func RowsFrom(cors []Correlation, records []Record, windowSeconds float64, meta RowMeta) []TrialRow {
	out := make([]TrialRow, 0, len(cors))
	for _, c := range cors {
		out = append(out, RowFrom(c, records, windowSeconds, meta))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SentUnix < out[j].SentUnix })
	return out
}

// WriteTrialRowsJSONL writes the rows as newline-delimited JSON, the on-disk
// form both the API driver and the web-app console write.
func WriteTrialRowsJSONL(w io.Writer, rows []TrialRow) error {
	enc := json.NewEncoder(w)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// RenderTrialTable prints the rows as an aligned text table with a key above it,
// so it reads correctly for someone who has never seen the columns before.
func RenderTrialTable(w io.Writer, rows []TrialRow) {
	fmt.Fprintln(w, "  PER-TRIAL RESULTS")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  what each column means:")
	fmt.Fprintln(w, "    sent (UTC)   when the prompt was sent (logs are UTC; add 3h for Riyadh time)")
	fmt.Fprintln(w, "    vendor/model who was asked, and which model")
	fmt.Fprintln(w, "    forced       was the web tool forced on (yes over the API, no in a web app)")
	fmt.Fprintln(w, "    robots       did the fetcher read /robots.txt for this trial")
	fmt.Fprintln(w, "    fetches      how many times the page itself was fetched")
	fmt.Fprintln(w, "    code         did the page's secret code come back in the answer")
	fmt.Fprintln(w, "    verdict      fetched / served-from-store (had a copy) / no-evidence /")
	fmt.Fprintln(w, "                 asked-permission (asked before browsing) / unattributable /")
	fmt.Fprintln(w, "                 not-scored / error")
	fmt.Fprintln(w)

	head := []string{"sent (UTC)", "vendor", "model", "forced", "robots", "fetches", "code", "verdict"}
	rowsText := [][]string{head}
	for _, r := range rows {
		rowsText = append(rowsText, []string{
			shortTS(r.SentUTC),
			r.Vendor,
			r.Model,
			yesNo(r.ToolForced),
			yesNo(r.RobotsFetched),
			fmt.Sprintf("%d", r.PageFetches),
			yesNo(r.CodeInAnswer),
			r.Verdict,
		})
	}

	widths := make([]int, len(head))
	for _, tr := range rowsText {
		for i, cell := range tr {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	for ri, tr := range rowsText {
		var b strings.Builder
		b.WriteString("  ")
		for i, cell := range tr {
			fmt.Fprintf(&b, "%-*s", widths[i]+2, cell)
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
		if ri == 0 {
			b.Reset()
			b.WriteString("  ")
			for i := range tr {
				b.WriteString(strings.Repeat("-", widths[i]) + "  ")
			}
			fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
		}
	}
	fmt.Fprintln(w)
	summarizeVerdicts(w, rows)
}

func summarizeVerdicts(w io.Writer, rows []TrialRow) {
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Verdict]++
	}
	order := []string{"fetched", "served-from-store", "asked-permission", "no-evidence", "unattributable", "not-scored", "error"}
	parts := make([]string, 0, len(order))
	for _, v := range order {
		if counts[v] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", v, counts[v]))
		}
	}
	fmt.Fprintf(w, "  %d trials: %s\n", len(rows), strings.Join(parts, ", "))
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// shortTS trims an RFC3339 timestamp to "MM-DD HH:MM" for the table; the full
// value stays in the JSONL.
func shortTS(ts string) string {
	if len(ts) >= 16 && ts[10] == 'T' {
		return ts[5:10] + " " + ts[11:16]
	}
	return ts
}
