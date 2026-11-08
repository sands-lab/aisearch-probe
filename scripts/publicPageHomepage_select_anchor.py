#!/usr/bin/env python3
"""Capability check before the staleness test: does each AI service find a page's anchor line and
quote the words that follow it? Asks on the RANDOM-TAG address (forces a fresh fetch), so stale
copies cannot confuse the result. Question approved by the user 2026-10-28 (DECISIONS D14):
'Fetch this document: <address>. Quote exactly the first 12 words that follow the line "<anchor>"
in it. If you cannot read the document, say so plainly.'
Words, not lines: each service splits a page into lines differently (blank lines, image text, dates).
12 words is under OpenAI's ~25-word verbatim limit and light for Google's RECITATION filter.

The anchor is a stable short LABEL chosen from the watch history (it must also be in any old copy an
AI may hold), and it is confirmed on a live download seconds before asking: exactly once, or the page
is skipped that round and the reason recorded.

Steps:
  anchors  <candidates.jsonl> <watch.jsonl> <snapshots> <anchors.jsonl>   offline, from watch data
  snap     <sites.txt> <out.jsonl>        run on the US server: one download per site (HTML + text)
  ask      <anchors.jsonl> <sites.txt> <snap-before.jsonl> <answers.jsonl> [--vendors ...]
                                          here; live anchor gate first; 4 services, 60 s apart
  score    <answers.jsonl> <snapA.jsonl>[,<snapB.jsonl>]
Every call records all token fields, the provider's own fetch status and its stop reason.
Pure logic is unit-tested in anchor_check_test.py.
"""
import sys, os, re, json, time, gzip, hashlib, secrets, collections, urllib.parse
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from publicPageHomepage_select_changes import is_noise_line, real_change_steps
from publicPage_config import REFUSAL, POST_KW, ANTHROPIC_TOOL, MODELS, _norm

N_LINES, N_WORDS, NO_SPACE_CHARS, MIN_PRESENT = 3, 4, 10, 0.9
MAX_LABEL_WORDS, MAX_LABEL_CHARS = 8, 40
NOT_FOUND = re.compile(r"(does not|doesn't|did not|didn't) (contain|include|have|find)|"
                       r"(could not|couldn't|cannot|can't|unable to) (find|locate)|"
                       r"\b(no|not) (such )?line\b.{0,40}(match|found|present)|not (found|present) (in|on) the", re.I)

# ---- pure logic (tested) --------------------------------------------------
def is_label(line):
    """A section label, not a headline: at most 8 words and 40 characters (CJK has no spaces)."""
    s = line.strip()
    return len(s.split()) <= MAX_LABEL_WORDS and len(s) <= MAX_LABEL_CHARS

def pick_anchor(versions, change_first):
    """versions: list of line lists; change_first: [(version index, index of first changed line)].
    Candidate = the line just above each change; keep the most voted candidate that is a label,
    not noise, and appears exactly once in >= 90% of versions."""
    votes = collections.Counter()
    for vi, li in change_first:
        if li > 0 and vi < len(versions) and li - 1 < len(versions[vi]):
            votes[versions[vi][li - 1]] += 1
    for cand, n in votes.most_common():
        if is_noise_line(cand) or not is_label(cand): continue
        share = sum(1 for v in versions if v.count(cand) == 1) / len(versions)
        if share >= MIN_PRESENT:
            return {"anchor": cand, "present_share": round(share, 3), "votes": n}
    return None

ASK_WORDS = 12                 # what the question asks for
WINDOW_WORDS, MIN_RUN = 20, 5  # scoring: a run of >= 5 words, in order, within the first 20 after the anchor
WINDOW_CHARS, MIN_CHARS = 60, 10   # same idea by characters, for pages without spaces (Japanese, Chinese)

ASK_CHARS = 30                 # pages written without spaces (approved 2026-10-28)

def question(url, anchor, spaceless_page=False):
    what = f"first {ASK_CHARS} characters" if spaceless_page else f"first {ASK_WORDS} words"
    return (f'Fetch this document: {url}. Quote exactly the {what} that follow the line '
            f'"{anchor}" in it. If you cannot read the document, say so plainly.')

def _tokens(text):
    return [t.rstrip(".-'") for t in re.findall(r"\w[\w.\-']*", (text or "").casefold()) if t.rstrip(".-'")]

