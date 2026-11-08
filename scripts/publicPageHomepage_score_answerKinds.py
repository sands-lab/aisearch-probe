#!/usr/bin/env python3
"""Post-run classifier for public-page answers (re-runnable on saved data).

Outcomes are FIXED before the run; reason codes are derived from provider evidence and refined after
the run (seed list below is not closed). One outcome per answer, first match wins:
  1 failed        system problem: HTTP error, timeout, empty reply, cut off at max tokens
  2 a valid read  a run of >= MIN_RUN words, in order, shared with the words after the anchor
                  (first EXT words) in our older or newer copy  ->
                    inconclusive  shared_text_only | matches_both_versions
                    stale         the matched words include words found only in the OLDER copy
                    fresh         ... only in the NEWER copy
                  flag position: near (run starts within the first NEAR words) | shifted (NEAR..EXT)
  3 refused       declined on purpose (copy block, stated quote limit, domain not allowed,
                  unsupported type, retrieval error reported by the provider, stated cannot-read)
  4 off_target    answered but not a usable read (anchor not found, asked back, matched far away)
  5 unclassified  tripwire: a missing reason code if more than a few percent land here
Deviation from the drafted scheme, on purpose: a valid read is checked BEFORE refusal wording,
because answers mix them (OpenAI, carview: "I can't provide the exact verbatim lines" + an excerpt).
Nothing is removed from pages or answers before matching. Pages without spaces (Japanese, Chinese)
use characters: runs of >= MIN_CHARS within the first EXT_CHARS after the anchor.
Pure logic is unit-tested in publicPageHomepage_score_answerKinds_test.py.
Usage: publicPageHomepage_score_answerKinds.py <answers.jsonl> <copies.jsonl> [--min-run 5 --near 20 --ext 60]
"""
import sys, os, re, json, difflib, subprocess, collections
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from publicPageHomepage_select_anchor import _tokens, spaceless, NOT_FOUND
from publicPage_config import REFUSAL

MIN_RUN, NEAR, EXT = 5, 20, 60
MIN_CHARS, NEAR_CHARS, EXT_CHARS = 10, 60, 180
QUOTE_LIMIT = re.compile(r"\b\d{1,3}\s+words?\s+verbatim|reproduce more than|verbatim .{0,40}(copyright|not able|can.t)|"
                         r"(can.t|cannot|unable to) (provide|reproduce|quote) .{0,40}(verbatim|exact)|copyrighted", re.I)
ASKED_BACK = re.compile(r"\b(do you (mean|want)|would you like|please (confirm|clarify)|shall i|should i)\b", re.I)

def _run(a, b):
    m = difflib.SequenceMatcher(None, a, b, autojunk=False).find_longest_match(0, len(a), 0, len(b))
    return m.size, m.b, b[m.b:m.b + m.size]

def _after(page, anchor, spaceless_page):
    i = page.index(anchor)
    if spaceless_page: return re.sub(r"\s+", "", "".join(page[i + 1:]))[:EXT_CHARS]
    return _tokens(" ".join(page[i + 1:]))[:EXT]

def _far(page, anchor, spaceless_page):
    i = page.index(anchor)
    if spaceless_page:
        return re.sub(r"\s+", "", "".join(page[:i])) + re.sub(r"\s+", "", "".join(page[i + 1:]))[EXT_CHARS:]
    return _tokens(" ".join(page[:i])) + _tokens(" ".join(page[i + 1:]))[EXT:]

def _failed(answer, error, stop_reason):
    if stop_reason == "RECITATION": return ("refused", "quote_blocked_api")
    if error:
        if re.search(r"HTTP 5\d\d", error): return ("failed", "http_5xx")
        if "timed out" in error or "Timeout" in error: return ("failed", "timeout")
        return ("failed", "provider_error")
    if stop_reason in ("MAX_TOKENS", "max_tokens", "length", "length/length"): return ("failed", "truncated_max_tokens")
    if not (answer or "").strip(): return ("failed", "empty_reply")
    return None

