#!/usr/bin/env python3
"""Robustness re-judging of the public-page staleness run (2 Nov 2026) with a consensus reference.

RULES (written before running; nothing below is tuned on the stale counts)

Match rule, unchanged from publicPageHomepage_score_answerKinds.py:
  text is casefolded, punctuation dropped, split into words (publicPageHomepage_select_anchor._tokens); on pages written
  without spaces (publicPageHomepage_select_anchor.spaceless on the text after the label) all whitespace is removed and
  characters are compared instead. Two texts match if they share a run of >= 5 words in order
  (>= 10 characters on pages without spaces).
  Answer against one of our downloads (unchanged): only the first 60 words (180 characters) after the
  label (publicPageHomepage_score_answerKinds._after); the answer text is used as it is.
  Whether a page is "without spaces" is decided from our end download E (start download A if the label
  is missing from E; the whole of E if the label is in neither).

Version 2 (2 Nov, after the first run showed answers "agreeing" only by repeating the question):
  Answer against answer: each answer first loses the words that come from ITS OWN prompt: every
  occurrence of the label, then every word that occurs anywhere in the prompt text (fixed question
  wording, the address, the label). Then the match rule above is applied to what is left.
  No quote (changed on review, 2 Nov, before any result was used): after removing its prompt words, an
  answer is a quote only if it shares an unbroken run of >= 5 words (>= 10 characters on pages without
  spaces) with the whole of our download A or E, cleaned the same way; otherwise it is "no quote". This
  is the same threshold as the main match rule. (The first draft, "no page word at all", let page
  descriptions through, since they share single words with the page.)
  A no-quote answer is excluded from agreement, matching and the consensus reference, under all rules.

Per page p and service s, one sitting (OpenAI and xAI on the pages re-asked on 2 Nov after the credit
ran out come from the re-run sitting; everything else from the main sitting). In that sitting:
  A = our download at the start, E = our download at the end,
  B = s's warm-up answer, C = s's plain-address answer 20 min later, D = s's tagged-address answer
  sent at the same moment as C.
  Not usable: C or D is an error or empty. No quote: C or D is a no-quote answer.

Rule 1, "download only": an answer matches if it matches A or E.
Rule 2, "consensus":
  sources = the usable, quoting D answers of the OTHER services in the same sitting + E (if E has the
  label). s's own D is never a source. Two sources agree if they match (answer-answer as above;
  answer-E as against a download).
  reference = the largest group of sources that all agree with each other, if it has >= 2 members;
  no agreeing pair -> "no reference"; two or more equally large largest groups -> "tie".
  an answer matches the reference if it matches at least one member.
Rule 3, "consensus without our download": as rule 2 with E removed from the sources.

Outcome per (page, service), under each rule:
  stale = D matches, C does not; chance mismatch = C matches, D does not; both match; neither matches.
Stricter stale (secondary): C matches B (answer-answer), C does not match the reference, D does.
Test: exact two-sided binomial (McNemar) on stale vs chance mismatch.
Anthropic check: of the pages called stale, how many have a plain-address copy that Anthropic itself
dates >= 2 minutes before the question (fetched_retrieved_at vs sent_at).
Also reported: false-match rate of answer-answer comparison (answers from DIFFERENT pages), the
tagged-answer agreement per page and per pair of services, and every class change against the
previous version's classes.csv.

Usage: publicPageHomepage_score_table.py <main-answers.jsonl> <redo-answers.jsonl> <out-dir> [<previous classes.csv>]
       (each answers file has its downloads next to it as <file>.copies.jsonl)
"""
import sys, os, re, csv, json, itertools, collections
from math import comb
from datetime import datetime
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from publicPageHomepage_score_answerKinds import _after, _run, MIN_RUN, MIN_CHARS
from publicPageHomepage_select_anchor import _tokens, spaceless

VENDORS = ["anthropic", "openai", "xai", "google"]
RULES = ["download only", "consensus", "consensus without our download"]

