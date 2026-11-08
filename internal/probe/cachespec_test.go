package probe

import (
	"net/http"
	"regexp"
	"testing"
	"time"
)

// SharedReuseLimit is the RFC 9111 rule the cache-header test grades every vendor
// against. Each case cites the section it comes from, so a wrong expectation is a
// wrong reading of the spec, not a matter of taste.
func TestSharedReuseLimitFollowsRFC9111(t *testing.T) {
	date := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	tenDaysAgo := date.Add(-10 * 24 * time.Hour)
	httpDate := func(t time.Time) string { return t.UTC().Format(http.TimeFormat) }

	cases := []struct {
		name    string
		cc      string
		expires string
		lm      time.Time // zero = no Last-Modified sent
		want    time.Duration
		kind    LimitKind
	}{
		// § 5.2.2.5: no-store — must not store at all.
		{"no-store", "no-store", "", tenDaysAgo, 0, LimitMust},
		// § 5.2.2.7: unqualified private — a SHARED cache must not store it.
		{"private", "private", "", tenDaysAgo, 0, LimitMust},
		// Qualified private only withholds the named fields; the body may be reused.
		{"qualified private does not forbid the body", `private="set-cookie", max-age=60`, "", time.Time{}, 60 * time.Second, LimitMust},
		// § 5.2.2.4: unqualified no-cache — never reuse without checking first.
		{"no-cache", "no-cache", "", tenDaysAgo, 0, LimitMust},
		{"qualified no-cache does not forbid the body", `no-cache="set-cookie", max-age=60`, "", time.Time{}, 60 * time.Second, LimitMust},
		// § 5.2.2.1 + § 4.2.4: max-age=0 is stale at once, and stale may not be served unasked.
		{"max-age=0", "max-age=0", "", tenDaysAgo, 0, LimitMust},
		{"real-world combo", "no-cache, no-store, max-age=0, must-revalidate", "", tenDaysAgo, 0, LimitMust},
		{"max-age=60", "max-age=60", "", tenDaysAgo, 60 * time.Second, LimitMust},
		{"must-revalidate does not change the fresh time", "max-age=60, must-revalidate", "", tenDaysAgo, 60 * time.Second, LimitMust},
		// § 5.2.2.10 + § 4.2.1: s-maxage overrides max-age for a shared cache.
		{"s-maxage wins for a shared cache", "max-age=3600, s-maxage=60", "", tenDaysAgo, 60 * time.Second, LimitMust},
		{"max-age=3600", "max-age=3600", "", tenDaysAgo, time.Hour, LimitMust},
		// § 5.2: directive names are case-insensitive; § 5.2: recipients accept the quoted form.
		{"case-insensitive", "MAX-AGE=60", "", time.Time{}, 60 * time.Second, LimitMust},
		{"quoted argument", `max-age="60"`, "", time.Time{}, 60 * time.Second, LimitMust},
		// § 5.3: Expires minus Date when no max-age.
		{"Expires in the future", "", httpDate(date.Add(2 * time.Minute)), tenDaysAgo, 2 * time.Minute, LimitMust},
		{"Expires in the past", "", httpDate(date.Add(-time.Hour)), tenDaysAgo, 0, LimitMust},
		{"Expires 0 means already expired", "", "0", tenDaysAgo, 0, LimitMust},
		{"Expires garbage means already expired", "", "not a date", tenDaysAgo, 0, LimitMust},
		// § 5.3: max-age present means Expires is ignored.
		{"max-age beats Expires", "max-age=60", httpDate(date.Add(time.Hour)), tenDaysAgo, 60 * time.Second, LimitMust},
		// § 4.2.2: no explicit time — a cache MAY guess; a tenth of the age since
		// Last-Modified is the suggestion ("encouraged", not "MUST").
		{"no header, changed ten days ago", "", "", tenDaysAgo, 24 * time.Hour, LimitSuggested},
		{"public alone is still a guess", "public", "", tenDaysAgo, 24 * time.Hour, LimitSuggested},
		{"no header and no date: the spec states no limit", "", "", time.Time{}, 0, LimitNone},
		// § 5.2.3: an unknown directive is ignored.
		{"unknown directive is ignored", "immutable", "", time.Time{}, 0, LimitNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SharedReuseLimit(c.cc, c.expires, date, c.lm)
			if got.Kind != c.kind {
				t.Errorf("kind = %v, want %v (%s)", got.Kind, c.kind, got.Why)
			}
			if c.kind != LimitNone && got.Max != c.want {
				t.Errorf("limit = %v, want %v (%s)", got.Max, c.want, got.Why)
			}
			if got.Why == "" {
				t.Error("every limit must say why, in words, for the report")
			}
		})
	}
}

