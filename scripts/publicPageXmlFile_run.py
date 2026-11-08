#!/usr/bin/env python3
"""Public XML file-type test (doc/public_page_validation_notes.md; write-up: file types are validated
on public sites for XML only). Asks four AI services to read real XML files published by the watched
sites — sitemaps and RSS/Atom feeds, by DECLARED type (application/xml, text/xml, rss, atom) — plus
llms.txt files served as text/plain as a contrast, and scores whether each answer shows the file
was read.

Steps:
  select  <files-all.jsonl> <files-test.jsonl>      pick the test files (rules in select_files)
  keys    <files-test.jsonl> <keys.jsonl>           fresh answer keys; run on the US server (KAUST
                                                    blocks some sites); robots.txt checked per file
  ask     <files-test.jsonl> <answers.jsonl> [--vendors openai,anthropic,xai,google] [--limit N] [--no-pause]
                                                    the AI questions; run here. One lane per vendor,
                                                    1 call every 3 min, lanes offset by 30 s; Google
                                                    pauses 07:00-09:30 UTC (another session's Gemini
                                                    run); restart-safe; a file STOP next to answers
                                                    ends all lanes cleanly; a lane stops at a 429
                                                    that survives its retries
  score   <answers.jsonl> <keys1.jsonl> [<keys2.jsonl> ...]
Every call records its token use (input/output) — a standing project rule.
Calls reuse publicPage_run's providers with: 5-min timeout, at most 3 retries (30 s, 60 s, 120 s
apart) only on 429/5xx, never after a timeout (a timed-out Anthropic call can still be billed);
Anthropic uses the BASIC web_fetch_20250910 with max_content_tokens=20000.
Pure logic is unit-tested in file_test_test.py.
"""
import sys, os, re, json, time, random, threading, datetime as dt, urllib.error
from collections import Counter, defaultdict

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from publicPage_config import ANTHROPIC_TOOL, POST_KW, MODELS, REFUSAL, _norm

INTERVAL = 180
OFFSETS = {"openai": 0, "anthropic": 30, "xai": 60, "google": 90}
PAUSE_UTC = ((7, 0), (9, 30))
PAUSE_VENDORS = {"google"}
SEED = 20261024
STOP_WORDS = {"http", "https", "html", "version", "encoding", "urlset", "sitemap", "sitemapindex", "lastmod",
              "changefreq", "priority", "daily", "weekly", "hourly", "monthly", "always", "never", "yearly",
              "channel", "item", "title", "link", "description", "schemas", "sitemaps", "xmlns", "image",
              "news", "loc", "pubdate", "guid", "rss", "atom", "feed", "entry", "updated"}
# Lines nearly every sitemap/feed shares: quoting them proves nothing about reading THIS file.
BOILERPLATE = re.compile(r"^\s*(<\?xml\b.*|</?(urlset|sitemapindex|sitemap|url|rss|channel|feed|item|entry)\b[^>]*>)\s*$", re.I)

# xAI's fetch tool can hand a sitemap over as one long line; it then says it cannot quote 3 lines.
ONE_LINE = re.compile(r"\b(single|one|only 1|1)\s+(\(?very long\)?\s+|continuous\s+|long\s+)?(line|block)\b", re.I)

# ---- pure logic (tested) --------------------------------------------------
def _mime(r): return (r.get("content_type") or "").split(";")[0].strip().lower()

def select_files(recs):
    out, seen = [], set()
    for r in recs:
        if r["url"] in seen or "mencretmesum" in r["site"]: continue
        ok = (r.get("status") == 200 and not r.get("over_cap") and r.get("robots_ai")
              and r.get("answer_key") and r.get("bytes", 0) >= 1024 and len(r["answer_key"]) >= 5)
        if not ok: continue
        if r.get("format") == "xml": group = _mime(r)
        elif r.get("found_via") == "/llms.txt" and _mime(r) == "text/plain": group = "llms.txt as text/plain"
        else: continue
        seen.add(r["url"])
        out.append({"site": r["site"], "url": r["url"], "group": group, "bytes": r["bytes"], "found_via": r["found_via"]})
    return out

def question(url):
    return (f"Fetch this document: {url}. Quote its first 3 lines exactly. "
            "If you cannot read the document, say so plainly.")

def slot_times(t0, n, interval=INTERVAL, offset=0):
    return [t0 + offset + i * interval for i in range(n)]

def in_pause(ts):
    t = dt.datetime.fromtimestamp(ts, dt.timezone.utc)
    (h0, m0), (h1, m1) = PAUSE_UTC
    return (h0, m0) <= (t.hour, t.minute) < (h1, m1)

def words(text):
    return {w for w in re.findall(r"\w{4,}", (text or "").lower()) if w not in STOP_WORDS and not w.isdigit()}

def score(answer, error, keys, url):
    """keys: one or more answer-key copies (lists of lines). Outcomes, first match wins:
    failed           error or empty answer
    read             >=5 distinctive key words in the answer (words of the address we gave excluded),
                     or an exact copy of one of the key's first 3 lines that is not boilerplate,
                     not the address itself, and at least 10 characters long
    boilerplate only the answer copies key lines, but only standard XML/feed lines every file shares
    refused          says it cannot read / access / fetch the document
    inconclusive     neither shows the file's content nor says it could not be read"""
    if error or not (answer or "").strip(): return "failed"
    aw = words(answer) - words(url); na = _norm(answer)
    for k in keys:
        if len(aw & (words("\n".join(k)) - words(url))) >= 5: return "read"
        for line in k[:3]:
            n = _norm(line)
            if len(n) >= 10 and not BOILERPLATE.match(line) and n not in (_norm(url),) and n in na: return "read"
    if any(_norm(l) in na for k in keys for l in k[:3] if BOILERPLATE.match(l)): return "boilerplate only"
    if ONE_LINE.search(answer): return "fetched, one line"
    if REFUSAL.search(answer): return "refused"
    return "inconclusive"