# ---- pure logic (tested in publicPageHomepage_score_table_test.py) ----
def reference(sources, agree):
    """-> ("ok", set of sources) | ("no reference", None) | ("tie", None)"""
    for k in range(len(sources), 1, -1):
        groups = [set(g) for g in itertools.combinations(sources, k)
                  if all(agree(a, b) for a, b in itertools.combinations(g, 2))]
        if len(groups) == 1: return "ok", groups[0]
        if len(groups) > 1: return "tie", None
    return "no reference", None

def outcome(c_hits, d_hits):
    if d_hits and not c_hits: return "stale"
    if c_hits and not d_hits: return "chance mismatch"
    return "both match" if c_hits else "neither matches"

def mcnemar_p(stale, chance):
    n = stale + chance
    if n == 0: return 1.0
    k = min(stale, chance)
    return min(1.0, 2 * sum(comb(n, i) for i in range(k + 1)) / 2 ** n)

def _label(prompt):
    m = re.search(r'follow the line "(.*)" in it\.', prompt or "")
    return m.group(1) if m else ""

def strip_prompt(answer, prompt, sp):
    """the answer without its prompt's words: words (or, on pages without spaces, the characters of the
    remaining words, joined)"""
    text = (answer or "").casefold()
    lab = _label(prompt).casefold()
    if lab: text = text.replace(lab, " ")
    drop = set(_tokens(prompt))
    kept = [w for w in _tokens(text) if w not in drop]
    return "".join(kept) if sp else kept

def _need(sp): return MIN_CHARS if sp else MIN_RUN

def ans_ans(a, qa, b, qb, sp):
    return _run(strip_prompt(a, qa, sp), strip_prompt(b, qb, sp))[0] >= _need(sp)

def no_quote(answer, prompt, pages, sp):
    rest = strip_prompt(answer, prompt, sp)
    for p in pages:
        if not p: continue
        toks = _tokens(" ".join(p))
        page = "".join(toks) if sp else toks
        if _run(rest, page)[0] >= _need(sp): return False
    return True

# ---- answer against a download (unchanged) ----
def _seq(text, sp): return re.sub(r"\s+", "", text or "") if sp else _tokens(text)
def ans_page(a, lines, anchor, sp):
    return bool(lines) and anchor in lines and _run(_seq(a, sp), _after(lines, anchor, sp))[0] >= _need(sp)

# ---- data ----
def _load(p): return [json.loads(l) for l in open(p)] if os.path.exists(p) else []
def _t(s): return datetime.fromisoformat(s.replace("Z", "+00:00"))

def sittings(main_p, redo_p):
    """-> list of (origin, sitting name, anchor, A, E, {vendor: {role: record}})"""
    redo_cover = {(r["origin"], r["vendor"]) for r in _load(redo_p) if r.get("vendor")}
    out = []
    for name, p in (("main", main_p), ("redo", redo_p)):
        cp = collections.defaultdict(dict)
        for c in _load(p + ".copies.jsonl"): cp[c["ask_id"]][c["role"]] = c.get("lines") or []
        by = collections.defaultdict(lambda: collections.defaultdict(dict)); anc = {}; org = {}
        for r in _load(p):
            if r.get("skipped") or not r.get("vendor"): continue
            if name == "main" and (r["origin"], r["vendor"]) in redo_cover: continue
            by[r["ask_id"]][r["vendor"]][r["role"]] = r; anc[r["ask_id"]] = r["anchor"]; org[r["ask_id"]] = r["origin"]
        for aid, vs in by.items():
            out.append((org[aid], name, anc[aid], cp[aid].get("older", []), cp[aid].get("newer", []), vs))
    return out

def page_spaceless(anchor, A, E):
    for L in (E, A):
        if anchor in L: return spaceless(" ".join(L[L.index(anchor) + 1:])[:200])
    return spaceless(" ".join(E)[:2000])

def usable(r): return bool(r) and not r.get("error") and bool((r.get("answer") or "").strip())

