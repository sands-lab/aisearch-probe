package probe

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// BotReport is the verdict on one bot.
//
// The three counters FirstSight, ReturnUnstamped and Judgeable partition every
// request for this bot. Only Judgeable requests say anything about the bot's
// caching behaviour:
//
//	FirstSight      — never seen this page before, so it had no copy to
//	                  revalidate. Downloading everything was its only option.
//	ReturnUnstamped — came back, but our previous response carried no ETag and
//	                  no Last-Modified, so it had nothing to ask about. Our bug.
//	Judgeable       — came back to a page we had stamped. The fair test.
//
// Within Judgeable, Asked + Blind == Judgeable.
type BotReport struct {
	Bot string `json:"bot"`

	FirstSight      int `json:"first_sight"`
	ReturnUnstamped int `json:"return_unstamped"`
	Judgeable       int `json:"judgeable"`

	Asked int `json:"asked"`
	Blind int `json:"blind"`

	NotModified int   `json:"not_modified_304"`
	WastedBytes int64 `json:"wasted_bytes"`

	TotalRequests int `json:"total_requests"`
	UniquePages   int `json:"unique_pages"`
	RevisitedURLs int `json:"revisited_pages"`
	EdgeServed    int `json:"edge_served"`

	IntervalMinS  float64 `json:"interval_min_s"`
	IntervalMeanS float64 `json:"interval_mean_s"`
	IntervalMaxS  float64 `json:"interval_max_s"`
	HasIntervals  bool    `json:"has_intervals"`

	PerPage map[string]*PageStats `json:"per_page"`
}

// PageStats is the per-page breakdown, using the same partition.
type PageStats struct {
	Hits            int   `json:"hits"`
	FirstSight      int   `json:"first_sight"`
	ReturnUnstamped int   `json:"return_unstamped"`
	Asked           int   `json:"asked"`
	Blind           int   `json:"blind"`
	WastedBytes     int64 `json:"wasted_bytes"`
}

// AskedPct is the share of fair-test visits on which the bot asked "has this
// changed?" before downloading. Returns -1 when there is nothing to judge —
// callers must render that as "n/a" rather than as zero, since a bot with no
// judgeable visits has not been shown to misbehave.
func (b *BotReport) AskedPct() float64 {
	if b.Judgeable == 0 {
		return -1
	}
	return 100 * float64(b.Asked) / float64(b.Judgeable)
}

// Verdict is the one-line plain-English conclusion.
func (b *BotReport) Verdict() string {
	if b.Judgeable == 0 {
		if b.ReturnUnstamped > 0 {
			return "Can't tell — we never stamped its copies, so it had nothing to ask about. Fix the origin's ETag/Last-Modified headers."
		}
		return "Can't tell yet — it has never returned to a page it already had."
	}
	switch pct := b.AskedPct(); {
	case pct == 0:
		return "Never asks. Every return visit downloads the whole page again."
	case pct < 10:
		return "Almost never asks. Nearly every return visit re-downloads the whole page."
	case pct < 50:
		return "Sometimes asks, but more often re-downloads the whole page."
	case pct < 95:
		return "Usually asks before downloading."
	default:
		return "Asks almost every time. Well behaved."
	}
}

// LoadRecords reads a JSONL log, normalising and filtering as it goes.
// Malformed lines are counted rather than silently dropped, so a schema break
// shows up as a number instead of as missing data.
func LoadRecords(path, botFilter string, since time.Time) ([]Record, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	return ParseRecords(f, botFilter, since)
}

// ParseRecords is LoadRecords over an arbitrary reader. Returns the kept
// records and the number of unparseable lines skipped.
func ParseRecords(r io.Reader, botFilter string, since time.Time) ([]Record, int, error) {
	var (
		records []Record
		skipped int
	)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			skipped++
			continue
		}
		rec.Normalize()
		if botFilter != "" && !strings.Contains(strings.ToLower(rec.Bot), strings.ToLower(botFilter)) {
			continue
		}
		if !since.IsZero() && rec.Time().Before(since) {
			continue
		}
		records = append(records, rec)
	}
	return records, skipped, scanner.Err()
}

// Analyze partitions every request into the three buckets and totals up the
// wasted traffic. Results are sorted by bot name for deterministic output.
//
// State is carried forward per (bot, page) in chronological order: a visit is
// judgeable only if THIS bot has seen THIS page before and the response we gave
// it on that earlier visit carried a validator.
func Analyze(records []Record) []BotReport {
	ordered := make([]Record, len(records))
	copy(ordered, records)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].TSUnix < ordered[j].TSUnix
	})

	type pageState struct {
		seen    bool
		stamped bool
		lastTS  float64
	}

	reports := map[string]*BotReport{}
	state := map[string]map[string]*pageState{}
	intervals := map[string][]float64{}

	for _, rec := range ordered {
		rep, ok := reports[rec.Bot]
		if !ok {
			rep = &BotReport{Bot: rec.Bot, PerPage: map[string]*PageStats{}}
			reports[rec.Bot] = rep
			state[rec.Bot] = map[string]*pageState{}
		}
		key := rec.PageKey
		if key == "" {
			key = rec.URL
		}

		ps, ok := rep.PerPage[key]
		if !ok {
			ps = &PageStats{}
			rep.PerPage[key] = ps
		}
		st, ok := state[rec.Bot][key]
		if !ok {
			st = &pageState{}
			state[rec.Bot][key] = st
		}

		rep.TotalRequests++
		ps.Hits++
		if rec.EdgeServed() {
			rep.EdgeServed++
		}
		if rec.StatusCode == 304 {
			rep.NotModified++
		}

		switch {
		case !st.seen:
			rep.FirstSight++
			ps.FirstSight++
		case !st.stamped:
			rep.ReturnUnstamped++
			ps.ReturnUnstamped++
		default:
			rep.Judgeable++
			if rec.AskedIfChanged() {
				rep.Asked++
				ps.Asked++
			} else {
				rep.Blind++
				ps.Blind++
				// Only a full body actually wastes traffic.
				if rec.StatusCode == 200 {
					rep.WastedBytes += rec.BytesSent
					ps.WastedBytes += rec.BytesSent
				}
			}
		}

		if st.seen && st.lastTS > 0 && rec.TSUnix > 0 {
			intervals[rec.Bot] = append(intervals[rec.Bot], rec.TSUnix-st.lastTS)
		}
		st.seen = true
		st.lastTS = rec.TSUnix
		// A 304 carries no body but still refreshes the stamp; only update the
		// stamp from responses that actually said something about validators.
		if rec.Stamped() {
			st.stamped = true
		} else if rec.StatusCode == 200 {
			// We served a full page with no validator — they now hold an
			// unstampable copy.
			st.stamped = false
		}
	}

	out := make([]BotReport, 0, len(reports))
	for bot, rep := range reports {
		rep.UniquePages = len(rep.PerPage)
		revisited := 0
		for _, ps := range rep.PerPage {
			if ps.Hits > 1 {
				revisited++
			}
		}
		rep.RevisitedURLs = revisited

		if ivs := intervals[bot]; len(ivs) > 0 {
			minV, maxV, sum := ivs[0], ivs[0], 0.0
			for _, v := range ivs {
				if v < minV {
					minV = v
				}
				if v > maxV {
					maxV = v
				}
				sum += v
			}
			rep.HasIntervals = true
			rep.IntervalMinS = minV
			rep.IntervalMaxS = maxV
			rep.IntervalMeanS = sum / float64(len(ivs))
		}
		out = append(out, *rep)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bot < out[j].Bot })
	return out
}
