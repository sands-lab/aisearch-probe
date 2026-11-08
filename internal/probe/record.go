// Package probe holds everything shared between the collector (proxy) and the
// reader (analyze): the on-disk log schema, bot classification, and the
// analysis itself.
//
// The schema lives here and ONLY here. nginx/logging-snippet.conf emits the
// same field names by hand and is the one copy the compiler cannot check —
// if you add a field, add it there too or Mode A silently logs nothing for it.
package probe

import (
	"net/url"
	"strings"
	"time"
)

// Record is one logged HTTP request. Both collection modes write this shape as
// newline-delimited JSON.
type Record struct {
	TS     string  `json:"ts"`      // RFC3339, when the request ARRIVED
	TSUnix float64 `json:"ts_unix"` // same instant, seconds since epoch

	Method     string `json:"method"`
	URL        string `json:"url"`      // full absolute URL as the client asked for it
	PageKey    string `json:"page_key"` // path only, no query — the identity of "a page"
	StatusCode int    `json:"status_code"`

	Bot       string `json:"bot"`
	UserAgent string `json:"user_agent"`
	ClientIP  string `json:"client_ip"`

	// What the client asked us. Non-empty in either field means the client
	// asked "has this changed?" instead of demanding the whole page.
	IfNoneMatch     string `json:"if_none_match"`
	IfModifiedSince string `json:"if_modified_since"`
	CacheControlReq string `json:"cache_control_req"`

	// What we gave them. Non-empty ETag or Last-Modified means we stamped
	// their copy, so on their NEXT visit they are in a position to ask.
	ETagSent         string `json:"etag_sent"`
	LastModifiedSent string `json:"last_modified_sent"`
	CacheControlResp string `json:"cache_control_resp"`
	ExpiresSent      string `json:"expires_sent"` // the cache-header test's Expires settings are graded from this
	BytesSent        int64  `json:"bytes_sent"`

	// Cloudflare's own view. A cache HIT here means the edge answered on our
	// behalf, so this record describes the CDN's behaviour and not the bot's.
	CFCacheStatus string `json:"cf_cache_status"`
	CFRay         string `json:"cf_ray"`

	Referer  string  `json:"referer"`
	ElapsedS float64 `json:"elapsed_s"`

	// Every request header, verbatim. The named fields above are the ones the
	// analysis reads; this is here because a header you did not record is one
	// you cannot go back for, and what a crawler sends is itself the object of
	// study. nginx (Mode A) cannot produce this, so it may be absent.
	ReqHeaders map[string]string `json:"req_headers,omitempty"`
}

// AskedIfChanged reports whether the client tried to revalidate rather than
// demanding the full body ("has this changed?").
func (r Record) AskedIfChanged() bool {
	return r.IfNoneMatch != "" || r.IfModifiedSince != ""
}

// Stamped reports whether our response gave the client something to revalidate
// against next time. If this is false, the client cannot be blamed for
// re-downloading on its next visit — that is our failure, not theirs.
func (r Record) Stamped() bool {
	return r.ETagSent != "" || r.LastModifiedSent != ""
}

// EdgeServed reports whether Cloudflare answered this from its own cache,
// which makes the record unusable as evidence about the bot.
func (r Record) EdgeServed() bool {
	return strings.HasPrefix(strings.ToUpper(r.CFCacheStatus), "HIT")
}

// Time is the instant the request arrived.
func (r Record) Time() time.Time {
	if r.TSUnix > 0 {
		sec := int64(r.TSUnix)
		return time.Unix(sec, int64((r.TSUnix-float64(sec))*1e9)).UTC()
	}
	if t, err := time.Parse(time.RFC3339Nano, r.TS); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// Normalize fills in fields that a collector may not have written. nginx (Mode
// A) cannot classify bots and has no native epoch-with-fraction variable in
// every build, so both are recovered here at read time. Without this, Mode A
// logs analyse as one nameless bot with every revisit interval equal to zero.
func (r *Record) Normalize() {
	// Re-derive when the stored value is empty (nginx never writes one) OR
	// "unknown" (an older collector ran before the signature existed). Without
	// the second case, adding a signature would not reclassify the historical
	// logs it was added for — which is the whole point of classifying at read
	// time. A name the collector actually recognised is never overwritten.
	if r.Bot == "" || r.Bot == "unknown" {
		r.Bot = ClassifyBot(r.UserAgent)
	}
	if r.TSUnix == 0 && r.TS != "" {
		if t, err := time.Parse(time.RFC3339Nano, r.TS); err == nil {
			r.TSUnix = float64(t.UnixNano()) / 1e9
		}
	}
	if r.PageKey == "" && r.URL != "" {
		if u, err := url.Parse(r.URL); err == nil {
			r.PageKey = u.Path
		}
	}
	// nginx logs $request_uri for page_key in older snippets; strip any query
	// so both modes agree on what "a page" is.
	if i := strings.IndexByte(r.PageKey, '?'); i >= 0 {
		r.PageKey = r.PageKey[:i]
	}
}
