#!/usr/bin/env python3
"""Staleness test on public pages.

Per page, at a start time inside an hour that page usually changes (busy_hours_utc):
  copy A    download from your download server, anchor checked on it (exactly once, or the page is skipped)
  warm-up   each service asked on the PLAIN address
  +20 min   each service asked twice AT THE SAME MOMENT: plain address, and address + random tag
            (the tag makes a new address, so the service has no copy of it and must read the page)
  copy B    download from your download server
Stale only if the plain answer has text found only in copy A AND the tagged answer from the same
moment does not. Question: the fixed wording in publicPageHomepage_select_anchor (12 words / 30 characters).

Steps:
  plan  <anchors.jsonl> <candidates.jsonl> <start UTC, e.g. 2026-10-29T09:00:00Z> <plan.jsonl> [--seed N --gap 240]
  run   <plan.jsonl> <answers.jsonl> [--vendors openai,anthropic,xai] [--wait-min 20]
        copies go to <answers.jsonl>.copies.jsonl; a restart skips pages already finished
  score <answers.jsonl>
Needs PROBE_CONTACT (your contact, sent in the User-Agent of every download), PROBE_SNAP_HOST and
PROBE_SNAP_DIR (your download server, see publicPageHomepage_select_anchor) and the provider keys in the env.
All times are UTC. Pure logic is unit-tested in publicPageHomepage_run_test.py.
"""
import sys, os, json, time, random, secrets, threading, collections, urllib.parse
from datetime import datetime, timezone, timedelta
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from publicPageHomepage_select_anchor import (question, tagged, live_gate, spaceless, remote_snap, ask_id, ask_one, _load, MODELS)

LATE_OK_S = 300          # a page whose start passed more than this at launch is recorded as missed

def _iso(t): return t.strftime("%Y-%m-%dT%H:%M:%SZ")
def _parse(s): return datetime.fromisoformat(s.replace("Z", "+00:00"))

def plain(url):
    """The address without any tag: the same normalisation tagged() applies, minus the tag."""
    p = urllib.parse.urlsplit(url)
    return urllib.parse.urlunsplit(p._replace(path=p.path or "/"))