def new_record(vendor, f):
    """One answer record; fetch_status (the provider's own fetch report) and usage are ALWAYS present."""
    return {"vendor": vendor, "model": MODELS[vendor], "url": f["url"], "group": f["group"], "site": f["site"],
            "sent_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "question": question(f["url"]),
            "answer": "", "usage": None, "fetch_status": None, "stop_reason": None, "error": ""}

# ---- I/O ------------------------------------------------------------------
def _load(p): return [json.loads(l) for l in open(p)] if os.path.exists(p) else []

def cmd_select(src, dst):
    sel = select_files(_load(src))
    with open(dst, "x") as f: f.writelines(json.dumps(r) + "\n" for r in sel)
    print(len(sel), "files:", dict(Counter(r["group"] for r in sel)))

def cmd_keys(src, dst):
    from concurrent.futures import ThreadPoolExecutor
    from publicPageHomepage_select_sites import name_tag
    from publicPageXmlFile_select import check_file, Robots
    ua = name_tag(os.environ); robots = Robots(ua); files = _load(src)
    def one(r):
        rec, text = check_file(r["site"], r["url"], "key refresh", ua, robots)
        return {"url": r["url"], "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "status": rec.get("status"),
                "content_type": rec.get("content_type"), "bytes": rec.get("bytes"), "skipped": rec.get("skipped"),
                "error": rec.get("error"), "key": [l.strip() for l in text.splitlines() if l.strip()][:40]}
    with open(dst, "x") as f, ThreadPoolExecutor(max_workers=4) as ex:
        for k in ex.map(one, files): f.write(json.dumps(k) + "\n")
    ks = _load(dst); print(len(ks), "keys; with content:", sum(1 for k in ks if k["key"]))

def cmd_ask(src, dst, vendors, limit=None, pause=True, interval=INTERVAL, start_in=5):
    """pause=False (--no-pause) skips the old Google pause (added for another session's Gemini run,
    which ended 29 Oct). interval/start_in exist for the tests; runs use the defaults.
    Each call goes through publicPageHomepage_select_anchor.ask_one (from 2026-10-30), so the record also keeps the raw
    reply, every failed try's error message (failed_tries) and Google's 2-try limit."""
    import publicPageHomepage_select_anchor
    files = _load(src)
    done = {(r["vendor"], r["url"]) for r in _load(dst)}
    stop = os.path.join(os.path.dirname(os.path.abspath(dst)), "STOP")
    lock = threading.Lock(); fo = open(dst, "a"); t0 = time.time() + start_in
    def lane(v):
        order = list(files); random.Random(f"{SEED}-{v}").shuffle(order)
        order = [r for r in order if (v, r["url"]) not in done][:limit]
        slots = slot_times(t0, len(order), interval, OFFSETS[v]); i = 0; shift = 0.0
        while i < len(order):
            target = slots[i] + shift
            while time.time() < target:
                if os.path.exists(stop): return print(f"[{v}] STOP file; lane ends", flush=True)
                time.sleep(min(5, max(0, target - time.time())))
            if pause and v in PAUSE_VENDORS and in_pause(time.time()):
                time.sleep(60); shift += 60; continue
            r = order[i]
            rec = {**new_record(v, r), **publicPageHomepage_select_anchor.ask_one(v, r["url"], question(r["url"]))}
            stop_lane = rec["error"] == "HTTP 429"
            with lock: fo.write(json.dumps(rec) + "\n"); fo.flush()
            print(f"[{v}] {i+1}/{len(order)} {r['group']:22s} {rec['error'] or 'ok':12s} "
                  f"in={((rec.get('usage') or {}).get('input_tokens'))} {rec['latency_s']}s", flush=True)
            if stop_lane: return print(f"[{v}] 429 after retries — lane stops for the night", flush=True)
            i += 1
        print(f"[{v}] lane done", flush=True)
    ths = [threading.Thread(target=lane, args=(v,)) for v in vendors]
    for t in ths: t.start()
    for t in ths: t.join()
    fo.close(); print("ALL LANES DONE", flush=True)

def cmd_score(ans_path, key_paths):
    keys = defaultdict(list)
    for p in key_paths:
        for k in _load(p):
            if k.get("key"): keys[k["url"]].append(k["key"])
    res = defaultdict(Counter); tok = defaultdict(Counter)
    for a in _load(ans_path):
        s = score(a.get("answer"), a.get("error"), keys.get(a["url"], []), a["url"]) if keys.get(a["url"]) else "no key"
        res[(a["vendor"], a["group"])][s] += 1
        tok[a["vendor"]].update({k: v for k, v in (a.get("usage") or {}).items() if isinstance(v, int)}); tok[a["vendor"]]["calls"] += 1
    for (v, g), c in sorted(res.items()): print(f"  {v:10s} {g:24s} {dict(c)}")
    for v, c in sorted(tok.items()): print(f"  tokens {v:10s} {dict(c)}")

if __name__ == "__main__":
    a = sys.argv[1:]
    if not a: sys.exit(__doc__)
    if a[0] == "select": cmd_select(a[1], a[2])
    elif a[0] == "keys": cmd_keys(a[1], a[2])
    elif a[0] == "ask":
        vend = ["openai", "anthropic", "xai", "google"]; lim = None
        if "--vendors" in a: vend = a[a.index("--vendors") + 1].split(",")
        if "--limit" in a: lim = int(a[a.index("--limit") + 1])
        cmd_ask(a[1], a[2], vend, lim, pause="--no-pause" not in a)
    elif a[0] == "score": cmd_score(a[1], a[2:])
    else: sys.exit(__doc__)