def spaceless(text):
    """True for text written without spaces between words (CJK, kana)."""
    letters = [c for c in text if c.isalpha()]
    cjk = sum(1 for c in letters if "぀" <= c <= "鿿" or "가" <= c <= "힯" or "＀" <= c <= "￯")
    return bool(letters) and cjk / len(letters) >= 0.5

def words_after(page, anchor, n=WINDOW_WORDS):
    """The first n words after the anchor, with the page's line breaks flattened away."""
    if anchor not in page: return None
    i = page.index(anchor)
    return _tokens(" ".join(page[i + 1:]))[:n]

def _chars_after(page, anchor, n=WINDOW_CHARS):
    i = page.index(anchor)
    return re.sub(r"\s+", "", "".join(page[i + 1:]))[:n]

def live_gate(page, anchor):
    """Seconds before asking: the anchor must appear exactly once on the live page.
    -> ("ok", what we expect the answer to start with) or (reason, None)."""
    n = page.count(anchor)
    if n == 0: return "anchor missing on the live page", None
    if n > 1: return "anchor repeated on the live page", None
    if spaceless(" ".join(page[page.index(anchor) + 1:])[:200]): return "ok", _chars_after(page, anchor, 24)
    return "ok", " ".join(words_after(page, anchor, ASK_WORDS))

def _run(a, b):
    """Longest run of items shared by sequences a and b, in order (tokens or characters)."""
    import difflib
    return difflib.SequenceMatcher(None, a, b, autojunk=False).find_longest_match(0, len(a), 0, len(b)).size

def tagged(url, token):
    p = urllib.parse.urlsplit(url)
    q = urllib.parse.parse_qsl(p.query, keep_blank_values=True) + [("x", token)]
    return urllib.parse.urlunsplit(p._replace(path=p.path or "/", query=urllib.parse.urlencode(q)))

def lines_after(page, anchor, n=N_LINES):
    if anchor not in page: return None
    i = page.index(anchor)
    return page[i + 1:i + 1 + n]

def score_follow(answer, error, pages, anchor, url):
    """followed: the answer shares a run of >= 5 words, in order, with the first 20 words after the
    anchor in one of our live copies (pages without spaces: a run of >= 10 characters within the first
    60 after the anchor). This tolerates image descriptions, dates or line breaks a service inserts.
    wrong place: such a run with the rest of the page instead. Then: anchor not found by the service,
    refused, failed, inconclusive (as in publicPageXmlFile_run)."""
    if error or not (answer or "").strip(): return "failed"
    # Nothing is removed from the page or the answer: plain matching. Public pages carry no
    # version-unique marker, so a match is evidence, not proof (see the method's stated limitation).
    wrong = False
    at = _tokens(answer); ac = re.sub(r"\s+", "", answer)
    for p in pages:
        if anchor not in p: continue
        i = p.index(anchor); rest = p[i + 1:]
        if spaceless(" ".join(rest)[:200]):
            win = _chars_after(p, anchor)
            if _run(ac, win) >= MIN_CHARS: return "followed"
            other = re.sub(r"\s+", "", "".join(p[:i]) + "".join(rest))[WINDOW_CHARS:]
            wrong = wrong or _run(ac, other) >= MIN_CHARS
        else:
            win = words_after(p, anchor)
            if _run(at, win) >= MIN_RUN: return "followed"
            other = _tokens(" ".join(p[:i])) + _tokens(" ".join(rest))[WINDOW_WORDS:]
            wrong = wrong or _run(at, other) >= MIN_RUN
    if wrong: return "wrong place"
    if NOT_FOUND.search(answer): return "anchor not found by the service"
    if REFUSAL.search(answer): return "refused"
    return "inconclusive"

# ---- I/O ------------------------------------------------------------------
def _load(p): return [json.loads(l) for l in open(p)] if os.path.exists(p) else []