// The header each test setting sends is the experiment. Pin every one exactly, so
// a typo cannot quietly turn one setting into another.
func TestCacheSettingsArePinned(t *testing.T) {
	want := map[string]CacheSetting{
		"nostore":    {CacheControl: "no-store"},
		"private":    {CacheControl: "private"},
		"nocache":    {CacheControl: "no-cache"},
		"maxage0":    {CacheControl: "max-age=0"},
		"combo":      {CacheControl: "no-cache, no-store, max-age=0, must-revalidate"},
		"expired":    {Expires: "Thu, 01 Jan 2026 00:00:00 GMT"},
		"max60":      {CacheControl: "max-age=60"},
		"max60reval": {CacheControl: "max-age=60, must-revalidate"},
		"smax60":     {CacheControl: "max-age=3600, s-maxage=60"},
		"max3600":    {CacheControl: "max-age=3600"},
		"none":       {},
	}
	if len(CacheSettings) != len(want) {
		t.Fatalf("CacheSettings has %d entries, want %d", len(CacheSettings), len(want))
	}
	for tok, w := range want {
		got, ok := CacheSettings[tok]
		if !ok {
			t.Errorf("setting %q missing", tok)
			continue
		}
		if got != w {
			t.Errorf("setting %q = %+v, want %+v", tok, got, w)
		}
	}
	if len(CacheSettingOrder) != len(CacheSettings) {
		t.Errorf("CacheSettingOrder lists %d settings, table has %d", len(CacheSettingOrder), len(CacheSettings))
	}
	seen := map[string]bool{}
	for _, tok := range CacheSettingOrder {
		if _, ok := CacheSettings[tok]; !ok || seen[tok] {
			t.Errorf("CacheSettingOrder entry %q is unknown or repeated", tok)
		}
		seen[tok] = true
	}
}

// Tokens travel in the URL path, so they must be plain lowercase words.
func TestCacheTokensArePathSafe(t *testing.T) {
	ok := regexp.MustCompile(`^[a-z0-9]+$`)
	for tok := range CacheSettings {
		if !ok.MatchString(tok) {
			t.Errorf("setting token %q is not a plain lowercase word", tok)
		}
	}
	for _, v := range ValidatorModes {
		if !ok.MatchString(string(v)) {
			t.Errorf("validator token %q is not a plain lowercase word", v)
		}
	}
}

// The design table the user agreed (2026-10-07): what each setting allows a shared
// cache, served with the normal version tag and the fixed old last-changed date.
// This is the grading the report will print, checked end to end.
func TestEverySettingGradesAsDesigned(t *testing.T) {
	date := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	never := []string{"nostore", "private", "nocache", "maxage0", "combo", "expired"}
	for _, tok := range never {
		s := CacheSettings[tok]
		got := SharedReuseLimit(s.CacheControl, s.Expires, date, PageLastModified)
		if got.Kind != LimitMust || got.Max != 0 {
			t.Errorf("%s: got %v/%v, want must / never reuse unchecked", tok, got.Kind, got.Max)
		}
	}
	for tok, want := range map[string]time.Duration{
		"max60": time.Minute, "max60reval": time.Minute, "smax60": time.Minute, "max3600": time.Hour,
	} {
		s := CacheSettings[tok]
		got := SharedReuseLimit(s.CacheControl, s.Expires, date, PageLastModified)
		if got.Kind != LimitMust || got.Max != want {
			t.Errorf("%s: got %v/%v, want must / %v", tok, got.Kind, got.Max, want)
		}
	}
	// No header + the old fixed date: a guess of a tenth of ~9 months, i.e. weeks.
	got := SharedReuseLimit("", "", date, PageLastModified)
	if got.Kind != LimitSuggested || got.Max < 20*24*time.Hour {
		t.Errorf("none with the old date: got %v/%v, want a suggested limit of weeks", got.Kind, got.Max)
	}
}
