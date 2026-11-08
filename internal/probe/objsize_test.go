package probe

import (
	"strconv"
	"strings"
	"testing"
)

// The analysis splits PER CALLER (vendor and model), never aggregated: two models
// cache independently, so one blended shape would hide the very thing measured.
// Within a caller, an episode is judged only when its warm fetched, kept-rate is
// held/(held+refetched), an unjudgeable size reports n/a not 0%, and a fetch-probe
// rung never mixes with the cache rungs. This is the honesty contract, pinned.
func TestAnalyzeObjsizeSplitsPerVendorAndBuckets(t *testing.T) {
	const win = 150.0

	mk := func(vendor, model, ep, step, id string, size int, kind string, sent float64, marker bool) ResultRecord {
		url := "https://h/sizepage/" + id + "/" + strconv.Itoa(size)
		return ResultRecord{
			RunID: "run1", EventID: ep + "-" + step, Experiment: "objsize",
			EpisodeID: ep, Step: step, Surface: "api", Vendor: vendor, Model: model,
			URL: url, Token: id, SentUTC: "t", SentUnix: sent, MarkerPresent: marker,
			Extra: map[string]any{"size_bytes": float64(size), "kind": kind},
		}
	}
	// Caller A (or:openai / m1): 1KiB held + 1KiB refetched, 4KiB no-warm-fetch,
	// 1MiB fetch-probe refetched with no marker.
	// Caller B (or:anthropic / m2): 1KiB held — SAME size as A, must tally apart.
	results := []ResultRecord{
		mk("or:openai", "m1", "E1", "warm", "P1", 1024, "cache", 1000, true),
		mk("or:openai", "m1", "E1", "reask", "P1", 1024, "cache", 2000, true),
		mk("or:openai", "m1", "E2", "warm", "P2", 1024, "cache", 1000, true),
		mk("or:openai", "m1", "E2", "reask", "P2", 1024, "cache", 2000, true),
		mk("or:openai", "m1", "E4", "warm", "P4", 4096, "cache", 1000, true),
		mk("or:openai", "m1", "E4", "reask", "P4", 4096, "cache", 2000, true),
		mk("or:openai", "m1", "E5", "warm", "P5", 1<<20, "fetch-probe", 1000, true),
		mk("or:openai", "m1", "E5", "reask", "P5", 1<<20, "fetch-probe", 2000, false),
		mk("or:anthropic", "m2", "E3", "warm", "P3", 1024, "cache", 1000, true),
		mk("or:anthropic", "m2", "E3", "reask", "P3", 1024, "cache", 2000, true),
	}

	hit := func(id string, size int, ts float64) Record {
		path := "/sizepage/" + id + "/" + strconv.Itoa(size)
		return Record{TSUnix: ts, URL: "https://h" + path, PageKey: path}
	}
	records := []Record{
		hit("P1", 1024, 1010),  // A E1 warm fetched (reask NOT)
		hit("P2", 1024, 1010),  // A E2 warm fetched
		hit("P2", 1024, 2010),  // A E2 reask fetched
		hit("P5", 1<<20, 1010), // A E5 warm fetched
		hit("P5", 1<<20, 2010), // A E5 reask fetched
		hit("P3", 1024, 1010),  // B E3 warm fetched (reask NOT)
		// P4 never fetched -> A E4 unjudgeable
	}

	a := AnalyzeObjsize(results, records, win)

	if len(a.Vendors) != 2 {
		t.Fatalf("got %d callers, want 2 (one per vendor/model)", len(a.Vendors))
	}
	// Sorted by caller label: anthropic before openai.
	if a.Vendors[0].Vendor > a.Vendors[1].Vendor {
		t.Errorf("callers not sorted: %q before %q", a.Vendors[0].Vendor, a.Vendors[1].Vendor)
	}

	find := func(match string) ObjsizeVendor {
		for _, v := range a.Vendors {
			if strings.Contains(v.Vendor, match) {
				return v
			}
		}
		t.Fatalf("no caller matching %q in %v", match, a.Vendors)
		return ObjsizeVendor{}
	}
	rung := func(v ObjsizeVendor, size int) ObjsizeRung {
		for _, r := range v.Rungs {
			if r.Bytes == size {
				return r
			}
		}
		t.Fatalf("caller %q has no rung for size %d", v.Vendor, size)
		return ObjsizeRung{}
	}

	// The caller label must carry BOTH vendor and model.
	A := find("or:openai")
	if !strings.Contains(A.Vendor, "m1") {
		t.Errorf("caller label %q must include the model", A.Vendor)
	}
	if r := rung(A, 1024); r.Held != 1 || r.Refetched != 1 || r.KeptPct() != 50 {
		t.Errorf("A 1KiB: held=%d refetched=%d kept=%v, want 1/1/50", r.Held, r.Refetched, r.KeptPct())
	}
	if r := rung(A, 4096); r.NoWarmFetch != 1 || r.KeptPct() != -1 {
		t.Errorf("A 4KiB: nowarm=%d kept=%v, want 1 and -1 (n/a)", r.NoWarmFetch, r.KeptPct())
	}
	if r := rung(A, 1<<20); r.Kind != "fetch-probe" || r.Refetched != 1 || r.MarkerMissing != 1 {
		t.Errorf("A 1MiB: kind=%q refetched=%d marker-missing=%d, want fetch-probe/1/1", r.Kind, r.Refetched, r.MarkerMissing)
	}

	// Caller B's 1KiB is a DIFFERENT tally from A's 1KiB — the whole point.
	B := find("or:anthropic")
	if r := rung(B, 1024); r.Held != 1 || r.Refetched != 0 || r.KeptPct() != 100 {
		t.Errorf("B 1KiB: held=%d refetched=%d kept=%v, want 1/0/100 (must not merge with A)", r.Held, r.Refetched, r.KeptPct())
	}
}

// The report shows each caller under its own heading, renders n/a for an
// unjudgeable size (never 0%), separates cache from the fetch-probe, and carries
// the do-not-over-claim caution.
func TestWriteObjsizeReportPerVendor(t *testing.T) {
	a := ObjsizeAnalysis{Vendors: []ObjsizeVendor{
		{Vendor: "or:anthropic / m2", Rungs: []ObjsizeRung{{Bytes: 1024, Kind: "cache", Held: 1}}},
		{Vendor: "or:openai / m1", Rungs: []ObjsizeRung{
			{Bytes: 4096, Kind: "cache", NoWarmFetch: 1},        // unjudgeable -> n/a
			{Bytes: 1 << 20, Kind: "fetch-probe", Refetched: 1}, // a probe rung
		}},
	}}
	var buf strings.Builder
	WriteObjsizeReport(&buf, a)
	out := buf.String()

	if !strings.Contains(out, "or:anthropic / m2") || !strings.Contains(out, "or:openai / m1") {
		t.Errorf("report must show each caller under its own heading:\n%s", out)
	}
	if !strings.Contains(out, "n/a") {
		t.Errorf("report must render n/a for an unjudgeable size:\n%s", out)
	}
	if !strings.Contains(out, "fetch/token-limit") || !strings.Contains(out, "cache-admission") {
		t.Errorf("report must separate cache-admission rungs from the fetch/token-limit probe:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "not") { // the do-not-over-claim caution
		t.Errorf("report must carry the do-not-over-claim caution:\n%s", out)
	}
}