def judge(anchor, A, E, vs, s, sp):
    """-> dict rule -> (status, outcome or None, strict_stale, E in reference)"""
    nq = lambda r: no_quote(r["answer"], r["question"], [A, E], sp)
    rec = vs[s]; B, C, D = rec.get("warmup"), rec.get("plain"), rec.get("tagged")
    if not (usable(C) and usable(D)):
        return {rule: ("not usable", None, False, False) for rule in RULES}
    if nq(C) or nq(D):
        return {rule: ("no quote", None, False, False) for rule in RULES}
    b_ok = usable(B) and not nq(B)
    rep = lambda: b_ok and ans_ans(C["answer"], C["question"], B["answer"], B["question"], sp)
    res = {}
    hit = lambda a: ans_page(a, A, anchor, sp) or ans_page(a, E, anchor, sp)
    if anchor not in A and anchor not in E: res["download only"] = ("no reference", None, False, False)
    else:
        c, d = hit(C["answer"]), hit(D["answer"])
        res["download only"] = ("ok", outcome(c, d), d and not c and rep(), True)
    others = {f"D:{v}": vs[v]["tagged"] for v in vs if v != s and usable(vs[v].get("tagged")) and not nq(vs[v]["tagged"])}
    for rule, with_e in (("consensus", True), ("consensus without our download", False)):
        srcs = list(others) + (["E"] if with_e and anchor in E else [])
        def agree(x, y):
            if "E" in (x, y):
                o = others[y if x == "E" else x]
                return ans_page(o["answer"], E, anchor, sp)
            return ans_ans(others[x]["answer"], others[x]["question"], others[y]["answer"], others[y]["question"], sp)
        st, ref = reference(srcs, agree)
        if st != "ok": res[rule] = (st, None, False, False); continue
        m = lambda r: any(ans_page(r["answer"], E, anchor, sp) if x == "E"
                          else ans_ans(r["answer"], r["question"], others[x]["answer"], others[x]["question"], sp) for x in ref)
        c, d = m(C), m(D)
        res[rule] = ("ok", outcome(c, d), d and not c and rep(), "E" in ref)
    return res

def anthropic_age_min(rec):
    ra = rec.get("fetched_retrieved_at") or []
    return (_t(rec["sent_at"]) - _t(ra[0])).total_seconds() / 60 if ra else None

def _grams(seq, n): return {tuple(seq[i:i + n]) for i in range(len(seq) - n + 1)}

