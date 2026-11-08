package probe

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// WriteHeaderReport shows, per bot, every request header it sent and the values
// seen. What a crawler declares — and what it omits — is evidence in its own
// right: a client that sends no Cache-Control and no conditional header is
// telling you it has no intention of revalidating.
func WriteHeaderReport(w io.Writer, records []Record) {
	byBot := map[string][]Record{}
	for _, r := range records {
		if len(r.ReqHeaders) == 0 {
			continue
		}
		byBot[r.Bot] = append(byBot[r.Bot], r)
	}
	if len(byBot) == 0 {
		fmt.Fprintln(w, "No request headers recorded. Headers are captured by the Go proxy")
		fmt.Fprintln(w, "(-serve or -origin mode); nginx Mode A cannot produce them.")
		return
	}

	bots := make([]string, 0, len(byBot))
	for b := range byBot {
		bots = append(bots, b)
	}
	sort.Strings(bots)

	fmt.Fprintln(w, sep)
	fmt.Fprintln(w, "  WHAT EACH CLIENT SENT US")
	fmt.Fprintln(w, sep)

	for _, bot := range bots {
		recs := byBot[bot]
		fmt.Fprintf(w, "\n  %s  (%d requests)\n", bot, len(recs))

		// Union of header names, with the distinct values seen for each.
		vals := map[string]map[string]int{}
		for _, r := range recs {
			for k, v := range r.ReqHeaders {
				if vals[k] == nil {
					vals[k] = map[string]int{}
				}
				vals[k][v]++
			}
		}
		names := make([]string, 0, len(vals))
		for k := range vals {
			names = append(names, k)
		}
		sort.Strings(names)

		for _, k := range names {
			seen := vals[k]
			parts := make([]string, 0, len(seen))
			for v, n := range seen {
				if len(seen) == 1 && n == len(recs) {
					parts = append(parts, truncate(v, 90))
				} else {
					parts = append(parts, fmt.Sprintf("%s (%d/%d)", truncate(v, 70), n, len(recs)))
				}
			}
			sort.Strings(parts)
			fmt.Fprintf(w, "    %-22s %s\n", k+":", strings.Join(parts, " | "))
		}

		// Absences are as telling as presences.
		var missing []string
		for _, h := range []string{"If-None-Match", "If-Modified-Since", "Cache-Control", "Pragma"} {
			if _, ok := vals[h]; !ok {
				missing = append(missing, h)
			}
		}
		if len(missing) > 0 {
			fmt.Fprintf(w, "    NOT SENT:              %s\n", strings.Join(missing, ", "))
		}
	}
	fmt.Fprintln(w)
}
