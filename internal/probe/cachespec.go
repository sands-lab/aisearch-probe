package probe

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The cache-header test, stated once.
//
// /ccdepthpage/<setting>/<validators>/<id>/<bytes> serves the shared depth content
// under one response-header setting. These tables are the ONLY place a test cache
// setting is defined: the proxy serves from them, the trial runner builds its pages
// and grades its results from them. Every other generated page sends no
// Cache-Control at all (DECISIONS.md § D13) — a test setting must never leak into
// another experiment's pages.
//
// The grading rule is RFC 9111 (HTTP Caching) read for a SHARED cache: a store
// that hands one copy to many users, which is what an AI vendor's fetch cache is.
// See doc/cache-directive-test.md for the section-by-section reading.

// PageLastModified is the fixed "last changed" date every generated page carries.
// Fixed so the bytes, and so the ETag, never drift. It is nine months before the
// cache-header test, which matters: with no Cache-Control, RFC 9111 § 4.2.2 lets a
// cache reuse such a page for about a tenth of that age.
var PageLastModified = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// CacheSetting is the caching headers one test setting sends. "" omits a header.
type CacheSetting struct {
	CacheControl string
	Expires      string // an HTTP date
}

// CacheSettings maps the setting word in the address to the headers it sends.
// Chosen from RFC 9111, with the most common real-world combination (from the
// survey of the 619 reachable sites in the CrUX top 2,000, logs/realpage-markers/cache_headers.jsonl)
// added as "combo".
var CacheSettings = map[string]CacheSetting{
	"nostore":    {CacheControl: "no-store"},
	"private":    {CacheControl: "private"},
	"nocache":    {CacheControl: "no-cache"},
	"maxage0":    {CacheControl: "max-age=0"},
	"combo":      {CacheControl: "no-cache, no-store, max-age=0, must-revalidate"},
	"expired":    {Expires: PageLastModified.Format(http.TimeFormat)},
	"max60":      {CacheControl: "max-age=60"},
	"max60reval": {CacheControl: "max-age=60, must-revalidate"},
	"smax60":     {CacheControl: "max-age=3600, s-maxage=60"},
	"max3600":    {CacheControl: "max-age=3600"},
	"none":       {},
}

// CacheSettingOrder is CacheSettings in a fixed order, strictest first, for
// building runs and printing reports.
var CacheSettingOrder = []string{
	"nostore", "private", "nocache", "maxage0", "combo", "expired",
	"max60", "max60reval", "smax60", "max3600", "none",
}

// ValidatorMode is the version-info word in the address: whether the page carries
// an ETag and Last-Modified, and which last-changed date.
type ValidatorMode string

const (
	// ValidatorsOld sends an ETag and Last-Modified = PageLastModified (the normal page).
	ValidatorsOld ValidatorMode = "tag"
	// ValidatorsRecent sends an ETag and Last-Modified = the time the origin started,
	// so a cache's tenth-of-the-age guess shrinks to minutes.
	ValidatorsRecent ValidatorMode = "recent"
	// ValidatorsNone sends neither, so "check with the site" can only mean a full
	// re-download. Most real sites look like this (62% of the survey).
	ValidatorsNone ValidatorMode = "notag"
)

// ValidatorModes lists every mode the route accepts.
var ValidatorModes = []ValidatorMode{ValidatorsOld, ValidatorsRecent, ValidatorsNone}

// LimitKind says how firm a reuse limit is.
type LimitKind int

const (
	// LimitNone: the spec states no limit (no explicit time and no date to guess from).
	LimitNone LimitKind = iota
	// LimitMust: a MUST in RFC 9111 — reuse past it without checking breaks the rule.
	LimitMust
	// LimitSuggested: the § 4.2.2 guess, which caches are only "encouraged" to follow.
	LimitSuggested
)

func (k LimitKind) String() string {
	switch k {
	case LimitMust:
		return "must"
	case LimitSuggested:
		return "suggested"
	default:
		return "none stated"
	}
}

// ReuseLimit is the longest a shared cache may reuse a response without checking
// with the site, and how firm that is.
type ReuseLimit struct {
	Max  time.Duration
	Kind LimitKind
	Why  string // one plain sentence for the report
}

