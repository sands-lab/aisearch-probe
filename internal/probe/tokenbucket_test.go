package probe

import (
	"strings"
	"testing"
)

// unknownRec builds a request the user-agent could not name, at the given URL.
func unknownRec(url string) Record {
	r := rec(1000, bot("unknown"), page("/m.html"))
	r.URL = url
	r.UserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/143.0.0.0 Safari/537.36"
	return r
}

// The headline case: a fetch that forged an ordinary-browser user-agent, but
// carried the secret token we planted in xAI's URL and nobody else's, must be
// attributed to xAI — not left in the catch-all unknown bucket.
func TestUnknownWithPlantedTokenIsAttributed(t *testing.T) {
	trials := []Trial{{Provider: "or:x-ai", Model: "grok-4", Token: "probe=or-x-ai-grok-4-run1-abc"}}
	r := unknownRec("https://x.test/m.html?probe=or-x-ai-grok-4-run1-abc")

	got := AttributeUnknownByToken([]Record{r}, trials)
	if got[0].Bot == "unknown" {
		t.Fatal("a forged-UA request carrying xAI's planted token must not stay unknown")
	}
	if !IsProbeTagBucket(got[0].Bot) {
		t.Errorf("bucket must be marked as token-attributed, got %q", got[0].Bot)
	}
	if !strings.Contains(got[0].Bot, "grok-4") {
		t.Errorf("bucket must name the provider and model, got %q", got[0].Bot)
	}
}

// Genuine internet noise: an unknown user-agent hitting a random path with no
// token of ours must stay in the background/unknown bucket, never attributed.
func TestScannerWithoutTokenStaysUnknown(t *testing.T) {
	trials := []Trial{{Provider: "or:x-ai", Model: "grok-4", Token: "probe=or-x-ai-grok-4-run1-abc"}}
	r := unknownRec("https://x.test/wp-login.php")
	r.UserAgent = "curl/8.0"

	got := AttributeUnknownByToken([]Record{r}, trials)
	if got[0].Bot != "unknown" {
		t.Errorf("background traffic with no planted token must stay unknown, got %q", got[0].Bot)
	}
}

// No regression: a crawler that DID name itself keeps its user-agent identity,
// even if the address happens to carry a token. User-agent alone stays as is;
// only requests we could not otherwise name are reclassified by token.
func TestDeclaredCrawlerIsNotStolenByToken(t *testing.T) {
	trials := []Trial{{Provider: "or:anthropic", Model: "claude", Token: "probe=or-anthropic-claude-run1"}}
	r := rec(1000, bot("anthropic-query"), page("/m.html"))
	r.URL = "https://x.test/m.html?probe=or-anthropic-claude-run1"

	got := AttributeUnknownByToken([]Record{r}, trials)
	if got[0].Bot != "anthropic-query" {
		t.Errorf("a self-declared crawler must keep its user-agent identity, got %q", got[0].Bot)
	}
}

// We do not assume a vendor's fetcher behaves the same across its models, so two
// models at one vendor must land in separate buckets.
func TestModelsKeptSeparateByToken(t *testing.T) {
	trials := []Trial{
		{Provider: "or:x-ai", Model: "grok-4", Token: "probe=or-x-ai-grok-4-r1"},
		{Provider: "or:x-ai", Model: "grok-2", Token: "probe=or-x-ai-grok-2-r1"},
	}
	got := AttributeUnknownByToken([]Record{
		unknownRec("https://x.test/m.html?probe=or-x-ai-grok-4-r1"),
		unknownRec("https://x.test/m.html?probe=or-x-ai-grok-2-r1"),
	}, trials)
	if got[0].Bot == got[1].Bot {
		t.Fatalf("two models at one vendor must not share a bucket: both %q", got[0].Bot)
	}
}

// The old logs hold tokens without a random tail, so one token can be a prefix
// of another. The longer, more specific token must win.
func TestLongerTokenWinsOverPrefix(t *testing.T) {
	trials := []Trial{
		{Provider: "openai", Token: "probe=openai"},
		{Provider: "openai", Model: "gpt-5-mini", Token: "probe=openai-gpt-5-mini"},
	}
	r := unknownRec("https://x.test/m.html?probe=openai-gpt-5-mini")

	got := AttributeUnknownByToken([]Record{r}, trials)
	if !strings.Contains(got[0].Bot, "gpt-5-mini") {
		t.Errorf("the longer, more specific token must win, got %q", got[0].Bot)
	}
}

// With no trials there is no notebook of planted tokens, so nothing is
// reclassified — behaviour is exactly as before.
func TestNoTrialsLeavesRecordsUnchanged(t *testing.T) {
	r := unknownRec("https://x.test/m.html?probe=or-x-ai-grok-4-r1")
	got := AttributeUnknownByToken([]Record{r}, nil)
	if got[0].Bot != "unknown" {
		t.Errorf("with no trial log, an unknown must stay unknown, got %q", got[0].Bot)
	}
}

// End to end through the report: the attributed bucket is shown, and the report
// says plainly that its identity rests on the planted code, not on what the
// request declared. Reverting the split leaves the request in unknown and fails
// this test.
func TestReportMarksTokenAttributedBucket(t *testing.T) {
	trials := []Trial{{Provider: "or:x-ai", Model: "grok-4", Token: "probe=or-x-ai-grok-4-r1"}}
	recs := AttributeUnknownByToken(
		[]Record{unknownRec("https://x.test/m.html?probe=or-x-ai-grok-4-r1")}, trials)

	var sb strings.Builder
	WriteReport(&sb, Analyze(recs), false)
	out := sb.String()

	if !strings.Contains(out, "grok-4") {
		t.Errorf("report must show the attributed bucket\n%s", out)
	}
	if !strings.Contains(out, "secret code") {
		t.Errorf("report must explain the identity rests on the planted code\n%s", out)
	}
}
