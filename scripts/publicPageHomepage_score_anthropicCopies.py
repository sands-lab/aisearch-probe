#!/usr/bin/env python3
"""Staleness from the copy Anthropic actually fetched, not from how it worded the answer
(public-page staleness run, 2 Nov 2026). Anthropic is the only service whose reply carries the page
text it used and the time it says it fetched it (web_fetch_tool_result: text + retrieved_at).

RULES (written before running; no cut-off is set, so nothing is tuned)
  age   = our question's sent_at minus Anthropic's retrieved_at for the copy it used (each call used at
          most one fetch). Groups: fresh (under 2 min), 2 min to 1 h, 1 to 24 h, 1 to 7 days, over
          7 days, no copy.
  reuse = for the normal (plain-address) question 20 min after the warm-up: same retrieved_at as the
          warm-up's copy -> "same copy as the warm-up"; later -> newer copy; earlier -> older copy.
  share = how much of Anthropic's copy is also in our download closest in time to the question (the
          start download A for the warm-up at 0:00; the end download E for the two questions at 0:20;
          fixed 2 Nov: the first version used E for all three, which understated the warm-up): of the
          runs of 5 words in a row (10 characters on pages without spaces) in Anthropic's copy, the
          fraction that also occur in that download. Both texts are cleaned like the match rule (casefold, punctuation dropped,
          publicPageHomepage_select_anchor._tokens); link addresses are removed from Anthropic's copy first. Reported as a
          spread (quartiles) per age group; the tagged-address copies (forced fresh reads) are the
          yardstick for what "the same page" looks like. No cut-off is applied.
  no copy = calls whose reply has no fetched text; the reason is read from the raw reply (Anthropic's
          error_code, or no fetch attempted).
  Whether a page is "without spaces" is decided as in publicPageHomepage_score_table.page_spaceless.

Usage: publicPageHomepage_score_anthropicCopies.py <answers.jsonl> <out-dir>   (downloads next to it as <answers>.copies.jsonl)
"""
import sys, os, re, csv, json, statistics, collections
from datetime import datetime
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import publicPage_run as ap
from publicPageHomepage_select_anchor import _tokens
from publicPageHomepage_score_table import page_spaceless

GROUPS = ["fresh (under 2 min)", "2 min to 1 h", "1 to 24 h", "1 to 7 days", "over 7 days", "no copy"]
ROLES = ["warmup", "plain", "tagged"]
ROLE_NAME = {"warmup": "warm-up (0:00, normal address)", "plain": "normal address (0:20)", "tagged": "tagged address (0:20)"}

def _t(s): return datetime.fromisoformat(s.replace("Z", "+00:00"))

# ---- pure logic (tested in anthropic_copies_test.py) ----
def age_group(minutes):
    if minutes is None: return "no copy"
    if minutes < 2: return "fresh (under 2 min)"
    if minutes < 60: return "2 min to 1 h"
    if minutes < 24 * 60: return "1 to 24 h"
    if minutes < 7 * 24 * 60: return "1 to 7 days"
    return "over 7 days"

def reuse(warm_at, plain_at):
    if not warm_at: return "no warm-up copy"
    if not plain_at: return "no copy"
    w, p = _t(warm_at), _t(plain_at)
    return "same copy as the warm-up" if w == p else "newer copy than the warm-up" if p > w else "older copy than the warm-up"

_LINK = re.compile(r"\]\([^)]*\)|https?://\S+")
def _seq(text, sp):
    toks = _tokens(text)
    return "".join(toks) if sp else toks
def _grams(seq, n): return {tuple(seq[i:i + n]) for i in range(len(seq) - n + 1)}

def share(copy_text, page_lines, sp):
    n = 10 if sp else 5
    g = _grams(_seq(_LINK.sub(" ", copy_text or ""), sp), n)
    if not g: return None
    return len(g & _grams(_seq(" ".join(page_lines or []), sp), n)) / len(g)

def closest_download(role):
    """which of our two downloads was taken closest in time to this question"""
    return "older" if role == "warmup" else "newer"

def fetch_error(raw_replies):
    seen = False
    for raw in raw_replies or []:
        for s in ap.anthropic_fetch_status((raw or {}).get("content")):
            seen = True
            if s.get("error_code"): return s["error_code"]
    return "fetched" if seen else "no fetch attempted"

# ---- run ----
def _q(v, q):
    if len(v) < 2: return v[0] if v else None
    return statistics.quantiles(v, n=4, method="inclusive")[[0, 1, 2][q]]

