#!/usr/bin/env python3
"""Find the watched pages whose real content changes within the AI test's 20-minute wait
(method: docs/experiments/publicPage.md). Offline: reads the watch's visit log and saved
page text (scripts/publicPageHomepage_poll.py output); no requests to any site.

A REAL change = a line that appears between two neighbouring visits (10 min apart) and is still
there on the next visit (it lasts >= 20 min). Noise, ignored: lines that last one visit only
(ads, rotating boxes), lines under 10 characters, and clock/date-like lines. Text only: the watch
kept visible text, not HTML, so main-content tools (trafilatura) cannot be used on this data.

Per site: share of 10-minute steps with a real change (ranking), median gap between real changes,
where the first changed line sits (median line number, p10-p90 spread), example lines, labels.
Keep: HTML, answered on >= 100 visits, median gap <= 20 min. Counted with and without the
position limits (first 100 lines, spread <= 5) so their cost is visible before deciding.
Pure logic is unit-tested in publicPageHomepage_select_changes_test.py.
Usage: publicPageHomepage_select_changes.py <watch.jsonl> <snapshots-dir> <out.jsonl> [labels: search.jsonl,new-results.jsonl]
"""
import sys, os, re, json, gzip, hashlib, collections

MIN_VISITS, MAX_GAP_MIN, MAX_FIRST_LINE, MAX_SPREAD, CYCLE_MIN = 100, 25, 100, 5, 10   # gap: under 30 min (the shortest provider copy lifetime); medians step in 5s
_TIMEISH = re.compile(r"\b\d{1,2}:\d{2}\b|\b\d+\s*(sec|secs|seconds|min|mins|minutes|hr|hrs|hours)\s+ago\b"
                      r"|\b\d{4}-\d{2}-\d{2}\b|\b\d{1,2}[./]\d{1,2}[./]\d{2,4}\b", re.I)

# ---- pure logic (tested) --------------------------------------------------
MIN_LETTERS, MAX_DIGIT_SHARE = 12, 0.3

def is_noise_line(line):
    """Noise: too short, clock/date-like, or not enough substance — fewer than 12 letters (any
    script, so Japanese/Korean/Chinese count) or digits >= 30% of the visible characters. This
    drops counters, prices, bylines and relative times in any language ("224コメント3時間前")."""
    s = line.strip()
    if len(s) < 10: return True
    if _TIMEISH.search(s) and len(_TIMEISH.sub("", s).strip(" -|·:,")) < 25: return True
    letters = sum(1 for ch in s if ch.isalpha())
    digits = sum(1 for ch in s if ch.isdigit())
    visible = sum(1 for ch in s if not ch.isspace())
    if letters < MIN_LETTERS: return True
    if visible and digits / visible >= MAX_DIGIT_SHARE: return True
    return False

def busy_hours(steps):
    """steps = [(hour, had_real_change)] -> {hour: share of that hour's steps with a real change}."""
    by = {}
    for h, ch in steps: by.setdefault(h, []).append(ch)
    return {h: round(sum(v) / len(v), 2) for h, v in sorted(by.items())}

def eligible_steps(cycles):
    """Indexes i+1 where visits i, i+1, i+2 are consecutive cycles (a step that can be judged)."""
    return [i + 1 for i in range(len(cycles) - 2)
            if cycles[i + 1] - cycles[i] == 1 and cycles[i + 2] - cycles[i + 1] == 1]

def real_change_steps(versions, cycles):
    """versions[i] = list of lines at visit i (cycles[i]). A step i->i+1 (consecutive cycles) has a
    real change if it adds a non-noise line that is also present at visit i+2 (consecutive too)."""
    out = []
    for i in range(len(versions) - 2):
        if cycles[i + 1] - cycles[i] != 1 or cycles[i + 2] - cycles[i + 1] != 1: continue
        before, now, after = set(versions[i]), versions[i + 1], set(versions[i + 2])
        new = [l for l in now if l not in before and l in after and not is_noise_line(l)]
        if new:
            out.append({"at": cycles[i + 1], "lines": new[:3], "first_index": now.index(new[0])})
    return out

def gaps_minutes(at_cycles, cycle_min=CYCLE_MIN):
    return [(b - a) * cycle_min for a, b in zip(at_cycles, at_cycles[1:])]

