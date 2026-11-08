package probe

import (
	"strings"
	"testing"
)

func TestVerdictOf(t *testing.T) {
	cases := []struct {
		name string
		c    Correlation
		want string
	}{
		{"error", Correlation{Trial: Trial{Error: "boom"}}, "error"},
		{"fetched", Correlation{Fetched: true, Hits: []Record{{Bot: "anthropic-query"}}}, "fetched"},
		{
			"not-scored",
			Correlation{Trial: Trial{PageLive: Liveness{CheckAt: "2026-09-17T00:00:00Z", OK: false}}},
			"not-scored",
		},
		{"unattributable", Correlation{OtherHits: []Record{{Bot: "unknown"}}}, "unattributable"},
		{"served-from-store", Correlation{Trial: Trial{FoundMarker: true}}, "served-from-store"},
		{"no-evidence", Correlation{}, "no-evidence"},
	}
	for _, tc := range cases {
		if got := verdictOf(tc.c); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// A fetched trial should carry the fetcher's identity and the optional fetch
// facts (conditional request, status, bytes).
func TestRowFromFetched(t *testing.T) {
	c := Correlation{
		Trial:   Trial{TrialID: "t1", SentUnix: 1000, SentAt: "2026-09-17T10:00:00Z", Provider: "or:anthropic", FoundMarker: true},
		Fetched: true,
		Hits: []Record{{
			Bot: "anthropic-query", UserAgent: "Claude-User/1.0", ClientIP: "34.162.191.81",
			StatusCode: 200, BytesSent: 316, IfNoneMatch: `"abc"`,
		}},
	}
	row := RowFrom(c, nil, 90, RowMeta{Phase: "retention", Surface: "api", ToolForced: true, Vendor: "or:anthropic", Model: "anthropic/claude-sonnet-5"})
	if row.Verdict != "fetched" || row.PageFetches != 1 {
		t.Fatalf("verdict/fetches wrong: %+v", row)
	}
	if row.FetcherName != "Claude-User/1.0" || row.FetcherIP != "34.162.191.81" {
		t.Errorf("fetcher identity not carried: %+v", row)
	}
	if !row.AskedIfChanged || row.Status != 200 || row.PageBytes != 316 {
		t.Errorf("optional fetch facts wrong: asked=%v status=%d bytes=%d", row.AskedIfChanged, row.Status, row.PageBytes)
	}
	if row.Vendor != "or:anthropic" || row.Model != "anthropic/claude-sonnet-5" {
		t.Errorf("vendor/model split wrong: %q %q", row.Vendor, row.Model)
	}
	if !row.CodeInAnswer {
		t.Errorf("code-in-answer should follow FoundMarker")
	}
}

// Vendor falls back to the trial's Provider when RowMeta leaves it blank.
func TestRowFromVendorFallback(t *testing.T) {
	c := Correlation{Trial: Trial{Provider: "or:x-ai"}}
	row := RowFrom(c, nil, 90, RowMeta{Phase: "api", Surface: "api"})
	if row.Vendor != "or:x-ai" {
		t.Errorf("vendor fallback: got %q", row.Vendor)
	}
}

// robots.txt is attributed by the FETCHER'S BOT NAME, not its IP, so a robots
// read from a different address in the same pool still counts, and a read by an
// unrelated bot does not.
func TestRobotsFetchedMatchesByBot(t *testing.T) {
	c := Correlation{
		Trial:   Trial{SentUnix: 1000},
		Fetched: true,
		Hits:    []Record{{Bot: "anthropic-query", ClientIP: "34.162.191.81", PageKey: "/page/x"}},
	}
	records := []Record{
		// robots read by the same bot, a DIFFERENT IP, inside the window (after the
		// prompt, as it is in the real log) -> counts
		{Bot: "anthropic-query", ClientIP: "34.162.244.71", PageKey: "/robots.txt", TSUnix: 1003},
	}
	if !robotsFetchedFor(c, records, 90) {
		t.Errorf("should match robots by bot name across IPs")
	}

	// a robots read by an unrelated bot must not count
	other := []Record{{Bot: "googlebot", ClientIP: "1.2.3.4", PageKey: "/robots.txt", TSUnix: 1000}}
	if robotsFetchedFor(c, other, 90) {
		t.Errorf("unrelated bot's robots read must not be attributed")
	}

	// outside the window must not count
	late := []Record{{Bot: "anthropic-query", PageKey: "/robots.txt", TSUnix: 2000}}
	if robotsFetchedFor(c, late, 90) {
		t.Errorf("robots read outside the window must not count")
	}
}

// With no page fetch there is no fetcher identity to link a robots read to.
func TestRobotsFetchedNoHits(t *testing.T) {
	c := Correlation{Trial: Trial{SentUnix: 1000}}
	records := []Record{{Bot: "anthropic-query", PageKey: "/robots.txt", TSUnix: 1000}}
	if robotsFetchedFor(c, records, 90) {
		t.Errorf("no page fetch means no robots attribution")
	}
}

func TestRenderAndJSONLSmoke(t *testing.T) {
	rows := []TrialRow{
		{TrialID: "t1", SentUTC: "2026-09-17T10:00:00Z", Vendor: "or:anthropic", Model: "claude-sonnet-5", ToolForced: true, PageFetches: 1, RobotsFetched: true, CodeInAnswer: true, Verdict: "fetched"},
		{TrialID: "t2", SentUTC: "2026-09-17T11:00:00Z", Vendor: "or:openai", Model: "gpt-5-mini", ToolForced: true, Verdict: "served-from-store", CodeInAnswer: true},
	}
	var tab strings.Builder
	RenderTrialTable(&tab, rows)
	out := tab.String()
	for _, want := range []string{"PER-TRIAL RESULTS", "verdict", "fetched", "served-from-store", "2 trials"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q\n%s", want, out)
		}
	}
	var jl strings.Builder
	if err := WriteTrialRowsJSONL(&jl, rows); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(jl.String()), "\n") + 1; lines != 2 {
		t.Errorf("want 2 JSONL lines, got %d", lines)
	}
	// omitempty: the no-fetch row must not carry page_bytes
	if strings.Contains(jl.String(), `"page_bytes"`) && strings.Contains(jl.String(), `"page_bytes":0`) {
		t.Errorf("zero page_bytes should be omitted")
	}
}