// heuristicFraction is the RFC 9111 § 4.2.2 suggestion: "A typical setting of this
// fraction might be 10%."
const heuristicFraction = 0.10

// SharedReuseLimit applies RFC 9111 to the headers a response carried, from the
// point of view of a shared cache. date is when the response was sent;
// lastModified is zero when no Last-Modified was sent.
func SharedReuseLimit(cacheControl, expires string, date, lastModified time.Time) ReuseLimit {
	d := parseCacheControl(cacheControl)
	switch {
	case d.has("no-store"):
		return ReuseLimit{0, LimitMust, "no-store: a cache must not store it at all"}
	case d.unqualified("private"):
		return ReuseLimit{0, LimitMust, "private: a shared cache must not store it"}
	case d.unqualified("no-cache"):
		return ReuseLimit{0, LimitMust, "no-cache: must check with the site before every reuse"}
	}
	// § 4.2.1 precedence: s-maxage (shared caches), then max-age, then Expires.
	for _, key := range []string{"s-maxage", "max-age"} {
		if v, ok := d.seconds(key); ok {
			lim := time.Duration(v) * time.Second
			return ReuseLimit{lim, LimitMust, fmt.Sprintf("%s=%d: reuse for %s, then check", key, v, lim)}
		}
	}
	if expires != "" {
		// § 5.3: an invalid date, "0" especially, means already expired.
		exp, err := http.ParseTime(expires)
		if err != nil || !exp.After(date) {
			return ReuseLimit{0, LimitMust, "Expires is in the past: already out of date"}
		}
		lim := exp.Sub(date)
		return ReuseLimit{lim, LimitMust, fmt.Sprintf("Expires: reuse for %s, then check", lim.Round(time.Second))}
	}
	if !lastModified.IsZero() && date.After(lastModified) {
		lim := time.Duration(float64(date.Sub(lastModified)) * heuristicFraction)
		return ReuseLimit{lim, LimitSuggested, fmt.Sprintf("no explicit time: a tenth of the %s since it last changed", humanDuration(date.Sub(lastModified).Seconds()))}
	}
	return ReuseLimit{0, LimitNone, "no explicit time and no last-changed date: the spec leaves it to the cache"}
}

// cacheDirectives is a parsed Cache-Control: lowercased name -> argument ("" if none).
type cacheDirectives map[string]string

// parseCacheControl splits a Cache-Control value into directives (RFC 9111 § 5.2).
// Names are case-insensitive and arguments may be quoted. The first occurrence of a
// repeated directive wins (§ 4.2.1).
func parseCacheControl(v string) cacheDirectives {
	d := cacheDirectives{}
	for _, part := range splitDirectives(v) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, arg, _ := strings.Cut(part, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		arg = strings.Trim(strings.TrimSpace(arg), `"`)
		if _, seen := d[name]; !seen {
			d[name] = arg
		}
	}
	return d
}

// splitDirectives splits on commas outside quotes, so no-cache="a, b" stays whole.
func splitDirectives(v string) []string {
	var out []string
	inQuote, start := false, 0
	for i, c := range v {
		switch c {
		case '"':
			inQuote = !inQuote
		case ',':
			if !inQuote {
				out = append(out, v[start:i])
				start = i + 1
			}
		}
	}
	return append(out, v[start:])
}

func (d cacheDirectives) has(name string) bool { _, ok := d[name]; return ok }

// unqualified reports a directive present with no field-name list. The qualified
// forms of private and no-cache only concern the named header fields, not the body.
func (d cacheDirectives) unqualified(name string) bool {
	arg, ok := d[name]
	return ok && arg == ""
}

func (d cacheDirectives) seconds(name string) (int, bool) {
	arg, ok := d[name]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(arg)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// humanDuration writes a number of seconds as a short, rounded duration.
func humanDuration(s float64) string {
	switch {
	case s < 90:
		return fmt.Sprintf("%.0fs", s)
	case s < 90*60:
		return fmt.Sprintf("%.0fm", s/60)
	case s < 48*3600:
		return fmt.Sprintf("%.1fh", s/3600)
	default:
		return fmt.Sprintf("%.1fd", s/86400)
	}
}