def median(xs):
    if not xs: return None
    s = sorted(xs); n = len(s)
    return s[n // 2] if n % 2 else (s[n // 2 - 1] + s[n // 2]) / 2

def position_spread(idx):
    if not idx: return None
    s = sorted(idx); n = len(s)
    return s[int(0.9 * (n - 1))] - s[int(0.1 * (n - 1))]

# ---- I/O ------------------------------------------------------------------
def _site_dir(origin): return hashlib.sha256(origin.encode()).hexdigest()[:16]

def load_labels(paths):
    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    lab = {}
    for p in paths:
        for r in (json.loads(l) for l in open(p)):
            if "categories_section" in r:
                import publicPageHomepage_select_urlScanSubmit
                lab[r["host"]] = publicPageHomepage_select_urlScanSubmit.labels_with_fallback(r["host"], r.get("categories_section"))[1]
            elif r.get("found"):
                lab[r["host"]] = r.get("labels")
    return lab

def main():
    if len(sys.argv) < 4: sys.exit(__doc__)
    watch, snapdir, out = sys.argv[1], sys.argv[2], sys.argv[3]
    labels = load_labels(sys.argv[4].split(",")) if len(sys.argv) > 4 else {}
    visits = collections.defaultdict(list)
    for r in (json.loads(l) for l in open(watch)):
        if r.get("status") == 200 and r.get("text_sha"): visits[r["origin"]].append(r)
    cache = {}
    def lines(origin, sha):
        k = (origin, sha)
        if k not in cache:
            p = os.path.join(snapdir, _site_dir(origin), sha + ".txt.gz")
            cache[k] = gzip.open(p, "rt").read().split("\n") if os.path.exists(p) else []
        return cache[k]
    funnel = collections.Counter(); rows = []
    for origin, vs in visits.items():
        vs.sort(key=lambda r: r["cycle"]); funnel["sites with answers"] += 1
        ctype = ((vs[-1].get("headers") or {}).get("content-type") or "").split(";")[0].strip().lower()
        if ctype != "text/html": continue
        funnel["HTML"] += 1
        if len(vs) < MIN_VISITS: continue
        funnel[f"answered >= {MIN_VISITS} visits"] += 1
        cyc = [r["cycle"] for r in vs]
        steps = real_change_steps([lines(origin, r["text_sha"]) for r in vs], cyc)
        at = [s["at"] for s in steps]; gm = median(gaps_minutes(at)); idx = [s["first_index"] for s in steps]
        host = origin.split("://", 1)[1]
        changed_at = set(at); elig = eligible_steps(cyc)
        # hour of each judged step, in UTC, for scheduling questions at busy hours
        hours = busy_hours([(int(vs[i]["ts"][11:13]), cyc[i] in changed_at) for i in elig])
        row = {"origin": origin, "visits": len(vs), "real_changes": len(steps),
               "share_of_steps_with_real_change": round(len(steps) / max(1, len(elig)), 3),
               "busy_hours_utc": [h for h, s in hours.items() if s >= 0.5], "hourly_share_utc": hours,
               "median_gap_min": gm, "first_line_median": median(idx), "first_line_spread": position_spread(idx),
               "examples": [l for s in steps[:3] for l in s["lines"][:1]], "labels": labels.get(host),
               "cache_control": (vs[-1].get("headers") or {}).get("cache-control")}
        row["keep"] = gm is not None and gm <= MAX_GAP_MIN
        row["keep_with_position"] = row["keep"] and row["first_line_median"] is not None \
            and row["first_line_median"] <= MAX_FIRST_LINE and (row["first_line_spread"] or 0) <= MAX_SPREAD
        if steps: funnel["has at least one real change"] += 1
        if row["keep"]: funnel[f"median gap <= {MAX_GAP_MIN} min"] += 1
        if row["keep_with_position"]: funnel[f"  ... and change in first {MAX_FIRST_LINE} lines, moving <= {MAX_SPREAD}"] += 1
        rows.append(row)
    rows.sort(key=lambda r: -r["share_of_steps_with_real_change"])
    with open(out, "x") as f: f.writelines(json.dumps(r, ensure_ascii=False) + "\n" for r in rows)
    print("step-by-step count:")
    for k, v in funnel.items(): print(f"  {k:52s} {v}")
    print("\ntop of the ranking (share of 10-min steps with a real change):")
    for r in [x for x in rows if x["keep"]][:15]:
        print(f"  {r['share_of_steps_with_real_change']:.2f}  gap {r['median_gap_min']:>5} min  line {r['first_line_median']}"
              f"±{r['first_line_spread']}  {r['origin'][:38]:38s} {(r['examples'] or [''])[0][:60]}")

if __name__ == "__main__": main()