def plan(pages, start, gap_s=240, seed=0, days=3):
    """One start time per page, inside one of its busy hours (UTC), at least gap_s/2 apart.
    Pages with the fewest busy hours are placed first; each start gets a random offset in its slot."""
    rng = random.Random(seed)
    order = sorted(pages, key=lambda p: (len(p["busy_hours_utc"]), rng.random()))
    taken, out = set(), []
    n_slots = days * 86400 // gap_s
    for p in order:
        busy = set(p["busy_hours_utc"])
        for k in range(n_slots):
            slot = start + timedelta(seconds=k * gap_s)
            if k in taken or slot.hour not in busy or \
               (slot + timedelta(seconds=gap_s // 2)).hour not in busy: continue
            taken.add(k)
            t = slot + timedelta(seconds=rng.uniform(0, gap_s / 2))
            out.append({"origin": p["origin"], "anchor": p["anchor"], "start": _iso(t)})
            break
    return sorted(out, key=lambda r: r["start"])

def verdict(plain_out, tagged_out):
    """(outcome, reason) of the plain answer at +20 min, given the tagged answer from the same moment."""
    if plain_out[0] != "stale": return plain_out
    if tagged_out[0] == "stale": return ("inconclusive", "tagged_also_old")
    if tagged_out[0] in ("fresh", "inconclusive"): return ("stale", None)
    return ("inconclusive", "no_tagged_check")

# ---- run ----
_lock = threading.Lock()

def _write(fo, rec):
    with _lock:
        fo.write(json.dumps(rec, ensure_ascii=False) + "\n"); fo.flush()

def _say(msg):
    with _lock: print(f"{datetime.now(timezone.utc).strftime('%H:%M:%S')} UTC  {msg}", flush=True)

def _sleep_until(t):
    while (d := (t - datetime.now(timezone.utc)).total_seconds()) > 0: time.sleep(min(d, 30))

def _ask_all(jobs):
    """jobs: [(vendor, role, url, q)] -> records, all sent at the same moment (one thread each)."""
    out = [None] * len(jobs)
    def go(i, v, role, url, q): out[i] = {"vendor": v, "model": MODELS[v], "role": role, "url": url, **ask_one(v, url, q)}
    ts = [threading.Thread(target=go, args=(i, *j)) for i, j in enumerate(jobs)]
    for t in ts: t.start()
    for t in ts: t.join()
    return out

def run_page(row, vendors, wait_min, fo, co):
    o, anchor = row["origin"], row["anchor"]
    _sleep_until(_parse(row["start"]))
    a = remote_snap(o); aid = ask_id(o, a.get("ts"))
    keep = lambda s, role: _write(co, {**{k: s.get(k) for k in ("origin", "ts", "lines", "status", "error", "skipped")},
                                       "role": role, "ask_id": aid})
    keep(a, "older")
    gate, expected = live_gate(a.get("lines") or [], anchor)
    if gate != "ok":
        _write(fo, {"origin": o, "anchor": anchor, "ask_id": aid, "skipped": gate, "planned_start": row["start"]})
        keep({"origin": o, "skipped": gate}, "newer")                      # marks the page finished
        _say(f"[skip] {o}: {gate}"); return
    page = a["lines"]; sp = spaceless(" ".join(page[page.index(anchor) + 1:])[:200])
    base = {"origin": o, "anchor": anchor, "ask_id": aid, "expected_live": expected, "planned_start": row["start"]}
    pu = plain(o)
    t_warm = datetime.now(timezone.utc)
    for r in _ask_all([(v, "warmup", pu, question(pu, anchor, sp)) for v in vendors]):
        _write(fo, {**base, **r}); _say(f"[{r['vendor']} warmup] {o} {r['error'] or 'ok'} {r['latency_s']}s")
    _sleep_until(t_warm + timedelta(minutes=wait_min))
    jobs = []
    for v in vendors:
        tu = tagged(o, secrets.token_hex(4))
        jobs += [(v, "plain", pu, question(pu, anchor, sp)), (v, "tagged", tu, question(tu, anchor, sp))]
    for r in _ask_all(jobs):
        _write(fo, {**base, **r}); _say(f"[{r['vendor']} {r['role']}] {o} {r['error'] or 'ok'} {r['latency_s']}s")
    keep(remote_snap(o), "newer")
    _say(f"[done] {o}")

def cmd_plan(anchors_p, cand_p, start, out_p, seed, gap):
    anchors = {r["origin"]: r["anchor"] for r in _load(anchors_p) if r.get("anchor")}
    pages = [{"origin": c["origin"], "anchor": anchors[c["origin"]], "busy_hours_utc": c["busy_hours_utc"]}
             for c in _load(cand_p) if c["origin"] in anchors]
    p = plan(pages, _parse(start), gap_s=gap, seed=seed)
    with open(out_p, "x") as fo:
        for r in p: fo.write(json.dumps({**r, "seed": seed, "gap_s": gap}, ensure_ascii=False) + "\n")
    print(f"{len(p)} of {len(pages)} pages placed; first {p[0]['start']}, last {p[-1]['start']}")

def cmd_run(plan_p, out_p, vendors, wait_min):
    if not os.environ.get("PROBE_CONTACT"): sys.exit("PROBE_CONTACT is not set")
    rows = _load(plan_p)
    done = {c["origin"] for c in _load(out_p + ".copies.jsonl") if c.get("role") == "newer"}
    now = datetime.now(timezone.utc)
    fo, co = open(out_p, "a"), open(out_p + ".copies.jsonl", "a")
    threads = []
    for row in rows:
        if row["origin"] in done: continue
        if (now - _parse(row["start"])).total_seconds() > LATE_OK_S:
            _write(fo, {"origin": row["origin"], "anchor": row["anchor"], "skipped": "missed start", "planned_start": row["start"]})
            continue
        t = threading.Thread(target=run_page, args=(row, vendors, wait_min, fo, co)); t.start(); threads.append(t)
    _say(f"{len(threads)} pages waiting for their start times; {len(done)} already finished")
    for t in threads: t.join()
    _say("ALL-DONE")

def cmd_score(out_p):
    from publicPageHomepage_score_answerKinds import classify
    copies = collections.defaultdict(dict)
    for c in _load(out_p + ".copies.jsonl"): copies[c["ask_id"]][c["role"]] = c.get("lines")
    by = collections.defaultdict(dict)
    for r in _load(out_p):
        if r.get("skipped"): print(json.dumps({"origin": r["origin"], "outcome": "skipped", "reason": r["skipped"]})); continue
        cp = copies.get(r["ask_id"], {})
        by[(r["ask_id"], r["vendor"])][r["role"]] = classify(r.get("answer"), r.get("error"), r.get("fetch_status"),
                                                             r.get("stop_reason"), cp.get("older"), cp.get("newer"),
                                                             r["anchor"], r["url"])
    counts = collections.Counter()
    for (aid, v), roles in sorted(by.items()):
        if "plain" not in roles: continue
        out = verdict(roles["plain"][:2], roles.get("tagged", ("failed", "missing"))[:2])
        counts[(v, out)] += 1
        print(json.dumps({"ask_id": aid, "vendor": v, "outcome": out[0], "reason": out[1],
                          **{f"{k}_answer": list(roles[k][:2]) for k in roles}}, ensure_ascii=False))
    print("\nsummary:", file=sys.stderr)
    for (v, (o, w)), n in sorted(counts.items()): print(f"  {v:10s} {o:13s} {str(w):18s} {n}", file=sys.stderr)

if __name__ == "__main__":
    a = sys.argv[1:]
    if not a: sys.exit(__doc__)
    opt = lambda k, d: a[a.index(k) + 1] if k in a else d
    if a[0] == "plan": cmd_plan(a[1], a[2], a[3], a[4], int(opt("--seed", 0)), int(opt("--gap", 240)))
    elif a[0] == "run": cmd_run(a[1], a[2], opt("--vendors", "openai,anthropic,xai").split(","), float(opt("--wait-min", 20)))
    elif a[0] == "score": cmd_score(a[1])
    else: sys.exit(__doc__)
