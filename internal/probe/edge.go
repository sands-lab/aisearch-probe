package probe

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Answering the CDN objection with arithmetic instead of assurances.
//
// THE OBJECTION. Our page is published through a Cloudflare tunnel, and a CDN is
// itself a cache. When a provider is asked about the page and nothing arrives at
// our server, the study reads that as "the provider reused its own copy". But an
// edge that answered from ITS cache produces exactly the same silence. Every
// reuse observation is, on its face, ambiguous.
//
// WHY THE EXISTING GUARD CANNOT SETTLE IT. Record.EdgeServed() looks for
// CF-Cache-Status: HIT. That can never fire: if the edge answers from its cache,
// the request does not reach the origin, so no log line exists to carry the
// header. Measured over 309 requests, the field was empty every single time. It
// is not a guard against edge caching — it is blind to it by construction.
//
// WHAT DOES SETTLE IT. An edge honours the origin's own freshness instruction.
// So for each reuse observation, compare the gap against the longest an edge
// could legitimately have held that response:
//
//	no-store / no-cache / private   an edge may not serve it at all      -> 0 min
//	max-age=60                      one minute                           -> 1 min
//	max-age=3600                    one hour                             -> 60 min
//
// A reuse observed LONGER than that limit cannot be the edge. It is the
// provider's own copy, and no dashboard is needed to know it.
//
// This is deliberately conservative in the study's disfavour: an observation
// INSIDE the limit is reported as edge-explainable even when other evidence makes
// that implausible, and is not counted as support for any finding.

// ReuseObservation is one query that produced no fetch, with everything needed
// to decide whether a CDN could account for it.
type ReuseObservation struct {
	Provider string
	Host     string
	Page     string
	GapMin   float64 // minutes since that provider last fetched this URL
	Served   string  // the Cache-Control we sent with the page
	EdgeCap  float64 // longest an edge could hold it, minutes; -1 if unknown
	// EdgeExplains is true when a CDN could account for this reuse, i.e. the
	// observation carries no weight on its own.
	EdgeExplains bool
}

// edgeCapMinutes returns the longest a shared cache may serve a response with
// this Cache-Control, in minutes, or -1 when it cannot be determined.
//
// Directives that forbid shared caching outright return 0 rather than -1: "an
// edge may not serve this at all" is knowledge, not absence of it.
func edgeCapMinutes(cacheControl string) float64 {
	cc := strings.ToLower(cacheControl)
	if cc == "" {
		return -1
	}
	for _, d := range []string{"no-store", "no-cache", "private"} {
		if strings.Contains(cc, d) {
			return 0
		}
	}
	// s-maxage overrides max-age for shared caches, so prefer it when present.
	for _, key := range []string{"s-maxage=", "max-age="} {
		if i := strings.Index(cc, key); i >= 0 {
			rest := cc[i+len(key):]
			end := strings.IndexAny(rest, ", ")
			if end < 0 {
				end = len(rest)
			}
			if secs, err := strconv.Atoi(strings.TrimSpace(rest[:end])); err == nil {
				return float64(secs) / 60
			}
		}
	}
	return -1
}

// EdgeAudit walks the trials chronologically and returns every reuse
// observation, each labelled with whether a CDN could account for it.
//
// A trial counts as a fetch when a request carrying that trial's token arrived
// for that page. Trials with no token fall back to any request for the page,
// which is why older logs still audit, if less precisely.
func EdgeAudit(trials []Trial, records []Record) []ReuseObservation {
	sorted := make([]Trial, len(trials))
	copy(sorted, trials)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].SentUnix < sorted[j].SentUnix })

	// What we served, per host+page. Taken from the log rather than assumed,
	// because the directive is the independent variable and changes between runs.
	served := map[string]string{}
	for _, r := range records {
		k := hostOf(r.URL) + r.PageKey
		if v := r.CacheControlResp; v != "" {
			served[k] = v
		}
	}

	const window = 180.0
	var out []ReuseObservation
	lastFetch := map[string]float64{}
	for _, t := range sorted {
		h, pg := hostOf(t.URL), pagePathOf(t.URL)
		var fetched float64
		var any bool
		for _, r := range records {
			if hostOf(r.URL) != h || r.PageKey != pg {
				continue
			}
			if r.TSUnix < t.SentUnix || r.TSUnix > t.SentUnix+window {
				continue
			}
			if t.Token != "" && !strings.Contains(r.URL, t.Token) {
				continue
			}
			if r.TSUnix > fetched {
				fetched, any = r.TSUnix, true
			}
		}
		key := t.Provider + "|" + h + "|" + pg
		if prev, ok := lastFetch[key]; ok && !any {
			cc := served[h+pg]
			cap := edgeCapMinutes(cc)
			gap := (t.SentUnix - prev) / 60
			out = append(out, ReuseObservation{
				Provider: t.Provider, Host: h, Page: pg,
				GapMin: gap, Served: cc, EdgeCap: cap,
				// Unknown directive is treated as edge-explainable: we cannot
				// prove otherwise, and the burden belongs on the finding.
				EdgeExplains: cap < 0 || gap <= cap,
			})
		}
		if any {
			lastFetch[key] = fetched
		}
	}
	return out
}

