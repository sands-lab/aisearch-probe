package probe

import (
	"strings"
	"testing"
)

func TestEdgeCapFromCacheControl(t *testing.T) {
	cases := []struct {
		cc   string
		want float64
	}{
		{"public, max-age=3600", 60},
		{"max-age=60", 1},
		{"no-store", 0},
		{"no-cache", 0},
		{"private, max-age=3600", 0}, // a shared cache may not serve it at all
		{"public, s-maxage=120, max-age=3600", 2},
		{"", -1},
		{"immutable", -1},
	}
	for _, c := range cases {
		if got := edgeCapMinutes(c.cc); got != c.want {
			t.Errorf("edgeCapMinutes(%q) = %v, want %v", c.cc, got, c.want)
		}
	}
}

// The whole point: a reuse longer than the edge was allowed to serve cannot be
// the CDN, and one inside that window must NOT be counted as evidence.
func TestEdgeAuditSeparatesWhatTheCDNCouldExplain(t *testing.T) {
	page := "https://x.trycloudflare.com/method.html"
	mk := func(prov, tok string, at float64) Trial {
		return Trial{Provider: prov, URL: page, Token: tok, SentUnix: at}
	}
	rec := func(tok string, at float64, cc string) Record {
		return Record{
			TSUnix: at, PageKey: "/method.html", StatusCode: 200,
			URL: page + "?" + tok, CacheControlResp: cc,
		}
	}
	trials := []Trial{
		mk("openai", "probe=openai", 0),      // fetch
		mk("openai", "probe=openai", 20*60),  // reuse at 20 min — inside 1 h
		mk("openai", "probe=openai", 120*60), // reuse at 120 min — beyond 1 h
	}
	recs := []Record{rec("probe=openai", 1, "public, max-age=3600")}

	obs := EdgeAudit(trials, recs)
	if len(obs) != 2 {
		t.Fatalf("expected 2 reuse observations, got %d", len(obs))
	}
	if !obs[0].EdgeExplains {
		t.Error("reuse at 20 min under max-age=3600 IS edge-explainable; must not be claimed as evidence")
	}
	if obs[1].EdgeExplains {
		t.Error("reuse at 120 min under max-age=3600 cannot be the edge")
	}

	var b strings.Builder
	WriteEdgeAudit(&b, obs)
	out := b.String()
	if !strings.Contains(out, "A CDN CANNOT account for                      : 1") {
		t.Errorf("the report should count exactly one solid observation:\n%s", out)
	}
}

// If EVERY reuse falls inside the edge's window, the report must say the results
// are not yet attributable — not quietly present them as findings.
func TestEdgeAuditRefusesToClaimWhenEverythingIsExplainable(t *testing.T) {
	page := "https://x.trycloudflare.com/p.html"
	trials := []Trial{
		{Provider: "openai", URL: page, Token: "probe=openai", SentUnix: 0},
		{Provider: "openai", URL: page, Token: "probe=openai", SentUnix: 600},
	}
	recs := []Record{{
		TSUnix: 1, PageKey: "/p.html", StatusCode: 200,
		URL: page + "?probe=openai", CacheControlResp: "public, max-age=3600",
	}}
	var b strings.Builder
	WriteEdgeAudit(&b, EdgeAudit(trials, recs))
	out := b.String()
	if !strings.Contains(out, "NOTHING here rules out the CDN") {
		t.Errorf("must refuse to claim attribution; got:\n%s", out)
	}
}

// An unknown directive must count AGAINST the finding, not for it.
func TestUnknownDirectiveIsTreatedAsEdgeExplainable(t *testing.T) {
	page := "https://x.trycloudflare.com/p.html"
	trials := []Trial{
		{Provider: "openai", URL: page, Token: "probe=openai", SentUnix: 0},
		{Provider: "openai", URL: page, Token: "probe=openai", SentUnix: 99999},
	}
	recs := []Record{{TSUnix: 1, PageKey: "/p.html", StatusCode: 200,
		URL: page + "?probe=openai", CacheControlResp: ""}}
	obs := EdgeAudit(trials, recs)
	if len(obs) != 1 || !obs[0].EdgeExplains {
		t.Fatalf("with no known directive the observation must be treated as explainable: %+v", obs)
	}
}
