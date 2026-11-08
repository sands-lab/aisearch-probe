"""Numbers for doc/openai-stale-after-fetch-notes.md section 6 (run from the repo root:
`python3 scripts/heal_score_openaiStale.py`) (heal test, OpenAI follow-ups).
Rates: all 8 sittings. Reply-level measures: sittings with whole replies (s4, s5, b2, b3).
Pages left out of the heal figure (first answer before our failure reached the vendor) and
failed calls are skipped. Read-only."""
import json, re, statistics
from collections import Counter, defaultdict

NOT_A_TEST = "no request reached us during the first ask"
GROUPS = [("Origin A, old prompt", ["s1", "s2", "s3"]), ("Origin A, new prompt", ["s4", "s5"]),
          ("Origin B, new prompt", ["b1", "b2", "b3"])]
WHOLE = ["s4", "s5", "b2", "b3"]
ORIGIN_LOG = {"probe.attentionhub.org": "logs/probe.jsonl", "agentfetchlab.com": "logs/probe-vps.jsonl"}
TAG = re.compile(r"turn\d+\w*view\w*", re.I)


def follow_ups(s):
    st = json.load(open(f"logs/heal-{s}.state.json"))
    res = json.load(open(f"logs/heal-{s}.results.json"))
    for pg in st["pages"]:
        if pg["provider"] != "or:openai":
            continue
        a = pg["reask"]
        row = next(r for r in res if r["provider"] == "or:openai" and r["fail"] == pg["fail"]
                   and abs(r["waited_s"] - a["waited_s"]) < 0.01)
        if row["call_failed"] or any(n.startswith(NOT_A_TEST) for n in row.get("notes") or []):
            continue
        kind = ("fresh" if row["code"] else "stale") if row["fetch"] else "none"
        yield st, pg, a, row, kind


print("== Rates (all sittings): follow-ups during which our server sent OpenAI the fixed page")
for name, ss in GROUPS:
    c = Counter()
    by_fail = Counter()
    for s in ss:
        for st, pg, a, row, kind in follow_ups(s):
            c[kind] += 1
            if kind != "none":
                by_fail[(pg["fail"], kind)] += 1
    back = c["stale"] + c["fresh"]
    print(f"  {name} ({', '.join(ss)}): old answer {c['stale']} of {back}; no request {c['none']}; "
          f"by failure: " + ", ".join(f"{f} {by_fail[(f,'stale')]}/{by_fail[(f,'stale')]+by_fail[(f,'fresh')]}" for f in ["503", "404", "403"]))

# reply-level
raw = defaultdict(list)
for line in open("logs/raw-replies.jsonl"):
    r = json.loads(line)
    if "healpage/or" in r["url"] and "openai" in r["url"] and r.get("raw"):
        raw[r["url"]].append(r)
logs = {}
for host, path in ORIGIN_LOG.items():
    d = defaultdict(list)
    for line in open(path):
        if "/healpage/or" not in line or "openai" not in line:
            continue
        x = json.loads(line)
        if x["bot"].startswith("probe-"):
            continue
        d[x["page_key"]].append(x["ts_unix"])
    logs[host] = d

m = defaultdict(lambda: defaultdict(list))
for s in WHOLE:
    for st, pg, a, row, kind in follow_ups(s):
        host = st["host"]
        reps = [r for r in raw.get(pg["url"], []) if a["sent_unix"] <= r["ts_unix"] <= a["done_unix"] + 1]
        if not reps:
            m[kind]["no reply kept"].append(1)
            continue
        msg = [r["raw"]["choices"][0]["message"] for r in reps]
        text = " ".join(x.get("content") or "" for x in msg)
        anns = [an["url_citation"] for x in msg for an in (x.get("annotations") or []) if an.get("type") == "url_citation"]
        m[kind]["search calls"].append(sum((r["raw"].get("usage") or {}).get("server_tool_use_details", {}).get("web_search_requests", 0) for r in reps))
        m[kind]["input tokens"].append(sum((r["raw"].get("usage") or {}).get("prompt_tokens", 0) for r in reps))
        m[kind]["duration"].append(a["done_unix"] - a["sent_unix"])
        hits = [t for t in logs[host].get(pg["path"], []) if a["sent_unix"] <= t <= a["done_unix"]]
        m[kind]["requests"].append(len(hits))
        if hits:
            m[kind]["request at"].append(min(hits) - a["sent_unix"])
        m[kind]["cites this page"].append(int(any(pg["page_id"] in c["url"].replace("%3A", ":") for c in anns)))
        m[kind]["cites home page"].append(int(any(c["url"].rstrip("/").count("/") == 2 for c in anns)))
        m[kind]["cites nothing"].append(int(not anns))
        m[kind]["leaked tag"].append(int(bool(TAG.search(text))))
        m[kind]["reasoning summary"].append(int(any(x.get("reasoning") for x in msg)))
        m[kind]["names original code"].append(int(bool(re.search(r"(?<!heal)(?<![0-9a-z-])" + pg["fail"] + r"(?![0-9a-z])", a.get("answer") or "", re.I))))
        m[kind]["quotes 'Failed to fetch'"].append(int(bool(re.search(r"failed to fetch", a.get("answer") or "", re.I))))


def rng(v):
    if not v:
        return "-"
    return f"{min(v):,.0f}-{max(v):,.0f} (median {statistics.median(v):,.0f})" if max(v) > 50 else f"{min(v):.1f}-{max(v):.1f} (median {statistics.median(v):.1f})"


print("\n== Reply-level (s4, s5, b2, b3)")
for kind in ["stale", "none", "fresh"]:
    d = m[kind]
    n = len(d["duration"])
    print(f"-- {kind}: {n} follow-ups" + (f" (+{len(d['no reply kept'])} with no reply kept)" if d["no reply kept"] else ""))
    print("   search calls:", dict(sorted(Counter(d["search calls"]).items())))
    print("   input tokens:", rng(d["input tokens"]))
    print("   duration s:", rng(d["duration"]))
    print("   request at s:", rng(d["request at"]), " requests per query:", dict(Counter(d["requests"])))
    for k in ["cites this page", "cites home page", "cites nothing", "leaked tag", "reasoning summary", "names original code", "quotes 'Failed to fetch'"]:
        print(f"   {k}: {sum(d[k])} of {n}")
