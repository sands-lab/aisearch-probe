package probe

import (
	"strings"
	"testing"
)

func render(t *testing.T, records []Record, verbose bool) string {
	t.Helper()
	var sb strings.Builder
	WriteReport(&sb, Analyze(records), verbose)
	return sb.String()
}

// The labels are the whole point of the redesign: a reader must be able to see
// why a number does or does not count against the bot.
func TestReportLabelsExplainWhyVisitsAreExcluded(t *testing.T) {
	out := render(t, []Record{rec(100), rec(200)}, false)

	for _, want := range []string{
		"Visits we can't fairly judge",
		"First time they'd seen the page",
		"they had no copy yet",
		"We never stamped their copy",
		"our fault, not theirs",
		"Visits we CAN judge",
		"Came back to a page we'd stamped",
		`Asked "has this changed?" first`,
		"Re-downloaded the whole page",
		"Wasted traffic",
		"content we sent them that they already had",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing the label %q\n---\n%s", want, out)
		}
	}
}

// A bot with nothing judgeable must show n/a, never 0%. Printing 0% would
// accuse a crawler that has simply never returned to a page.
func TestReportShowsNotApplicableRatherThanZeroPercent(t *testing.T) {
	out := render(t, []Record{rec(100)}, false) // a single first visit

	if strings.Contains(out, "(0%)") {
		t.Errorf("report printed 0%% for a bot with nothing to judge — that reads as an accusation\n---\n%s", out)
	}
	if !strings.Contains(out, "n/a") {
		t.Errorf("report should print n/a when nothing is judgeable\n---\n%s", out)
	}
	if !strings.Contains(out, "Can't tell yet") {
		t.Errorf("verdict should say it can't tell yet\n---\n%s", out)
	}
}

func TestReportWarnsWhenTheCDNAnswered(t *testing.T) {
	out := render(t, []Record{rec(100), rec(200, cfStatus("HIT"))}, false)
	if !strings.Contains(out, "Cloudflare's own cache") {
		t.Errorf("report must flag CDN-served requests as contamination\n---\n%s", out)
	}
	if !strings.Contains(out, "Bypass-cache") {
		t.Errorf("report should say how to fix CDN contamination\n---\n%s", out)
	}
}

func TestReportOmitsCDNWarningWhenClean(t *testing.T) {
	out := render(t, []Record{rec(100), rec(200)}, false)
	if strings.Contains(out, "Cloudflare's own cache") {
		t.Errorf("clean run should not show a CDN warning\n---\n%s", out)
	}
}

func TestVerbosePerPageBreakdown(t *testing.T) {
	recs := []Record{rec(100, page("/a")), rec(200, page("/a")), rec(300, page("/b"))}
	quiet := render(t, recs, false)
	loud := render(t, recs, true)

	if strings.Contains(quiet, "Per page:") {
		t.Error("non-verbose report should not include the per-page breakdown")
	}
	if !strings.Contains(loud, "Per page:") {
		t.Error("verbose report should include the per-page breakdown")
	}
	if !strings.Contains(loud, "/a") || !strings.Contains(loud, "/b") {
		t.Errorf("verbose report should list both pages\n---\n%s", loud)
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KB"},
		{5 * 1024 * 1024, "5.0 MB"},
	}
	for _, tc := range tests {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{30, "30s"},
		{600, "10m"},
		{7200, "2.0h"},
		{86400 * 3, "3.0d"},
	}
	for _, tc := range tests {
		if got := humanDuration(tc.in); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