def _read(answer, older, newer, anchor, min_run, near):
    pages = [p for p in (older, newer) if p and anchor in p]
    if not pages: return None
    sp = spaceless(" ".join(pages[-1][pages[-1].index(anchor) + 1:])[:200])
    a = re.sub(r"\s+", "", answer) if sp else _tokens(answer)
    need, near_lim = (MIN_CHARS, NEAR_CHARS) if sp else (min_run, near)
    hits = {}
    for name, p in (("older", older), ("newer", newer)):
        if p and anchor in p:
            n, start, matched = _run(a, _after(p, anchor, sp))
            if n >= need: hits[name] = (start, matched)
    if not hits: return None
    pos = "near" if min(s for s, _ in hits.values()) < near_lim else "shifted"
    old_w = _after(older, anchor, sp) if older and anchor in older else ([] if not sp else "")
    new_w = _after(newer, anchor, sp) if newer and anchor in newer else ([] if not sp else "")
    if sp:   # characters: is the matched stretch present in the other copy?
        only_old = any(name == "older" and m not in new_w for name, (_, m) in hits.items())
        only_new = any(name == "newer" and m not in old_w for name, (_, m) in hits.items())
    else:
        matched = set(t for _, m in hits.values() for t in m)
        only_old = bool(matched & (set(old_w) - set(new_w)))
        only_new = bool(matched & (set(new_w) - set(old_w)))
    if only_old and only_new: return ("inconclusive", "matches_both_versions", {"position": pos})
    if only_old: return ("stale", None, {"position": pos})
    if only_new: return ("fresh", None, {"position": pos})
    return ("inconclusive", "shared_text_only", {"position": pos})

def _refused(answer, fetch_status):
    for f in fetch_status or []:
        code = f.get("error_code")
        if code == "url_not_allowed": return ("refused", "domain_not_allowed")
        if code == "unsupported_content_type": return ("refused", "unsupported_type")
        if code: return ("refused", "fetch_status_error")
        st = f.get("status")
        if st and st != "URL_RETRIEVAL_STATUS_SUCCESS": return ("refused", "fetch_status_error")
    # "I cannot read it ... I fetched the page but could not find the line" is a miss, not a refusal
    if NOT_FOUND.search(answer): return ("off_target", "anchor_not_found")
    if QUOTE_LIMIT.search(answer): return ("refused", "quote_limit_stated")
    if REFUSAL.search(answer): return ("refused", "cannot_read_stated")
    return None

def _off_target(answer, older, newer, anchor, min_run):
    if NOT_FOUND.search(answer): return ("off_target", "anchor_not_found")
    if ASKED_BACK.search(answer) or answer.strip().endswith("?"): return ("off_target", "asked_back")
    for p in (older, newer):
        if p and anchor in p:
            sp = spaceless(" ".join(p[p.index(anchor) + 1:])[:200])
            a = re.sub(r"\s+", "", answer) if sp else _tokens(answer)
            if _run(a, _far(p, anchor, sp))[0] >= (MIN_CHARS if sp else min_run): return ("off_target", "matched_far")
    return None

def classify(answer, error, fetch_status, stop_reason, older, newer, anchor, url, min_run=MIN_RUN, near=NEAR):
    """-> (outcome, reason, flags)"""
    answer = answer or ""
    r = _failed(answer, error, stop_reason)
    if r: return r + ({},)
    r = _read(answer, older, newer, anchor, min_run, near)
    if r: return r
    r = _refused(answer, fetch_status) or _off_target(answer, older, newer, anchor, min_run)
    if r: return r + ({},)
    return ("unclassified", None, {})

def _commit():
    try: return subprocess.run(["git", "rev-parse", "--short", "HEAD"], capture_output=True, text=True,
                               cwd=os.path.dirname(os.path.abspath(__file__))).stdout.strip()
    except Exception: return None

def main():
    a = sys.argv[1:]
    if len(a) < 2: sys.exit(__doc__)
    ans_p, copies_p = a[0], a[1]
    opt = lambda k, d: int(a[a.index(k) + 1]) if k in a else d
    min_run, near = opt("--min-run", MIN_RUN), opt("--near", NEAR)
    # copies.jsonl: {"origin", "ts", "lines", "role": "older"|"newer", "ask_id"?}; older/newer per origin
    copies = collections.defaultdict(dict)
    for c in (json.loads(l) for l in open(copies_p)):
        copies[c.get("ask_id") or c["origin"]][c.get("role", "newer")] = c.get("lines")
    ver = _commit(); counts = collections.Counter(); reasons = collections.Counter()
    for r in (json.loads(l) for l in open(ans_p)):
        if r.get("skipped"): continue
        cp = copies.get(r.get("ask_id") or r["origin"], {})
        out, why, flags = classify(r.get("answer"), r.get("error"), r.get("fetch_status"), r.get("stop_reason"),
                                   cp.get("older"), cp.get("newer"), r["anchor"], r["url"], min_run, near)
        counts[(r["vendor"], out)] += 1; reasons[(r["vendor"], out, why)] += 1
        print(json.dumps({"vendor": r["vendor"], "origin": r["origin"], "outcome": out, "reason": why,
                          "flags": flags, "classifier": ver, "min_run": min_run, "near": near}, ensure_ascii=False))
    print("\nsummary:", file=sys.stderr)
    for (v, o, w), n in sorted(reasons.items()): print(f"  {v:10s} {o:13s} {str(w):24s} {n}", file=sys.stderr)

if __name__ == "__main__": main()