def main():
    a = sys.argv[1:]
    if len(a) not in (3, 4): sys.exit(__doc__)
    main_p, redo_p, out = a[:3]; prev_p = a[3] if len(a) == 4 else None
    os.makedirs(out, exist_ok=True)
    S = sittings(main_p, redo_p)
    lines = []
    def say(x=""): print(x); lines.append(x)
    say("Public-page staleness, 2 Nov 2026: re-judged with a consensus reference, version 2 (rules in publicPageHomepage_score_table.py)")

    # 1. false-match check for answer against answer (answers from different pages)
    items = []
    for origin, name, anchor, A, E, vs in S:
        sp = page_spaceless(anchor, A, E)
        for v, roles in vs.items():
            for role, r in roles.items():
                if usable(r) and not no_quote(r["answer"], r["question"], [A, E], sp):
                    seq = strip_prompt(r["answer"], r["question"], sp)
                    items.append((origin, sp, _grams(seq, _need(sp))))
    fm = collections.Counter(); fm_n = collections.Counter(); fm_pairs = collections.Counter()
    for (o1, s1, g1), (o2, s2, g2) in itertools.combinations(items, 2):
        if o1 == o2 or s1 != s2: continue
        k = "pages without spaces" if s1 else "pages with spaces"
        fm_n[k] += 1
        if g1 & g2: fm[k] += 1; fm_pairs[tuple(sorted((o1, o2)))] += 1
    say("\n== 1. False matches, answer against answer, answers from DIFFERENT pages (prompt words removed)")
    for k in ("pages with spaces", "pages without spaces"):
        say(f"  {k:22s} {fm[k]} of {fm_n[k]} comparisons count as agreeing")
    for (x, y), n in fm_pairs.most_common(10): say(f"     {n:3d}  {x}  <->  {y}")
    with open(os.path.join(out, "false_matches.csv"), "w", newline="") as f:
        w = csv.writer(f); w.writerow(["page A", "page B", "agreeing answer pairs"]); w.writerows([(x, y, n) for (x, y), n in fm_pairs.most_common()])

    # 2. no-quote answers per service and role
    nqc = collections.Counter(); allc = collections.Counter()
    for origin, name, anchor, A, E, vs in S:
        sp = page_spaceless(anchor, A, E)
        for v, roles in vs.items():
            for role, r in roles.items():
                if not usable(r): continue
                allc[(v, role)] += 1; nqc[(v, role)] += no_quote(r["answer"], r["question"], [A, E], sp)
    say("\n== 2. No-quote answers (no page words left after removing prompt words), of usable answers")
    for v in VENDORS:
        say(f"  {v:9s} " + "  ".join(f"{role} {nqc[(v, role)]} of {allc[(v, role)]}" for role in ("warmup", "plain", "tagged")))

    # 3. per page x service classes under the three rules
    rows = []
    for origin, name, anchor, A, E, vs in S:
        sp = page_spaceless(anchor, A, E)
        for s in vs:
            j = judge(anchor, A, E, vs, s, sp)
            age = anthropic_age_min(vs[s]["plain"]) if s == "anthropic" and vs[s].get("plain") else None
            rows.append({"page": origin, "sitting": name, "service": s, "without_spaces": sp, "anthropic_copy_age_min": age,
                         **{f"{rule}|{k}": v for rule in RULES for k, v in zip(("status", "outcome", "strict_stale", "download_in_reference"), j[rule])}})
    with open(os.path.join(out, "classes.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(rows[0])); w.writeheader(); w.writerows(rows)
    subsets = [("all pages", lambda r: True), ("pages with spaces", lambda r: not r["without_spaces"]),
               ("pages without spaces", lambda r: r["without_spaces"])]
    count_rows = []
    for sub, keep in subsets:
        for rule in RULES:
            for s in VENDORS:
                sel = [r for r in rows if r["service"] == s and keep(r)]
                st = collections.Counter(r[f"{rule}|status"] for r in sel)
                oc = collections.Counter(r[f"{rule}|outcome"] for r in sel if r[f"{rule}|status"] == "ok")
                count_rows.append({"subset": sub, "rule": rule, "service": s, "pages": len(sel),
                                   "not usable": st["not usable"], "no quote": st["no quote"], "no reference": st["no reference"],
                                   "tie": st["tie"], "judged": st["ok"], "stale": oc["stale"], "chance mismatch": oc["chance mismatch"],
                                   "both match": oc["both match"], "neither matches": oc["neither matches"],
                                   "p (stale vs chance mismatch)": round(mcnemar_p(oc["stale"], oc["chance mismatch"]), 6),
                                   "stricter stale (C repeats warm-up)": sum(1 for r in sel if r[f"{rule}|strict_stale"]),
                                   "our download in reference": sum(1 for r in sel if r[f"{rule}|download_in_reference"]) if rule == "consensus" else ""})
    with open(os.path.join(out, "counts.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(count_rows[0])); w.writeheader(); w.writerows(count_rows)
    for sub, _ in subsets:
        say(f"\n== 3. Classes, {sub}")
        for rule in RULES:
            say(f"  -- rule: {rule}")
            for c in count_rows:
                if c["subset"] == sub and c["rule"] == rule:
                    say(f"     {c['service']:9s} pages {c['pages']:3d} | not usable {c['not usable']:2d} no quote {c['no quote']:2d} no ref {c['no reference']:2d} tie {c['tie']:2d} | "
                        f"stale {c['stale']:2d} chance {c['chance mismatch']:2d} both {c['both match']:2d} neither {c['neither matches']:2d} | "
                        f"p {c['p (stale vs chance mismatch)']:.4g} | stricter stale {c['stricter stale (C repeats warm-up)']}"
                        + (f" | our download in reference {c['our download in reference']}" if rule == "consensus" else ""))

    # 4. tagged answers: do the services agree with each other, and with our download? (the "32 of 90" count)
    cases = collections.Counter(); pair_ok = collections.Counter(); pair_n = collections.Counter()
    for origin, name, anchor, A, E, vs in S:
        if name != "main": continue
        sp = page_spaceless(anchor, A, E)
        tg = {v: vs[v]["tagged"] for v in vs if usable(vs[v].get("tagged")) and not no_quote(vs[v]["tagged"]["answer"], vs[v]["tagged"]["question"], [A, E], sp)}
        agree = set()
        for x, y in itertools.combinations(sorted(tg), 2):
            pair_n[(x, y)] += 1
            if ans_ans(tg[x]["answer"], tg[x]["question"], tg[y]["answer"], tg[y]["question"], sp): pair_ok[(x, y)] += 1; agree |= {x, y}
        e_match = {v for v in tg if ans_page(tg[v]["answer"], E, anchor, sp)}
        if len(tg) < 2: cases["fewer than 2 quoting tagged answers"] += 1
        elif not agree: cases["no two services agree"] += 1
        elif agree <= e_match: cases["services agree, and match our download"] += 1
        elif not (agree & e_match): cases["services agree with each other, NOT with our download"] += 1
        else: cases["services agree, partly match our download"] += 1
    say("\n== 4. Tagged answers (main sitting): services against each other and against our download")
    for k, n in cases.most_common(): say(f"  {n:3d}  {k}")
    say("  pairs of services quoting the same words (tagged answers):")
    for p in sorted(pair_n): say(f"     {p[0]:9s} + {p[1]:9s} {pair_ok[p]:3d} of {pair_n[p]}")

    # 5. Anthropic date check
    say("\n== 5. Anthropic: pages called stale vs Anthropic's own copy date")
    val = []
    for rule in RULES:
        flagged = [r for r in rows if r["service"] == "anthropic" and r[f"{rule}|outcome"] == "stale"]
        old = sum(1 for r in flagged if r["anthropic_copy_age_min"] is not None and r["anthropic_copy_age_min"] >= 2)
        fresh = sum(1 for r in flagged if r["anthropic_copy_age_min"] is not None and r["anthropic_copy_age_min"] < 2)
        val.append({"rule": rule, "called stale": len(flagged), "dated >= 2 min old": old, "dated < 2 min": fresh, "no date": len(flagged) - old - fresh})
        say(f"  {rule:32s} called stale {len(flagged):2d}: dated old {old}, dated fresh {fresh}, no date {len(flagged) - old - fresh}")
    with open(os.path.join(out, "anthropic_validation.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(val[0])); w.writeheader(); w.writerows(val)

    # 6. class changes against the previous version
    if prev_p and os.path.exists(prev_p):
        prev = {(r["page"], r["service"]): r for r in csv.DictReader(open(prev_p))}
        say("\n== 6. Class changes against the previous version (status or outcome), per rule")
        ch_rows = []
        for r in rows:
            p = prev.get((r["page"], r["service"]))
            if not p: continue
            for rule in RULES:
                old = p[f"{rule}|outcome"] or p[f"{rule}|status"]; new = r[f"{rule}|outcome"] or r[f"{rule}|status"]
                if str(old) != str(new): ch_rows.append({"page": r["page"], "service": r["service"], "rule": rule, "before": old, "after": new})
        cnt = collections.Counter((c["rule"], c["service"]) for c in ch_rows)
        for rule in RULES: say(f"  {rule:32s} " + "  ".join(f"{s} {cnt[(rule, s)]}" for s in VENDORS))
        with open(os.path.join(out, "changes.csv"), "w", newline="") as f:
            w = csv.DictWriter(f, fieldnames=["page", "service", "rule", "before", "after"]); w.writeheader(); w.writerows(ch_rows)
        say("  Anthropic, consensus rule:")
        for c in ch_rows:
            if c["service"] == "anthropic" and c["rule"] == "consensus": say(f"     {c['page']:36s} {c['before']:16s} -> {c['after']}")
    with open(os.path.join(out, "summary.txt"), "w") as f: f.write("\n".join(lines) + "\n")

if __name__ == "__main__": main()
