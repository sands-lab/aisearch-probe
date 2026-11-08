package main

import (
	"math"
	"strings"
	"testing"
	"time"

	"aisearch-probe/internal/probe"
)

// In coded mode the console must mint a URL on the /cpage/ route whose id the
// SERVER can parse, and use that page's OWN reference code (recomputed from the
// secret) as the marker — not the shared RCP-2026-08. The secret is the driver's;
// it is used only to recompute the code, never put in the URL.
func TestMintCoded(t *testing.T) {
	const secret = "driver-secret-xyz-0123"
	id, url, marker := mintCoded("https://probe.attentionhub.org", "chatgpt", secret)

	if !strings.Contains(url, "/cpage/"+id) {
		t.Errorf("url %q must be on the /cpage/ route with id %q", url, id)
	}
	if _, ok := probe.CpageSubjectForID(id); !ok {
		t.Errorf("minted id %q does not resolve on the server (bad cr<NN>- form)", id)
	}
	if marker != probe.CpageCode(secret, id, probe.CpageFieldRef, 1) {
		t.Error("marker must equal the recomputed per-page reference code")
	}
	if marker == "" || marker == "RCP-2026-08" {
		t.Errorf("marker must be a real per-page code, got %q", marker)
	}
	if !strings.Contains(id, "chatgpt") {
		t.Errorf("id %q should carry the app name for log-eyeballing", id)
	}
	if strings.Contains(url, secret) {
		t.Error("the secret must never appear in the URL")
	}
}

func TestWebVerdict(t *testing.T) {
	cases := []struct {
		fetches   int
		code      bool
		askedBack bool
		confirmed bool
		want      string
	}{
		{1, true, false, false, "fetched"},           // read it, code came back
		{3, false, false, false, "fetched"},          // a burst; a fetch still wins
		{0, true, false, false, "served-from-store"}, // no read, code back: had a copy
		{0, false, false, false, "no-evidence"},      // no read, no code: never had it
		{0, false, true, false, "asked-permission"},  // asked first, not confirmed
		{1, false, true, false, "fetched"},           // asked, but a fetch still wins
		{0, false, true, true, "no-evidence"},        // confirmed but still didn't browse
		{0, true, true, true, "served-from-store"},   // confirmed, no fetch, code back
	}
	for _, c := range cases {
		if got := webVerdict(c.fetches, c.code, c.askedBack, c.confirmed); got != c.want {
			t.Errorf("webVerdict(%d, code=%v, asked=%v, confirmed=%v) = %q, want %q",
				c.fetches, c.code, c.askedBack, c.confirmed, got, c.want)
		}
	}
}

func TestRobotsSeenMatchesByIdentity(t *testing.T) {
	page := []probe.Record{{PageKey: "/page/chatgpt-x", ClientIP: "1.2.3.4", Bot: "ChatGPT-User"}}

	// A robots read from the SAME ip counts.
	all := append([]probe.Record{}, page...)
	all = append(all, probe.Record{PageKey: "/robots.txt", ClientIP: "1.2.3.4", Bot: "ChatGPT-User"})
	if !robotsSeen(page, all) {
		t.Error("robots read from the same fetcher should count")
	}

	// A robots read from an unrelated party does not.
	all = append([]probe.Record{}, page...)
	all = append(all, probe.Record{PageKey: "/robots.txt", ClientIP: "9.9.9.9", Bot: "SomeoneElse"})
	if robotsSeen(page, all) {
		t.Error("robots read from an unrelated party must not count")
	}

	// With no page fetch there is nothing to attribute a robots read to.
	if robotsSeen(nil, []probe.Record{{PageKey: "/robots.txt", ClientIP: "1.2.3.4"}}) {
		t.Error("no page fetch means no robots attribution")
	}
}

// TestDeriveOne is the core of the regenerable dataset: from a stored reply and
// the server log, it must recover the fetch count, the fetcher, the code check,
// and the verdict — matching the trial by its unique address alone.
func TestDeriveOne(t *testing.T) {
	raw := webRawRecord{
		TrialID:  "deepseek-run1",
		RunID:    "run1",
		Phase:    "app-fetch",
		App:      "deepseek",
		Config:   "free-loggedin-search",
		PageURL:  "https://probe.attentionhub.org/page/deepseek-run1",
		Marker:   "RCP-2026-08",
		Reply:    "The page says:\n\nStudy reference: RCP-2026-08\n",
		SentUnix: 1000,
	}
	records := []probe.Record{
		{PageKey: "/page/deepseek-run1", TSUnix: 1005, UserAgent: "Mozilla/5.0 (forged)", ClientIP: "94.74.104.231", StatusCode: 200, BytesSent: 321},
		{PageKey: "/page/other", TSUnix: 1005, UserAgent: "someone-else", ClientIP: "5.5.5.5"}, // must be ignored
	}

	row := deriveOne(raw, records, math.Inf(1))

	if row.PageFetches != 1 {
		t.Errorf("page_fetches = %d, want 1 (only the matching address counts)", row.PageFetches)
	}
	if !row.CodeInAnswer {
		t.Error("code_in_answer should be true — the marker is in the stored reply past a blank line")
	}
	if row.Verdict != "fetched" {
		t.Errorf("verdict = %q, want fetched", row.Verdict)
	}
	if row.FetcherName != "Mozilla/5.0 (forged)" || row.FetcherIP != "94.74.104.231" {
		t.Errorf("fetcher not carried over: name=%q ip=%q", row.FetcherName, row.FetcherIP)
	}
	if row.RunID != "run1" || row.Surface != "web-app" || row.ToolForced {
		t.Errorf("run/surface/forced wrong: run=%q surface=%q forced=%v", row.RunID, row.Surface, row.ToolForced)
	}
}

