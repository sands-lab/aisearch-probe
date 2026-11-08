package probe

import (
	"strings"
	"testing"
	"time"
)

// rec builds a Record at a given second offset. Defaults describe a plain
// unconditional fetch answered with a stamped 200.
type opt func(*Record)

func withAsk() opt          { return func(r *Record) { r.IfNoneMatch = `"v1"` } }
func withIMS() opt          { return func(r *Record) { r.IfModifiedSince = "Wed, 21 Oct 2026 07:28:00 GMT" } }
func noStamp() opt          { return func(r *Record) { r.ETagSent = ""; r.LastModifiedSent = "" } }
func status(c int) opt      { return func(r *Record) { r.StatusCode = c } }
func bytes(n int64) opt     { return func(r *Record) { r.BytesSent = n } }
func page(p string) opt     { return func(r *Record) { r.PageKey = p } }
func bot(b string) opt      { return func(r *Record) { r.Bot = b } }
func cfStatus(s string) opt { return func(r *Record) { r.CFCacheStatus = s } }

func rec(tsec float64, opts ...opt) Record {
	r := Record{
		TS:         time.Unix(int64(tsec), 0).UTC().Format(time.RFC3339Nano),
		TSUnix:     tsec,
		Method:     "GET",
		URL:        "https://example.test/a",
		PageKey:    "/a",
		StatusCode: 200,
		Bot:        "testbot",
		UserAgent:  "TestBot/1.0",
		ETagSent:   `"v1"`,
		BytesSent:  1000,
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func only(t *testing.T, reports []BotReport, name string) BotReport {
	t.Helper()
	for _, r := range reports {
		if r.Bot == name {
			return r
		}
	}
	t.Fatalf("no report for bot %q (got %d reports)", name, len(reports))
	return BotReport{}
}

// The core fairness claim: a first visit is never held against a bot.
func TestFirstVisitIsNeverJudged(t *testing.T) {
	reports := Analyze([]Record{rec(100)})
	r := only(t, reports, "testbot")

	if r.FirstSight != 1 {
		t.Errorf("FirstSight = %d, want 1", r.FirstSight)
	}
	if r.Judgeable != 0 {
		t.Errorf("Judgeable = %d, want 0 — a first visit has nothing to revalidate", r.Judgeable)
	}
	if r.WastedBytes != 0 {
		t.Errorf("WastedBytes = %d, want 0 — the first download is not waste", r.WastedBytes)
	}
	if got := r.AskedPct(); got != -1 {
		t.Errorf("AskedPct() = %v, want -1 (n/a) when nothing is judgeable", got)
	}
}

// A bot that only ever visits new pages must not be called a bad citizen.
// This is precisely the bias the old ratio-over-all-requests metric had.
func TestManyDistinctFirstVisitsGiveNoVerdict(t *testing.T) {
	var recs []Record
	for i, p := range []string{"/a", "/b", "/c", "/d", "/e"} {
		recs = append(recs, rec(float64(100+i), page(p)))
	}
	r := only(t, Analyze(recs), "testbot")

	if r.Judgeable != 0 || r.Blind != 0 {
		t.Errorf("Judgeable=%d Blind=%d, want 0/0", r.Judgeable, r.Blind)
	}
	if v := r.Verdict(); !strings.Contains(v, "Can't tell yet") {
		t.Errorf("Verdict() = %q, want a 'can't tell yet' verdict, not an accusation", v)
	}
}

func TestReturnVisitBlindIsWaste(t *testing.T) {
	r := only(t, Analyze([]Record{
		rec(100, bytes(1500)),
		rec(200, bytes(1500)), // came back, did not ask
	}), "testbot")

	if r.FirstSight != 1 {
		t.Errorf("FirstSight = %d, want 1", r.FirstSight)
	}
	if r.Judgeable != 1 || r.Blind != 1 || r.Asked != 0 {
		t.Errorf("Judgeable=%d Blind=%d Asked=%d, want 1/1/0", r.Judgeable, r.Blind, r.Asked)
	}
	if r.WastedBytes != 1500 {
		t.Errorf("WastedBytes = %d, want 1500 (the second body only)", r.WastedBytes)
	}
	if v := r.Verdict(); !strings.Contains(v, "Never asks") {
		t.Errorf("Verdict() = %q, want 'Never asks'", v)
	}
}

func TestReturnVisitAskingIsNotWaste(t *testing.T) {
	r := only(t, Analyze([]Record{
		rec(100, bytes(1500)),
		rec(200, withAsk(), status(304), bytes(0)),
	}), "testbot")

	if r.Judgeable != 1 || r.Asked != 1 || r.Blind != 0 {
		t.Errorf("Judgeable=%d Asked=%d Blind=%d, want 1/1/0", r.Judgeable, r.Asked, r.Blind)
	}
	if r.WastedBytes != 0 {
		t.Errorf("WastedBytes = %d, want 0", r.WastedBytes)
	}
	if r.NotModified != 1 {
		t.Errorf("NotModified = %d, want 1", r.NotModified)
	}
	if got := r.AskedPct(); got != 100 {
		t.Errorf("AskedPct() = %v, want 100", got)
	}
}

// If-Modified-Since counts as asking just as much as If-None-Match.
func TestIfModifiedSinceCountsAsAsking(t *testing.T) {
	r := only(t, Analyze([]Record{rec(100), rec(200, withIMS())}), "testbot")
	if r.Asked != 1 {
		t.Errorf("Asked = %d, want 1 — If-Modified-Since is also asking", r.Asked)
	}
}

// Our failure to stamp must be blamed on us, not on the bot.
func TestUnstampedReturnVisitBlamesUsNotTheBot(t *testing.T) {
	r := only(t, Analyze([]Record{
		rec(100, noStamp()),
		rec(200, noStamp()),
		rec(300, noStamp()),
	}), "testbot")

	if r.FirstSight != 1 {
		t.Errorf("FirstSight = %d, want 1", r.FirstSight)
	}
	if r.ReturnUnstamped != 2 {
		t.Errorf("ReturnUnstamped = %d, want 2", r.ReturnUnstamped)
	}
	if r.Judgeable != 0 || r.Blind != 0 {
		t.Errorf("Judgeable=%d Blind=%d, want 0/0 — unstamped visits are not the bot's fault", r.Judgeable, r.Blind)
	}
	if r.WastedBytes != 0 {
		t.Errorf("WastedBytes = %d, want 0", r.WastedBytes)
	}
	v := r.Verdict()
	if !strings.Contains(v, "never stamped") {
		t.Errorf("Verdict() = %q, want it to point at our own ETag headers", v)
	}
}

// Last-Modified alone is enough of a stamp.
func TestLastModifiedAloneCountsAsStamp(t *testing.T) {
	lm := func(r *Record) { r.ETagSent = ""; r.LastModifiedSent = "Wed, 21 Oct 2026 07:28:00 GMT" }
	r := only(t, Analyze([]Record{rec(100, lm), rec(200, lm)}), "testbot")
	if r.Judgeable != 1 {
		t.Errorf("Judgeable = %d, want 1 — Last-Modified is a validator too", r.Judgeable)
	}
}

// The buckets must partition the traffic exactly, or the report double-counts.
func TestBucketsPartitionAllRequests(t *testing.T) {
	recs := []Record{
		rec(100, page("/a")),
		rec(110, page("/b"), noStamp()),
		rec(200, page("/a"), withAsk(), status(304), bytes(0)),
		rec(210, page("/b"), noStamp()),
		rec(300, page("/a")),
		rec(400, page("/c")),
	}
	r := only(t, Analyze(recs), "testbot")

	if sum := r.FirstSight + r.ReturnUnstamped + r.Judgeable; sum != r.TotalRequests {
		t.Errorf("buckets sum to %d but TotalRequests = %d", sum, r.TotalRequests)
	}
	if r.Asked+r.Blind != r.Judgeable {
		t.Errorf("Asked+Blind = %d but Judgeable = %d", r.Asked+r.Blind, r.Judgeable)
	}
	if r.TotalRequests != 6 {
		t.Errorf("TotalRequests = %d, want 6", r.TotalRequests)
	}
	if r.UniquePages != 3 {
		t.Errorf("UniquePages = %d, want 3", r.UniquePages)
	}
}

// Two bots visiting the same page must not contaminate each other: bot B's
// first sight of /a is still a first sight even though bot A went there first.
func TestBotsTrackedIndependently(t *testing.T) {
	reports := Analyze([]Record{
		rec(100, bot("alpha")),
		rec(200, bot("alpha")),
		rec(300, bot("beta")),
	})
	a := only(t, reports, "alpha")
	b := only(t, reports, "beta")

	if a.Judgeable != 1 {
		t.Errorf("alpha Judgeable = %d, want 1", a.Judgeable)
	}
	if b.FirstSight != 1 || b.Judgeable != 0 {
		t.Errorf("beta FirstSight=%d Judgeable=%d, want 1/0", b.FirstSight, b.Judgeable)
	}
}

// Records arriving out of order must not change the answer.
func TestOutOfOrderRecordsAreSorted(t *testing.T) {
	inOrder := Analyze([]Record{rec(100), rec(200, withAsk(), status(304), bytes(0))})
	shuffled := Analyze([]Record{rec(200, withAsk(), status(304), bytes(0)), rec(100)})

	a, b := only(t, inOrder, "testbot"), only(t, shuffled, "testbot")
	if a.Judgeable != b.Judgeable || a.Asked != b.Asked || a.FirstSight != b.FirstSight {
		t.Errorf("order changed the result: %+v vs %+v", a, b)
	}
}

// A blind return visit that 404s wastes no body, but is still a blind visit.
func TestBlindNon200DoesNotCountAsWastedBytes(t *testing.T) {
	r := only(t, Analyze([]Record{rec(100), rec(200, status(404), bytes(50))}), "testbot")
	if r.Blind != 1 {
		t.Errorf("Blind = %d, want 1", r.Blind)
	}
	if r.WastedBytes != 0 {
		t.Errorf("WastedBytes = %d, want 0 — a 404 body is not content they already had", r.WastedBytes)
	}
}

func TestEdgeServedRequestsAreFlagged(t *testing.T) {
	r := only(t, Analyze([]Record{
		rec(100),
		rec(200, cfStatus("HIT")),
		rec(300, cfStatus("MISS")),
	}), "testbot")
	if r.EdgeServed != 1 {
		t.Errorf("EdgeServed = %d, want 1", r.EdgeServed)
	}
}

func TestRevisitIntervals(t *testing.T) {
	r := only(t, Analyze([]Record{rec(100), rec(200), rec(500)}), "testbot")
	if !r.HasIntervals {
		t.Fatal("HasIntervals = false, want true")
	}
	if r.IntervalMinS != 100 || r.IntervalMaxS != 300 || r.IntervalMeanS != 200 {
		t.Errorf("intervals min=%v mean=%v max=%v, want 100/200/300",
			r.IntervalMinS, r.IntervalMeanS, r.IntervalMaxS)
	}
}
