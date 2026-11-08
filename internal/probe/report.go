package probe

import (
	"fmt"
	"io"
	"sort"
)

const sep = "──────────────────────────────────────────────────────────────────────"

// humanBytes renders a byte count the way a reader will quote it.
func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/(1024*1024*1024))
	}
}

func pctOf(n, total int) string {
	if total == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(n)/float64(total))
}

// WriteReport renders the plain-English report. Every line a reader might quote
// is labelled with why it counts or why it doesn't.
func WriteReport(w io.Writer, reports []BotReport, verbose bool) {
	fmt.Fprintln(w, sep)
	fmt.Fprintln(w, "  DO AI CRAWLERS ASK BEFORE RE-DOWNLOADING?")
	fmt.Fprintln(w, sep)

	for _, b := range reports {
		if b.Bot == "probe-selfcheck" && !verbose {
			continue // our own checks, not a crawler
		}
		fmt.Fprintf(w, "\nBot: %s\n", b.Bot)

		fmt.Fprintf(w, "\n  Visits we can't fairly judge\n")
		fmt.Fprintf(w, "    First time they'd seen the page      : %3d    (they had no copy yet)\n", b.FirstSight)
		fmt.Fprintf(w, "    We never stamped their copy          : %3d    (our fault, not theirs)\n", b.ReturnUnstamped)

		fmt.Fprintf(w, "\n  Visits we CAN judge\n")
		fmt.Fprintf(w, "    Came back to a page we'd stamped     : %3d\n", b.Judgeable)
		fmt.Fprintf(w, "      ├─ Asked \"has this changed?\" first : %3d    (%s)\n", b.Asked, pctOf(b.Asked, b.Judgeable))
		fmt.Fprintf(w, "      └─ Re-downloaded the whole page    : %3d    (%s)\n", b.Blind, pctOf(b.Blind, b.Judgeable))

		fmt.Fprintf(w, "\n  Wasted traffic                         : %s\n", humanBytes(b.WastedBytes))
		fmt.Fprintf(w, "    (content we sent them that they already had)\n")

		if b.EdgeServed > 0 {
			fmt.Fprintf(w, "\n  ⚠ %d of %d requests were answered by Cloudflare's own cache.\n", b.EdgeServed, b.TotalRequests)
			fmt.Fprintf(w, "    Those describe the CDN's behaviour, not the bot's. Set a\n")
			fmt.Fprintf(w, "    Bypass-cache rule on the probe hostname and re-collect.\n")
		}

		if verbose {
			fmt.Fprintf(w, "\n  Pages seen: %d   (came back to %d of them)\n", b.UniquePages, b.RevisitedURLs)
			if b.HasIntervals {
				fmt.Fprintf(w, "  Gap between return visits: shortest %s, typical %s, longest %s\n",
					humanDuration(b.IntervalMinS), humanDuration(b.IntervalMeanS), humanDuration(b.IntervalMaxS))
			}
			pages := make([]string, 0, len(b.PerPage))
			for p := range b.PerPage {
				pages = append(pages, p)
			}
			sort.Strings(pages)
			fmt.Fprintf(w, "\n  Per page:\n")
			for _, pg := range pages {
				p := b.PerPage[pg]
				fmt.Fprintf(w, "    %-38s visits=%-4d first=%-3d asked=%-3d re-downloaded=%-3d wasted=%s\n",
					truncate(pg, 38), p.Hits, p.FirstSight, p.Asked, p.Blind, humanBytes(p.WastedBytes))
			}
		}

		if b.Bot == "unknown" {
			fmt.Fprintf(w, "\n  NOTE: \"unknown\" is every client whose user-agent matched no known\n")
			fmt.Fprintf(w, "  crawler AND carried none of the secret codes we planted. On a public\n")
			fmt.Fprintf(w, "  URL that includes port scanners and link previewers. Check the\n")
			fmt.Fprintf(w, "  user-agents before treating this as a finding.\n")
		}
		if IsProbeTagBucket(b.Bot) {
			fmt.Fprintf(w, "\n  NOTE: this crawler did not name itself — its user-agent claimed to be\n")
			fmt.Fprintf(w, "  an ordinary browser. We know who it is only because the request carried\n")
			fmt.Fprintf(w, "  a secret code we put in a web address we gave to this provider and no\n")
			fmt.Fprintf(w, "  one else. Identity here rests on that code, not on what it declared.\n")
		}
		fmt.Fprintf(w, "\n  → %s\n", b.Verdict())
	}

	fmt.Fprintf(w, "\n%s\n", sep)
	fmt.Fprintln(w, "  SUMMARY")
	fmt.Fprintln(w, sep)
	for _, b := range reports {
		if b.Bot == "probe-selfcheck" && !verbose {
			continue
		}
		asked := "n/a"
		if b.Judgeable > 0 {
			asked = pctOf(b.Asked, b.Judgeable)
		}
		fmt.Fprintf(w, "  %-24s judged %3d visits, asked first %-4s wasted %s\n",
			b.Bot+":", b.Judgeable, asked, humanBytes(b.WastedBytes))
	}
	fmt.Fprintln(w)
}

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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
