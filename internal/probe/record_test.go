package probe

import (
	"strings"
	"testing"
	"time"
)

func TestClassifyBot(t *testing.T) {
	tests := []struct {
		ua   string
		want string
	}{
		{"Mozilla/5.0 (compatible; OAI-SearchBot/1.0; +https://openai.com/searchbot)", "openai-search"},
		{"Mozilla/5.0 ... ChatGPT-User/1.0", "openai-query"},
		{"Mozilla/5.0 (compatible; GPTBot/1.1; +https://openai.com/gptbot)", "openai-training"},
		{"Mozilla/5.0 (compatible; PerplexityBot/1.0)", "perplexity-index"},
		{"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", "google-index"},
		{"Googlebot-Image/1.0", "google-index"},
		{"Mozilla/5.0 (compatible; bingbot/2.0)", "bing-index"},
		{"curl/8.4.0", "unknown"},
		{"", "unknown"},
	}
	for _, tc := range tests {
		if got := ClassifyBot(tc.ua); got != tc.want {
			t.Errorf("ClassifyBot(%q) = %q, want %q", tc.ua, got, tc.want)
		}
	}
}

// Specific tokens must win over the general ones they contain, or every
// Anthropic crawler collapses into one bucket.
func TestClassifyBotPrefersMoreSpecificToken(t *testing.T) {
	if got := ClassifyBot("Mozilla/5.0 (compatible; Claude-SearchBot/1.0)"); got != "anthropic-search" {
		t.Errorf("Claude-SearchBot classified as %q, want anthropic-search", got)
	}
	if got := ClassifyBot("Mozilla/5.0 (compatible; ClaudeBot/1.0)"); got != "anthropic-training" {
		t.Errorf("ClaudeBot classified as %q, want anthropic-training", got)
	}
}

// Google-Extended is a robots.txt token, not a user agent. It must not sit in
// the table pretending to match live traffic.
func TestRobotsOnlyTokensAreNotInTheUATable(t *testing.T) {
	for _, sig := range botSignatures {
		if strings.EqualFold(sig.needle, "Google-Extended") || strings.EqualFold(sig.needle, "Applebot-Extended") {
			t.Errorf("%q is a robots.txt opt-out token, not a User-Agent — it will never match", sig.needle)
		}
	}
}

func TestAskedStampedAndEdgeHelpers(t *testing.T) {
	if (Record{}).AskedIfChanged() {
		t.Error("empty record should not count as asking")
	}
	if !(Record{IfNoneMatch: `"v1"`}).AskedIfChanged() {
		t.Error("If-None-Match should count as asking")
	}
	if !(Record{IfModifiedSince: "x"}).AskedIfChanged() {
		t.Error("If-Modified-Since should count as asking")
	}
	if (Record{}).Stamped() {
		t.Error("empty record should not count as stamped")
	}
	if !(Record{LastModifiedSent: "x"}).Stamped() {
		t.Error("Last-Modified should count as a stamp")
	}
	if !(Record{CFCacheStatus: "HIT"}).EdgeServed() {
		t.Error("CF-Cache-Status HIT should count as edge-served")
	}
	if (Record{CFCacheStatus: "MISS"}).EdgeServed() {
		t.Error("MISS should not count as edge-served")
	}
}

// Mode A (nginx) cannot classify bots and may not emit an epoch timestamp.
// Normalize must recover both, or nginx logs analyse as one nameless bot with
// every revisit interval equal to zero.
func TestNormalizeRecoversNginxGaps(t *testing.T) {
	r := Record{
		TS:        "2026-08-30T14:22:01+00:00",
		UserAgent: "Mozilla/5.0 (compatible; PerplexityBot/1.0)",
		URL:       "https://example.test/docs/page?ref=x",
	}
	r.Normalize()

	if r.Bot != "perplexity-index" {
		t.Errorf("Bot = %q, want perplexity-index recovered from the User-Agent", r.Bot)
	}
	if r.TSUnix <= 0 {
		t.Error("TSUnix = 0 — revisit intervals would all collapse to zero")
	}
	if r.PageKey != "/docs/page" {
		t.Errorf("PageKey = %q, want /docs/page derived from the URL", r.PageKey)
	}
}

