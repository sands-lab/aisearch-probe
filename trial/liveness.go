package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// THE FAILURE THAT WOULD INVERT THE HEADLINE FINDING.
//
// The study's central claim has this shape: we submitted a query, no request
// arrived at our server, and the answer proved the model held our content — so
// it reused a cached copy.
//
// Now suppose the page was simply unreachable — a quick tunnel dies with its
// terminal, and a long run spans hours. The provider tries to fetch, gets
// nothing, our server logs nothing, and the model answers from its cache. The
// data is bit-for-bit identical to the finding above, and the conclusion drawn
// from it ("still cached at T+8h") is wrong: we never learned whether it would
// have re-fetched, because there was nothing to fetch.
//
// A pilot that fits in one sitting is not exposed to this. A run that spans
// hours is exposed to it constantly, and it is invisible after the fact — the
// absence of a request looks the same either way.
//
// So every round records, independently of any provider, whether OUR OWN GET of
// the page succeeded and returned the marker. A round where it did not measures
// nothing and must never be scored.

// PageLiveness is what we could see of our own page at the moment of a round.
type PageLiveness struct {
	OK      bool   `json:"ok"`
	Status  int    `json:"status,omitempty"`
	Marker  bool   `json:"marker,omitempty"` // the page we got back still carries the marker
	Err     string `json:"err,omitempty"`
	CheckAt string `json:"checked_at,omitempty"`
}

// livenessUA identifies these requests in the probe log so they are attributed
// to the instrument rather than swelling the "unknown" bucket. bots.go carries
// the matching signature.
const livenessUA = "probe-liveness/1 (+probe-trial)"

// checkPageLive fetches the bare page URL — no provider, no token — and reports
// what came back. Deliberately not fatal: a round that cannot see the page is
// recorded as such and the run continues, because the tunnel may come back and
// the later rounds are still worth having.
func checkPageLive(ctx context.Context, pageURL, marker string) PageLiveness {
	l := PageLiveness{CheckAt: time.Now().UTC().Format(time.RFC3339)}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		l.Err = err.Error()
		return l
	}
	req.Header.Set("User-Agent", livenessUA)
	// No conditional headers and no caching on our side: we want to know that
	// the page is being served right now, not that we once saw it.
	req.Header.Set("Cache-Control", "no-cache")

	// probeHTTPClient, not the default client: this network cannot resolve
	// *.trycloudflare.com, and the default resolver made a live page look dead.
	resp, err := probeHTTPClient(30 * time.Second).Do(req)
	if err != nil {
		l.Err = err.Error()
		return l
	}
	defer resp.Body.Close()
	l.Status = resp.StatusCode

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		l.Err = "reading body: " + err.Error()
		return l
	}
	l.Marker = containsFold(string(body), marker)
	// A 200 that no longer contains the marker is its own kind of broken: the
	// tunnel address may have been recycled to somebody else's service, and the
	// page providers are fetching is then not our page at all.
	l.OK = resp.StatusCode == http.StatusOK && l.Marker
	if !l.OK && l.Err == "" {
		if resp.StatusCode != http.StatusOK {
			l.Err = fmt.Sprintf("HTTP %d", resp.StatusCode)
		} else {
			l.Err = "page served but the marker is gone — is this still our page?"
		}
	}
	return l
}

// String renders the one line a run operator needs to see.
func (l PageLiveness) String() string {
	if l.OK {
		return "page reachable, marker present"
	}
	if l.Err != "" {
		return "PAGE NOT USABLE: " + strings.TrimSpace(l.Err)
	}
	return "PAGE NOT USABLE"
}