def main():
    a = sys.argv[1:]
    if len(a) != 2: sys.exit(__doc__)
    ans_p, out = a; os.makedirs(out, exist_ok=True)
    copies = collections.defaultdict(dict)
    for l in open(ans_p + ".copies.jsonl"):
        c = json.loads(l); copies[c["ask_id"]][c["role"]] = c.get("lines") or []
    rows, by_page = [], collections.defaultdict(dict)
    for l in open(ans_p):
        r = json.loads(l)
        if r.get("skipped") or r.get("vendor") != "anthropic": continue
        A, E = copies[r["ask_id"]].get("older", []), copies[r["ask_id"]].get("newer", [])
        sp = page_spaceless(r["anchor"], A, E)
        ft = ap.unpack_raw(r["fetched_texts"]) if r.get("fetched_texts") else []
        at = ft[0]["retrieved_at"] if ft and ft[0].get("retrieved_at") else None
        age = (_t(r["sent_at"]) - _t(at)).total_seconds() / 60 if at else None
        raw = ap.unpack_raw(r["raw"]) if r.get("raw") else []
        row = {"page": r["origin"], "role": r["role"], "without_spaces": sp, "retrieved_at": at,
               "age_min": None if age is None else round(age, 1), "age_group": age_group(age),
               "compared_with": "start download A" if closest_download(r["role"]) == "older" else "end download E",
               "share_with_our_download": None if not ft else share(ft[0].get("text"), copies[r["ask_id"]].get(closest_download(r["role"]), []), sp),
               "fetch": "fetched" if ft else fetch_error(raw if isinstance(raw, list) else [raw])}
        rows.append(row); by_page[r["origin"]][r["role"]] = row
    with open(os.path.join(out, "publicPageHomepage_score_anthropicCopies.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(rows[0])); w.writeheader(); w.writerows(rows)

    lines = []
    def say(x=""): print(x); lines.append(x)
    say("Anthropic's own copies, public-page staleness run 2 Nov 2026 (rules in publicPageHomepage_score_anthropicCopies.py)")
    say(f"pages: {len(by_page)}")

    say("\nTable A. How old the copy Anthropic used was (question time minus Anthropic's retrieved_at)")
    ta = []
    for role in ROLES:
        c = collections.Counter(r["age_group"] for r in rows if r["role"] == role)
        ta.append({"question": ROLE_NAME[role], **{g: c[g] for g in GROUPS}, "total": sum(c.values())})
        say(f"  {ROLE_NAME[role]:32s} " + "  ".join(f"{g}: {c[g]}" for g in GROUPS))
    with open(os.path.join(out, "table_a_age.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(ta[0])); w.writeheader(); w.writerows(ta)

    say("\nTable B. The normal question 20 min after the warm-up: which copy did it use?")
    tb = collections.Counter(reuse((d.get("warmup") or {}).get("retrieved_at"), (d.get("plain") or {}).get("retrieved_at"))
                             for d in by_page.values())
    for k, n in tb.most_common(): say(f"  {n:3d}  {k}")
    with open(os.path.join(out, "table_b_reuse.csv"), "w", newline="") as f:
        w = csv.writer(f); w.writerow(["normal question used", "pages"]); w.writerows(tb.most_common())

    say("\nTable C. How much of Anthropic's copy is also in our download closest in time (warm-up: start download; later questions: end download), share of 5-word runs, by copy age")
    tc = []
    for role in ROLES:
        for g in GROUPS[:-1]:
            v = [r["share_with_our_download"] for r in rows if r["role"] == role and r["age_group"] == g
                 and r["share_with_our_download"] is not None]
            if not v: continue
            row = {"question": ROLE_NAME[role], "copy age": g, "copies": len(v), "lowest quarter up to": round(_q(v, 0), 3),
                   "median": round(statistics.median(v), 3), "highest quarter from": round(_q(v, 2), 3)}
            tc.append(row)
            say(f"  {ROLE_NAME[role]:32s} {g:20s} copies {len(v):3d}  quartiles {row['lowest quarter up to']:.0%} / "
                f"{row['median']:.0%} / {row['highest quarter from']:.0%}")
    with open(os.path.join(out, "table_c_share.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(tc[0])); w.writeheader(); w.writerows(tc)

    say("\nTable D. Calls with no copy: what Anthropic's reply says")
    td = collections.Counter((r["role"], r["fetch"]) for r in rows if r["age_group"] == "no copy")
    sites = collections.defaultdict(set)
    for r in rows:
        if r["age_group"] == "no copy": sites[r["fetch"]].add(r["page"])
    for role in ROLES:
        say(f"  {ROLE_NAME[role]:32s} " + "  ".join(f"{k[1]}: {n}" for k, n in sorted(td.items()) if k[0] == role))
    for k, s in sites.items(): say(f"  sites with {k}: {len(s)}")
    with open(os.path.join(out, "table_d_no_copy.csv"), "w", newline="") as f:
        w = csv.writer(f); w.writerow(["question", "reason", "calls"]); w.writerows([(ROLE_NAME[k[0]], k[1], n) for k, n in sorted(td.items())])
    with open(os.path.join(out, "summary.txt"), "w") as f: f.write("\n".join(lines) + "\n")

if __name__ == "__main__": main()
