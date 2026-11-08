package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"aisearch-probe/internal/probe"
)

// The design the user agreed (2026-10-07): 11 settings. Every "don't reuse without
// asking" setting runs with AND without version info, a full grid so each can be
// compared both ways (for no-store/private/expired the spec expects no difference,
// so only a difference is worth reporting); the timed settings run with the date;
// no header at all runs with the old date, a recent date, and none (the date sets
// the spec's suggested limit).
func TestCcDepthArmsAreTheDesignedNineteen(t *testing.T) {
	want := []string{
		"cc-nostore-tag", "cc-nostore-notag",
		"cc-private-tag", "cc-private-notag",
		"cc-nocache-tag", "cc-nocache-notag",
		"cc-maxage0-tag", "cc-maxage0-notag",
		"cc-combo-tag", "cc-combo-notag",
		"cc-expired-tag", "cc-expired-notag",
		"cc-max60-tag", "cc-max60reval-tag", "cc-smax60-tag", "cc-max3600-tag",
		"cc-none-tag", "cc-none-recent", "cc-none-notag",
	}
	arms := ccDepthArms(2048)
	if len(arms) != len(want) {
		t.Fatalf("got %d arms, want %d", len(arms), len(want))
	}
	for i, a := range arms {
		if a.Label != want[i] {
			t.Errorf("arm %d = %q, want %q", i, a.Label, want[i])
		}
		if _, ok := probe.CacheSettings[a.Setting]; !ok {
			t.Errorf("%s: setting %q is not in probe.CacheSettings", a.Label, a.Setting)
		}
		if a.Code == nil {
			t.Errorf("%s: no per-page code — the read must prove the model saw THIS page", a.Label)
		}
	}
}

// Every address must be the cache route with its two words, the page id (the
// fetch join keys on it) and the size — and stay under Anthropic's 250-character
// URL limit even with a long model tag.
func TestCcDepthArmsBuildTheRightAddresses(t *testing.T) {
	provs := []Caller{{Name: "openrouter-anthropic", Model: "anthropic/claude-sonnet-5-5-20260501"}}
	pages := buildTTLPages(ccDepthArms(2048), provs, "https://probe.example.org", "run20261007-120000-abcde", 12, "secret", "")
	seen := map[string]bool{}
	for _, pg := range pages {
		if seen[pg.PageID] {
			t.Errorf("duplicate page id %q", pg.PageID)
		}
		seen[pg.PageID] = true
		frag := "/cacheHeaders/" + pg.Setting + "/" + string(pg.Validators) + "/" + pg.PageID + "/2048"
		if !strings.HasSuffix(pg.URL, frag) {
			t.Errorf("%s: url %q, want it to end %q", pg.Arm, pg.URL, frag)
		}
		if len(pg.URL) >= 250 {
			t.Errorf("%s: url is %d chars — Anthropic rejects 250+", pg.Arm, len(pg.URL))
		}
		if want := probe.DepthCode("secret", pg.PageID, probe.DepthPercents[len(probe.DepthPercents)-1], probe.DepthPageRevision); pg.Marker != want {
			t.Errorf("%s: marker %q, want the deepest per-page code", pg.Arm, pg.Marker)
		}
	}
}