// Both modes must agree on what "a page" is, or the same traffic yields
// different page counts depending on the collector.
func TestNormalizeStripsQueryFromPageKey(t *testing.T) {
	r := Record{PageKey: "/docs/page?ref=x", UserAgent: "curl", TS: "2026-08-30T14:22:01Z"}
	r.Normalize()
	if r.PageKey != "/docs/page" {
		t.Errorf("PageKey = %q, want the query stripped", r.PageKey)
	}
}

func TestNormalizeKeepsExplicitBot(t *testing.T) {
	r := Record{Bot: "already-set", UserAgent: "Googlebot/2.1", TS: "2026-08-30T14:22:01Z"}
	r.Normalize()
	if r.Bot != "already-set" {
		t.Errorf("Bot = %q, want the collector's own value preserved", r.Bot)
	}
}

func TestRecordTimeFallsBackToTS(t *testing.T) {
	r := Record{TS: "2026-08-30T14:22:01Z"}
	got := r.Time()
	want := time.Date(2026, 8, 30, 14, 22, 1, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("Time() = %v, want %v", got, want)
	}
}

func TestParseRecordsCountsBadLines(t *testing.T) {
	in := strings.Join([]string{
		`{"ts":"2026-08-30T14:22:01Z","user_agent":"Googlebot/2.1","page_key":"/a","status_code":200}`,
		`this is not json`,
		``,
		`{"ts":"2026-08-30T14:23:01Z","user_agent":"Googlebot/2.1","page_key":"/a","status_code":200}`,
	}, "\n")

	recs, skipped, err := ParseRecords(strings.NewReader(in), "", time.Time{})
	if err != nil {
		t.Fatalf("ParseRecords: %v", err)
	}
	if len(recs) != 2 {
		t.Errorf("got %d records, want 2", len(recs))
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 — a schema break must be visible, not silent", skipped)
	}
}

func TestParseRecordsFiltersByBotAndDate(t *testing.T) {
	in := strings.Join([]string{
		`{"ts":"2026-08-01T00:00:00Z","user_agent":"Googlebot/2.1","page_key":"/a"}`,
		`{"ts":"2026-09-01T00:00:00Z","user_agent":"Googlebot/2.1","page_key":"/a"}`,
		`{"ts":"2026-09-01T00:00:00Z","user_agent":"PerplexityBot/1.0","page_key":"/a"}`,
	}, "\n")

	recs, _, err := ParseRecords(strings.NewReader(in), "google", time.Time{})
	if err != nil {
		t.Fatalf("ParseRecords: %v", err)
	}
	if len(recs) != 2 {
		t.Errorf("bot filter kept %d records, want 2", len(recs))
	}

	since := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	recs, _, err = ParseRecords(strings.NewReader(in), "", since)
	if err != nil {
		t.Fatalf("ParseRecords: %v", err)
	}
	if len(recs) != 2 {
		t.Errorf("date filter kept %d records, want 2", len(recs))
	}
}

