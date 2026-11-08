package probe

// Reading the object-size sweep's standard result records into a size-vs-cache
// shape. The runner (trial/objsizerun.go) writes FACTS ONLY — one row per ask,
// the page id as the join token, the byte size in extra — and never a verdict.
// This is the separate, re-runnable step that draws the verdict, exactly as
// analyze.go draws the caching verdict from the raw request log. Change the rule
// here and re-run it over the same rows; the data does not move.
//
// The mechanic mirrors retention: an episode is a warm+reask pair on one sized
// page. The warm should have caused a fetch; the reask, a short gap later, either
// causes a second fetch (the copy was not reused) or does not (it was reused).
// The authoritative fetch count is the join of the ask's token against the raw
// request log within a window (Correlate), never the runner's snapshot.
//
// THREE honesty rules, the same ones analyze.go already lives by:
//
//   - Judge an episode ONLY when its warm actually fetched. If the fetcher never
//     fetched the page even once, "no second fetch" carries no information — it
//     goes in no-warm-fetch, out of the kept-rate. (The FirstSight discipline.)
//   - Kept-rate is -1 (rendered "n/a"), never 0, when nothing is judgeable, so
//     "no evidence" never reads as an accusation. (The AskedPct discipline.)
//   - Cache rungs and fetch-probe rungs stay apart. A failure on a page bigger
//     than the model's context is a token/truncation limit, and must never be
//     read as a cache-admission cliff. marker-missing is shown, not folded into
//     "held", because on a big page it flags exactly that truncation.
//
// See doc/object-size-sweep-design.md, especially § CAUTION: report the SHAPE and
// name candidate policies; do not declare one.

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// ObjsizeRung is the tally for one byte size across every episode at that size.
type ObjsizeRung struct {
	Bytes int
	Kind  string // "cache" or "fetch-probe"

	Held        int // reask caused no fetch: the warm's copy was reused
	Refetched   int // reask caused a fetch: the copy was not reused
	NoWarmFetch int // the warm never fetched, so caching cannot be tested here

	MarkerMissing int // reask answer lacked the marker (truncation flag on big pages)
}

// Judgeable is the number of episodes at this size that can be scored: those
// whose warm fetched. no-warm-fetch episodes are excluded.
func (r ObjsizeRung) Judgeable() int { return r.Held + r.Refetched }

// KeptPct is the share of judgeable episodes whose copy was reused, or -1 when
// none are judgeable. Callers render -1 as "n/a", never 0.
func (r ObjsizeRung) KeptPct() float64 {
	j := r.Judgeable()
	if j == 0 {
		return -1
	}
	return 100 * float64(r.Held) / float64(j)
}

// ObjsizeVendor is one caller's whole sweep: its size rungs, sorted ascending.
// A caller is a vendor AND its model, per the standing gotcha — two models cache
// independently, so they must never share a tally.
type ObjsizeVendor struct {
	Vendor string
	Rungs  []ObjsizeRung
}

// ObjsizeAnalysis is the sweep split per caller, callers sorted by label.
type ObjsizeAnalysis struct {
	Vendors []ObjsizeVendor
}

// objsizeCaller is the split key: vendor and model together, so two models at one
// vendor stay apart. Model may be empty (e.g. a web-app surface), and then the
// vendor alone is the label.
func objsizeCaller(vendor, model string) string {
	if strings.TrimSpace(model) == "" {
		return vendor
	}
	return vendor + " / " + model
}