func hostOf(rawURL string) string {
	i := strings.Index(rawURL, "://")
	if i < 0 {
		return ""
	}
	rest := rawURL[i+3:]
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		return rest[:j]
	}
	return rest
}

// WriteEdgeAudit prints the answer to "could Cloudflare have caused this?".
func WriteEdgeAudit(w io.Writer, obs []ReuseObservation) {
	fmt.Fprintln(w, sep)
	fmt.Fprintln(w, "  COULD A CDN HAVE CAUSED THESE RESULTS?")
	fmt.Fprintln(w, sep)
	fmt.Fprintln(w)
	if len(obs) == 0 {
		fmt.Fprintln(w, "  No reuse observations to audit.")
		fmt.Fprintln(w)
		return
	}

	fmt.Fprintln(w, "  Our page is published through a Cloudflare tunnel, and a CDN is a cache")
	fmt.Fprintln(w, "  too. When nothing reaches our server, an edge that answered from its own")
	fmt.Fprintln(w, "  copy looks identical to a provider reusing its own. So every reuse is")
	fmt.Fprintln(w, "  checked against the longest an edge could legitimately have held it —")
	fmt.Fprintln(w, "  which is the freshness instruction WE sent.")
	fmt.Fprintln(w)

	var solid []ReuseObservation
	for _, o := range obs {
		if !o.EdgeExplains {
			solid = append(solid, o)
		}
	}
	sort.SliceStable(solid, func(i, j int) bool { return solid[i].GapMin > solid[j].GapMin })

	fmt.Fprintf(w, "  Reuse observations in total                    : %d\n", len(obs))
	fmt.Fprintf(w, "  A CDN could account for                       : %d\n", len(obs)-len(solid))
	fmt.Fprintf(w, "  A CDN CANNOT account for                      : %d\n", len(solid))
	fmt.Fprintln(w)

	if len(solid) == 0 {
		fmt.Fprintln(w, "  ⚠ NOTHING here rules out the CDN. Every reuse fell inside the window")
		fmt.Fprintln(w, "  the edge was allowed to serve from, so these results cannot yet be")
		fmt.Fprintln(w, "  attributed to the providers. Serve the pages with no-store, or run past")
		fmt.Fprintln(w, "  the max-age you send, and re-check.")
		fmt.Fprintln(w)
		return
	}

	fmt.Fprintln(w, "  The observations that carry the findings:")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "    %-9s %-14s %-22s %s\n", "reused at", "provider", "we served", "an edge could hold")
	for i, o := range solid {
		if i == 12 {
			fmt.Fprintf(w, "    … and %d more\n", len(solid)-12)
			break
		}
		fmt.Fprintf(w, "    %7.1fm %-14s %-22s %.0f min\n", o.GapMin, o.Provider, o.Served, o.EdgeCap)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Each line above was reused for longer than the edge was permitted to")
	fmt.Fprintln(w, "  serve it, so the copy was the provider's own.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  NOTE: CF-Cache-Status in the log cannot answer this question. When the")
	fmt.Fprintln(w, "  edge serves from its cache the request never reaches us, so no log line")
	fmt.Fprintln(w, "  exists to carry the header. It has been empty on every request recorded.")
	fmt.Fprintln(w)
}