// gradeCacheReuse is the verdict. A reuse is a poll where NO fetch reached us and
// the answer still carried the page's code; it is past the limit when its gap
// exceeds what RFC 9111 allows for the headers we actually sent (read from the log).
func TestGradeCacheReuse(t *testing.T) {
	warm := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	fixedLM := probe.PageLastModified.Format(http.TimeFormat)
	page := func(setting string, v probe.ValidatorMode, servedCC, servedExp, servedLM string, polls ...ttlPoll) *ttlPage {
		return &ttlPage{
			Arm: "cc-" + setting + "-" + string(v), Setting: setting, Validators: v,
			WarmUnix: unixf(warm), WarmFetches: 1, ServedOK: true,
			Served: probe.Record{TSUnix: unixf(warm), CacheControlResp: servedCC, ExpiresSent: servedExp, LastModifiedSent: servedLM, ETagSent: map[bool]string{true: "", false: `"e"`}[v == probe.ValidatorsNone]},
			Polls:  polls,
		}
	}
	held := func(min float64, marker bool) ttlPoll { return ttlPoll{GapSec: min * 60, Marker: marker} }
	fetched := func(min float64) ttlPoll { return ttlPoll{GapSec: min * 60, Marker: true, Fetches: 1} }

	cases := []struct {
		name        string
		pg          *ttlPage
		wantJudged  bool
		wantPast    bool
		wantLongest float64 // minutes
		wantKind    probe.LimitKind
	}{
		{"no-store reused at 5 min is past the limit",
			page("nostore", probe.ValidatorsOld, "no-store", "", fixedLM, held(5, true)), true, true, 5, probe.LimitMust},
		{"a held poll without the code is not a reuse",
			page("nostore", probe.ValidatorsOld, "no-store", "", fixedLM, held(5, false)), true, false, 0, probe.LimitMust},
		{"a re-fetch every time obeys no-cache",
			page("nocache", probe.ValidatorsNone, "no-cache", "", "", fetched(3)), true, false, 0, probe.LimitMust},
		{"max-age=3600 reused at 30 min is within",
			page("max3600", probe.ValidatorsOld, "max-age=3600", "", fixedLM, held(30, true)), true, false, 30, probe.LimitMust},
		{"max-age=60 reused at 3 min is past",
			page("max60", probe.ValidatorsOld, "max-age=60", "", fixedLM, held(3, true)), true, true, 3, probe.LimitMust},
		{"past Expires reused at 3 min is past",
			page("expired", probe.ValidatorsOld, "", probe.CacheSettings["expired"].Expires, fixedLM, held(3, true)), true, true, 3, probe.LimitMust},
		{"no header + old date: weeks are allowed, 30 min is within",
			page("none", probe.ValidatorsOld, "", "", fixedLM, held(30, true)), true, false, 30, probe.LimitSuggested},
		{"no header + changed 2h ago: 12 min suggested, 30 min is past",
			page("none", probe.ValidatorsRecent, "", "", warm.Add(-2*time.Hour).Format(http.TimeFormat), held(30, true)), true, true, 30, probe.LimitSuggested},
		{"no header and no date: nothing to be past",
			page("none", probe.ValidatorsNone, "", "", "", held(30, true)), true, false, 30, probe.LimitNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := gradeCacheReuse(c.pg)
			if g.Judged != c.wantJudged || g.Past != c.wantPast {
				t.Errorf("judged/past = %v/%v, want %v/%v (%s)", g.Judged, g.Past, c.wantJudged, c.wantPast, g.Limit.Why)
			}
			if got := g.LongestReuseS / 60; got != c.wantLongest {
				t.Errorf("longest reuse = %v min, want %v", got, c.wantLongest)
			}
			if g.Limit.Kind != c.wantKind {
				t.Errorf("limit kind = %v, want %v", g.Limit.Kind, c.wantKind)
			}
			if g.Drift != "" {
				t.Errorf("unexpected drift: %s", g.Drift)
			}
		})
	}

	// A warm that never reached us cannot be graded.
	pg := page("nostore", probe.ValidatorsOld, "no-store", "", fixedLM, held(5, true))
	pg.WarmFetches, pg.ServedOK = 0, false
	if g := gradeCacheReuse(pg); g.Judged {
		t.Error("a page whose warm never fetched must not be graded")
	}
	// What we served must be what the arm declared, or the grade is about the wrong header.
	pg = page("nostore", probe.ValidatorsOld, "max-age=3600", "", fixedLM, held(5, true))
	if g := gradeCacheReuse(pg); g.Drift == "" {
		t.Error("served Cache-Control differs from the arm's setting — must be flagged")
	}
	pg = page("none", probe.ValidatorsNone, "", "", "", held(5, true))
	pg.Served.ETagSent = `"e"`
	if g := gradeCacheReuse(pg); g.Drift == "" {
		t.Error("a notag page that was served with an ETag must be flagged")
	}
	// Ungraded arms (every non-cache run) are never judged.
	if g := gradeCacheReuse(&ttlPage{Arm: "page", WarmFetches: 1, ServedOK: true}); g.Judged {
		t.Error("an arm with no cache setting must not be graded")
	}
}

// Day 2 (2026-10-14): every ask on a cache-header page NAMES one code, cycling the
// page's depths so no ask repeats an earlier one until all five are used; an answer
// counts as read only if it carries exactly that code.
func TestCcDepthAsksRotateTheNamedCode(t *testing.T) {
	want := []int{0, 25, 50, 75, 95, 0, 25} // first ask + 6 re-asks
	for ask, pct := range want {
		if got := rotatedDepth(ask); got != pct {
			t.Errorf("ask %d asks for depth %d%%, want %d%%", ask, got, pct)
		}
	}
	// Since 2026-10-25 the named-code question IS the standard question (D14).
	for _, pct := range probe.DepthPercents {
		if rotatedQuestion(pct) != standardQuestion(pct) {
			t.Errorf("rotatedQuestion(%d) = %q, want the standard question", pct, rotatedQuestion(pct))
		}
	}

	pages := buildTTLPages(ccDepthArms(2048), []Caller{{Name: "openai", Model: "gpt-5-mini"}}, "https://p.example", "run1", 1, "secret", "RCP-2026-08")
	pg := pages[0]
	for ask, pct := range want {
		question, marker := pg.askFor(ask)
		if wantCode := probe.DepthCode("secret", pg.PageID, pct, probe.DepthPageRevision); marker != wantCode {
			t.Errorf("ask %d: marker %q, want the depth-%d%% code %q", ask, marker, pct, wantCode)
		}
		if question != rotatedQuestion(pct) {
			t.Errorf("ask %d: question %q, want %q", ask, question, rotatedQuestion(pct))
		}
	}
}

// The Anthropic no-store follow-up (2026-10-18) runs 5 of the 19 pages. selectArms
// keeps the given labels in the list's own order and refuses a label it does not
// know, so a typo cannot silently drop a page from a paid run.
func TestSelectArms(t *testing.T) {
	all := ccDepthArms(2048)
	got, err := selectArms(all, "cc-none-tag, cc-nostore-tag,cc-private-tag")
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, a := range got {
		labels = append(labels, a.Label)
	}
	if strings.Join(labels, ",") != "cc-nostore-tag,cc-private-tag,cc-none-tag" {
		t.Errorf("got %v, want the list's own order", labels)
	}
	if _, err := selectArms(all, "cc-nostore-tag,cc-nostor-tag"); err == nil {
		t.Error("an unknown label must be an error, not a silently smaller run")
	}
	if got, _ := selectArms(all, ""); len(got) != len(all) {
		t.Error("an empty selection keeps every arm")
	}
}

// By default a page stops being asked once the vendor fetches it again (its copy's
// clock restarted). With keep-asking on — the follow-up's whole point — it is asked
// at every gap, so "fetches fresh every time" can be told from "fetched once".
func TestRetireAfterRefetch(t *testing.T) {
	defer func() { ttlmapKeepAsking = false }()
	ttlmapKeepAsking = false
	if !retireAfter(true) || retireAfter(false) {
		t.Error("default: retire on a fetch-again, keep a held page")
	}
	ttlmapKeepAsking = true
	if retireAfter(true) || retireAfter(false) {
		t.Error("keep-asking: never retire")
	}
}