// AnalyzeObjsize joins the objsize result rows against the raw request log and
// tallies each size, SPLIT PER CALLER. windowSeconds is how long after an ask an
// arriving request still counts as caused by it (the same one-sided window
// Correlate uses everywhere).
func AnalyzeObjsize(results []ResultRecord, records []Record, windowSeconds float64) ObjsizeAnalysis {
	// One Correlate pass over every objsize ask; index the result by event id.
	var trials []Trial
	for _, r := range results {
		if r.Experiment != "objsize" {
			continue
		}
		trials = append(trials, Trial{
			TrialID: r.EventID, Provider: r.Vendor, URL: r.URL, Token: r.Token,
			SentAt: r.SentUTC, SentUnix: r.SentUnix, FoundMarker: r.MarkerPresent, Error: r.Error,
		})
	}
	fetched := map[string]bool{}
	for _, c := range Correlate(trials, records, windowSeconds) {
		fetched[c.Trial.TrialID] = c.Fetched
	}

	// Group warm and reask of each episode, and record each episode's caller.
	warm := map[string]ResultRecord{}
	reask := map[string]ResultRecord{}
	for _, r := range results {
		if r.Experiment != "objsize" {
			continue
		}
		switch r.Step {
		case "warm":
			warm[r.EpisodeID] = r
		case "reask":
			reask[r.EpisodeID] = r
		}
	}

	// Per caller, per size. rungs[caller][size] is a running tally.
	rungs := map[string]map[int]*ObjsizeRung{}
	get := func(caller string, size int, kind string) *ObjsizeRung {
		bySize := rungs[caller]
		if bySize == nil {
			bySize = map[int]*ObjsizeRung{}
			rungs[caller] = bySize
		}
		r := bySize[size]
		if r == nil {
			r = &ObjsizeRung{Bytes: size, Kind: kind}
			bySize[size] = r
		}
		return r
	}

	for ep, w := range warm {
		re, ok := reask[ep]
		if !ok {
			continue // an episode with no reask has no outcome yet
		}
		caller := objsizeCaller(w.Vendor, w.Model)
		size := extraInt(w.Extra, "size_bytes")
		kind := extraStr(w.Extra, "kind")
		rung := get(caller, size, kind)

		if !fetched[w.EventID] {
			rung.NoWarmFetch++ // the fetcher never fetched even once: unjudgeable
			continue
		}
		if fetched[re.EventID] {
			rung.Refetched++
		} else {
			rung.Held++
		}
		if !re.MarkerPresent {
			rung.MarkerMissing++
		}
	}

	out := ObjsizeAnalysis{}
	for caller, bySize := range rungs {
		v := ObjsizeVendor{Vendor: caller}
		for _, r := range bySize {
			v.Rungs = append(v.Rungs, *r)
		}
		sort.Slice(v.Rungs, func(i, j int) bool { return v.Rungs[i].Bytes < v.Rungs[j].Bytes })
		out.Vendors = append(out.Vendors, v)
	}
	sort.Slice(out.Vendors, func(i, j int) bool { return out.Vendors[i].Vendor < out.Vendors[j].Vendor })
	return out
}

// WriteObjsizeReport prints kept-rate against size — the SHAPE, cache rungs and
// fetch-probe rungs apart, with candidate policies named but none declared.
func WriteObjsizeReport(w io.Writer, a ObjsizeAnalysis) {
	fmt.Fprintln(w, sep)
	fmt.Fprintln(w, "  OBJECT-SIZE SWEEP — does size change whether a page is cached?")
	fmt.Fprintln(w, sep)
	fmt.Fprintln(w, "  Shape only. The verdict is this reading, re-runnable over the same rows;")
	fmt.Fprintln(w, "  do NOT over-claim. A flat result rules out a refuse-to-store cutoff, not")
	fmt.Fprintln(w, "  size-and-age eviction (that needs the longer-gap pass). Caching has many")
	fmt.Fprintln(w, "  moving parts, so name a candidate shape, do not declare a policy.")
	fmt.Fprintln(w, "  One section PER CALLER — sizes are never blended across models.")
	fmt.Fprintln(w)

	writeRungs := func(rungs []ObjsizeRung, title, kind string) {
		var rows []ObjsizeRung
		for _, r := range rungs {
			if r.Kind == kind {
				rows = append(rows, r)
			}
		}
		if len(rows) == 0 {
			return
		}
		fmt.Fprintf(w, "    %s\n", title)
		fmt.Fprintf(w, "      %-8s %-9s %-11s %-8s %s\n", "size", "kept-rate", "held/judged", "no-warm", "marker-miss")
		for _, r := range rows {
			kept := "n/a"
			if p := r.KeptPct(); p >= 0 {
				kept = fmt.Sprintf("%.0f%%", p)
			}
			fmt.Fprintf(w, "      %-8s %-9s %d/%-9d %-8d %d\n",
				objsizeLabel(r.Bytes), kept, r.Held, r.Judgeable(), r.NoWarmFetch, r.MarkerMissing)
		}
		fmt.Fprintln(w)
	}

	for _, v := range a.Vendors {
		fmt.Fprintf(w, "  %s\n", v.Vendor)
		writeRungs(v.Rungs, "cache-admission rungs (real-page sizes; a size effect shows here):", "cache")
		writeRungs(v.Rungs, "fetch/token-limit probe (huge pages; a failure here is NOT a cache cliff):", "fetch-probe")
	}
}

// objsizeLabel is a short, readable name for a byte size.
func objsizeLabel(n int) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%dm", n/(1<<20))
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%dk", n/(1<<10))
	default:
		return fmt.Sprintf("%db", n)
	}
}

// extraInt / extraStr read a fact out of a record's extra map. JSON numbers
// decode as float64; an in-memory float64 or int is accepted too.
func extraInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}

func extraStr(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}