def cmd_anchors(cand_p, watch_p, snapdir, out_p):
    cands = {r["origin"] for r in _load(cand_p)}
    visits = collections.defaultdict(list)
    for r in (json.loads(l) for l in open(watch_p)):
        if r["origin"] in cands and r.get("status") == 200 and r.get("text_sha"): visits[r["origin"]].append(r)
    found = 0
    with open(out_p, "x") as fo:
        for o, vs in visits.items():
            vs.sort(key=lambda r: r["cycle"]); d = hashlib.sha256(o.encode()).hexdigest()[:16]
            cache = {}
            def L(sha):
                if sha not in cache:
                    p = os.path.join(snapdir, d, sha + ".txt.gz")
                    cache[sha] = gzip.open(p, "rt").read().split("\n") if os.path.exists(p) else []
                return cache[sha]
            versions = [L(r["text_sha"]) for r in vs]; cyc = [r["cycle"] for r in vs]
            steps = real_change_steps(versions, cyc)
            pos = {c: i for i, c in enumerate(cyc)}
            a = pick_anchor(versions, [(pos[s["at"]], s["first_index"]) for s in steps])
            found += a is not None
            fo.write(json.dumps({"origin": o, **(a or {"anchor": None})}, ensure_ascii=False) + "\n")
    print(f"{len(visits)} candidate sites, {found} with a usable anchor")