// After a run the console shows only that run's rows, not the whole dataset.
func TestRowsForRun(t *testing.T) {
	rows := []probe.TrialRow{
		{TrialID: "a", RunID: "run1"},
		{TrialID: "b", RunID: "run2"},
		{TrialID: "c", RunID: "run1"},
	}
	got := rowsForRun(rows, "run1")
	if len(got) != 2 || got[0].TrialID != "a" || got[1].TrialID != "c" {
		t.Errorf("rowsForRun(run1) = %+v, want the two run1 rows (a, c)", got)
	}
	if len(rowsForRun(rows, "nope")) != 0 {
		t.Error("an unknown run should yield no rows")
	}
}

func TestParseFollowups(t *testing.T) {
	steps, err := parseFollowups("0m:same-session, 5m:new-session")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(steps))
	}
	if steps[0].off != 0 || steps[0].label != "same-session" {
		t.Errorf("step 0 = %v", steps[0])
	}
	if steps[1].off != 5*time.Minute || steps[1].label != "new-session" {
		t.Errorf("step 1 = %v", steps[1])
	}
	for _, bad := range []string{"", "5m", "5x:label", "5m:"} {
		if _, err := parseFollowups(bad); err == nil {
			t.Errorf("parseFollowups(%q) should have errored", bad)
		}
	}
}

// TestReaskWindow is the heart of the holds-a-copy test: two asks of the SAME page
// must split the fetches by time, so the warm ask owns the first fetch and the
// re-ask owns only a later one — never both counting the same fetch.
func TestReaskWindow(t *testing.T) {
	url := "https://probe.attentionhub.org/page/claude-warm"
	warm := webRawRecord{TrialID: "claude-warm", PageURL: url, Marker: "RCP-2026-08", SentUnix: 1000, Reply: "code RCP-2026-08"}
	reask := webRawRecord{TrialID: "claude-reask", PageURL: url, Marker: "RCP-2026-08", SentUnix: 5000, ReaskOf: "claude-warm", Reply: "code RCP-2026-08"}
	records := []probe.Record{
		{PageKey: "/page/claude-warm", TSUnix: 1002, ClientIP: "a"}, // warm fetch
		{PageKey: "/page/claude-warm", TSUnix: 5002, ClientIP: "b"}, // re-ask fetch
	}

	warmRow := deriveOne(warm, records, 5000)          // upper = the re-ask's send time
	reaskRow := deriveOne(reask, records, math.Inf(1)) // last ask: open-ended

	if warmRow.PageFetches != 1 {
		t.Errorf("warm ask should own exactly 1 fetch, got %d", warmRow.PageFetches)
	}
	if reaskRow.PageFetches != 1 {
		t.Errorf("re-ask should own exactly 1 fetch (the later one), got %d", reaskRow.PageFetches)
	}
	// A re-ask that DID re-fetch means the app did NOT keep a copy.
	if reaskRow.Verdict != "fetched" {
		t.Errorf("re-ask with a new fetch = fetched, got %q", reaskRow.Verdict)
	}
	if reaskRow.ReaskOf != "claude-warm" {
		t.Errorf("reask_of not carried: %q", reaskRow.ReaskOf)
	}
}

// selectWarms picks the warmed pages a batch re-ask should revisit: config starts
// with the label, and it is a warm (not itself a re-ask), deduped by page.
func TestSelectWarms(t *testing.T) {
	raws := []webRawRecord{
		{TrialID: "a", App: "chatgpt", Config: "lifetime-warm", PageURL: "https://h/cpage/u1"},
		{TrialID: "b", App: "deepseek", Config: "lifetime-warm-search", PageURL: "https://h/cpage/u2"},
		{TrialID: "c", App: "x", Config: "other", PageURL: "https://h/cpage/u3"},
		{TrialID: "d", App: "chatgpt", Config: "lifetime-reask", PageURL: "https://h/cpage/u1", ReaskOf: "a"},
		{TrialID: "e", App: "chatgpt", Config: "lifetime-warm", PageURL: "https://h/cpage/u1"}, // dup page of a
	}
	got := selectWarms(raws, "lifetime-warm")
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (a,b; other excluded, reask excluded, dup page collapsed)", len(got))
	}
	if got[0].TrialID != "a" || got[1].TrialID != "b" {
		t.Errorf("got %v, want [a b]", []string{got[0].TrialID, got[1].TrialID})
	}
	if len(selectWarms(raws, "nope")) != 0 {
		t.Error("unknown label should select nothing")
	}
}