// Mode A's nginx snippet is the one copy of the schema the compiler cannot
// check. This feeds a line shaped exactly like nginx/logging-snippet.conf
// produces through the whole reader and asserts it analyses correctly.
func TestNginxModeALineAnalysesCorrectly(t *testing.T) {
	// Two visits to the same page by the same bot, the second one asking.
	// Note: no "bot" field and an nginx-style $time_iso8601 with an offset.
	lines := strings.Join([]string{
		`{"ts":"2026-08-30T14:22:01+00:00","ts_unix":1756563721.123,"method":"GET",` +
			`"url":"https://example.test/docs/page?ref=x","page_key":"/docs/page","status_code":200,` +
			`"user_agent":"Mozilla/5.0 (compatible; PerplexityBot/1.0)","client_ip":"203.0.113.9",` +
			`"if_none_match":"","if_modified_since":"","cache_control_req":"",` +
			`"etag_sent":"\"v1\"","last_modified_sent":"","cache_control_resp":"max-age=3600",` +
			`"expires_sent":"Thu, 01 Jan 2026 00:00:00 GMT",` +
			`"bytes_sent":2048,"cf_cache_status":"","cf_ray":"","referer":"","elapsed_s":0.031}`,
		`{"ts":"2026-08-30T15:22:01+00:00","ts_unix":1756567321.456,"method":"GET",` +
			`"url":"https://example.test/docs/page?ref=x","page_key":"/docs/page","status_code":200,` +
			`"user_agent":"Mozilla/5.0 (compatible; PerplexityBot/1.0)","client_ip":"203.0.113.9",` +
			`"if_none_match":"","if_modified_since":"","cache_control_req":"",` +
			`"etag_sent":"\"v1\"","last_modified_sent":"","cache_control_resp":"max-age=3600",` +
			`"expires_sent":"",` +
			`"bytes_sent":2048,"cf_cache_status":"","cf_ray":"","referer":"","elapsed_s":0.029}`,
	}, "\n")

	recs, skipped, err := ParseRecords(strings.NewReader(lines), "", time.Time{})
	if err != nil {
		t.Fatalf("ParseRecords: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped %d nginx lines — the snippet and the schema have drifted", skipped)
	}
	if recs[0].ExpiresSent != "Thu, 01 Jan 2026 00:00:00 GMT" {
		t.Errorf("ExpiresSent = %q — the nginx expires_sent field did not reach the record", recs[0].ExpiresSent)
	}

	reports := Analyze(recs)
	if len(reports) != 1 {
		t.Fatalf("got %d reports, want 1", len(reports))
	}
	r := reports[0]

	if r.Bot != "perplexity-index" {
		t.Errorf("Bot = %q, want perplexity-index — nginx logs no bot field, it must be recovered", r.Bot)
	}
	if r.FirstSight != 1 || r.Judgeable != 1 || r.Blind != 1 {
		t.Errorf("FirstSight=%d Judgeable=%d Blind=%d, want 1/1/1", r.FirstSight, r.Judgeable, r.Blind)
	}
	if r.WastedBytes != 2048 {
		t.Errorf("WastedBytes = %d, want 2048", r.WastedBytes)
	}
	// The revisit interval must be real, not zero. This is the bug that made
	// Mode A logs analyse with every interval collapsed to 0.
	if !r.HasIntervals {
		t.Fatal("HasIntervals = false — nginx timestamps were not usable")
	}
	if got := r.IntervalMinS; got < 3599 || got > 3601 {
		t.Errorf("revisit interval = %vs, want ~3600s", got)
	}
}

// Adding a signature must reclassify logs already on disk — including records a
// previous collector wrote as "unknown" before that signature existed.
func TestNormalizeReclassifiesPreviouslyUnknownBots(t *testing.T) {
	r := Record{Bot: "unknown", UserAgent: "probe-selfcheck/1 (a shell script)", TS: "2026-08-30T14:00:00Z"}
	r.Normalize()
	if r.Bot != "probe-selfcheck" {
		t.Errorf("Bot = %q, want probe-selfcheck — a stale 'unknown' must be re-derived", r.Bot)
	}

	// A genuinely unrecognised agent stays unknown.
	u := Record{Bot: "unknown", UserAgent: "Mozilla/5.0 (Windows NT 10.0)", TS: "2026-08-30T14:00:00Z"}
	u.Normalize()
	if u.Bot != "unknown" {
		t.Errorf("Bot = %q, want unknown", u.Bot)
	}

	// A name the collector did recognise is never overwritten.
	k := Record{Bot: "openai-query", UserAgent: "Googlebot/2.1", TS: "2026-08-30T14:00:00Z"}
	k.Normalize()
	if k.Bot != "openai-query" {
		t.Errorf("Bot = %q — a recognised classification must be preserved", k.Bot)
	}
}