def snap_one(o, ua):
    """One download of a site's homepage (robots.txt obeyed): visible lines, headers, gzipped HTML."""
    from publicPageHomepage_select_sites import _get, _decode, we_may_fetch_homepage
    from publicPageHomepage_poll import visible_lines, headers_record
    rec = {"origin": o, "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
    try:
        st, _, h, _, b = _get(o.rstrip("/") + "/robots.txt", ua)
        ok, why = we_may_fetch_homepage(st, _decode(b, h) if st == 200 else "")
        if not ok: rec["skipped"] = why
        else:
            st, final, h, raw, body = _get(o, ua); html = _decode(body, h)
            rec.update(status=st, lines=visible_lines(html), headers=headers_record(h.items()),
                       html_gz_b64=__import__("base64").b64encode(gzip.compress(body)).decode())
    except Exception as e:
        rec["error"] = type(e).__name__
    return rec

def cmd_snap(sites_p, out_p):
    from publicPageHomepage_select_sites import name_tag
    ua = name_tag(os.environ)
    with open(out_p, "x") as fo:
        for o in (l.strip() for l in open(sites_p) if l.strip()):
            fo.write(json.dumps(snap_one(o, ua), ensure_ascii=False) + "\n"); fo.flush()
    print("snapped", sum(1 for _ in open(out_p)))

def cmd_snap1(o):
    """Run on the US server: download one page now and print it as one JSON line."""
    from publicPageHomepage_select_sites import name_tag
    print(json.dumps(snap_one(o, name_tag(os.environ)), ensure_ascii=False))

def remote_snap_cmd(origin, contact, host="probe-vps"):
    import shlex
    return ["ssh", "-o", "BatchMode=yes", host,
            f"cd ~/public-files && PROBE_CONTACT={shlex.quote(contact)} python3 publicPageHomepage_select_anchor.py snap1 {shlex.quote(origin)}"]

def remote_snap(origin):
    """Download one page from the US server right now (per-page copies around each page's questions)."""
    import subprocess
    p = subprocess.run(remote_snap_cmd(origin, os.environ["PROBE_CONTACT"]), capture_output=True, text=True, timeout=180)
    lines = [l for l in p.stdout.splitlines() if l.startswith("{")]
    return json.loads(lines[-1]) if lines else {"origin": origin, "error": "remote snap failed: " + p.stderr[-200:]}

def ask_id(origin, ts): return f"{origin}@{ts}"

def ask_one(v, url, q):
    """One question to one service. Records the answer, every token field, the provider's own fetch
    status, stop reason, full raw replies, Anthropic's fetched text, and each failed try's message."""
    import publicPage_run as ap
    meta, attempts, failures = {}, [], []
    rec = {"question": q, "sent_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
           "answer": "", "usage": None, "fetch_status": None, "stop_reason": None, "error": ""}
    # Google's free tier allows 500 requests a day and every failed try counts: 2 tries, not 4
    t1 = time.time(); kw = dict(POST_KW, attempts=attempts, failures=failures, **({"retries": 2} if v == "google" else {}))
    try:
        if v == "anthropic": ans, u = ap.ask_anthropic(url, None, prompt=q, tool=ANTHROPIC_TOOL, post_kw=kw, meta=meta)
        elif v == "google": ans, u = ap.ask_gemini(url, None, prompt=q, post_kw=kw, meta=meta)
        else: ans, u = ap.ask_openrouter(MODELS[v], url, None, prompt=q, post_kw=kw, meta=meta)
        rec.update(answer=ans, usage=u, fetch_status=meta.get("fetch_status"), stop_reason=meta.get("stop_reason"),
                   raw=ap.pack_raw(meta.get("raw") or []),
                   fetched_retrieved_at=[t["retrieved_at"] for t in meta.get("fetched_texts") or []],
                   fetched_texts=ap.pack_raw(meta.get("fetched_texts") or []) if meta.get("fetched_texts") else None)
    except Exception as e:
        rec["error"] = (f"HTTP {e.code}" if hasattr(e, "code") else type(e).__name__)
        # the failure in its own words: the provider's last reply body, or the exception's text
        if failures and (failures[-1].get("body") or failures[-1].get("message")):
            detail = failures[-1].get("body") or failures[-1].get("message")
        else:
            try: detail = e.read().decode("utf-8", "replace") if hasattr(e, "read") else str(e)
            except Exception: detail = str(e)
        rec["error_detail"] = (detail or str(e))[:2000]
    rec.update(latency_s=round(time.time() - t1, 1), attempts=len(attempts), failed_tries=failures)
    return rec

def cmd_ask(anchors_p, sites_p, snap_p, out_p, vendors):
    anchors = {r["origin"]: r["anchor"] for r in _load(anchors_p) if r.get("anchor")}
    per_page = snap_p == "--live"          # download each page right before and after its own questions
    live = {} if per_page else {s["origin"]: s.get("lines") for s in _load(snap_p)}
    sites = [l.strip() for l in open(sites_p) if l.strip()]
    copies_fo = open(out_p + ".copies.jsonl", "a") if per_page else None
    def keep_copy(snap, role, aid):
        copies_fo.write(json.dumps({**{k: snap.get(k) for k in ("origin", "ts", "lines", "status", "error", "skipped")},
                                    "role": role, "ask_id": aid}, ensure_ascii=False) + "\n"); copies_fo.flush()
    with open(out_p, "a") as fo:
        for o in sites:
            aid = None
            if per_page:
                before = remote_snap(o); live[o] = before.get("lines"); aid = ask_id(o, before.get("ts"))
                keep_copy(before, "older", aid)
            gate, expected = live_gate(live.get(o) or [], anchors[o])
            if gate != "ok":   # do not spend calls on a page whose anchor is not on the live page
                fo.write(json.dumps({"origin": o, "anchor": anchors[o], "skipped": gate,
                                     "sent_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())},
                                    ensure_ascii=False) + "\n"); fo.flush()
                print(f"[skip] {o}: {gate}", flush=True); continue
            page = live.get(o); sp = spaceless(" ".join(page[page.index(anchors[o]) + 1:])[:200])
            for v in vendors:
                url = tagged(o, secrets.token_hex(4)); q = question(url, anchors[o], sp)
                rec = {"vendor": v, "model": MODELS[v], "origin": o, "ask_id": aid, "url": url, "anchor": anchors[o],
                       "expected_live": expected, **ask_one(v, url, q)}
                fo.write(json.dumps(rec, ensure_ascii=False) + "\n"); fo.flush()
                print(f"[{v}] {o} {rec['error'] or 'ok'} {rec['latency_s']}s", flush=True)
                time.sleep(60)
            if per_page: keep_copy(remote_snap(o), "newer", aid)

def cmd_score(ans_p, snap_ps):
    snaps = collections.defaultdict(list)
    for p in snap_ps:
        for s in _load(p):
            if s.get("lines"): snaps[s["origin"]].append(s["lines"])
    for a in _load(ans_p):
        if a.get("skipped"):
            print(f"{'(skipped)':10s} {a['origin'][:36]:36s} {a['skipped']}"); continue
        s = score_follow(a["answer"], a["error"], snaps.get(a["origin"], []), a["anchor"], a["url"])
        print(f"{a['vendor']:10s} {a['origin'][:36]:36s} {s:32s} | {(a['answer'] or a['error'])[:150]!r}")

if __name__ == "__main__":
    a = sys.argv[1:]
    if not a: sys.exit(__doc__)
    if a[0] == "anchors": cmd_anchors(*a[1:5])
    elif a[0] == "snap1": cmd_snap1(a[1])
    elif a[0] == "snap": cmd_snap(a[1], a[2])
    elif a[0] == "ask":
        vend = ["openai", "anthropic", "xai", "google"]
        if "--vendors" in a: vend = a[a.index("--vendors") + 1].split(",")
        cmd_ask(a[1], a[2], a[3], a[4], vend)
    elif a[0] == "score": cmd_score(a[1], a[2].split(","))
    else: sys.exit(__doc__)
