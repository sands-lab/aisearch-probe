#!/usr/bin/env python3
"""Look up existing PUBLIC Cloudflare URL Scanner scans for each site, without scanning anything.
Searching and reading old reports only touch Cloudflare's records, so nothing reaches the sites.
This is the search-first half of the site filter (the paper we follow, Habib et al. IMC '26, took
Cloudflare categories from URL Scanner). Sites with no recent public scan are only COUNTED here;
submitting new scans is a separate, later step that needs an explicit OK.

Labels come from our host's own entry in the report's meta.processors.domainCategories.data
(its content + inherited.content; shape seen in real reports 2026-10-22). A site is excluded if
any label is on the paper's list (EXCLUDE). The scanned page's address is never stored.

Gentle by design: one request at a time, 1 per 12 s (documented free-plan limit 1 per 10 s, plus margin), and the run STOPS at the first 429.
Env: CLOUDFLARE_API_TOKEN (URL Scanner Read), CLOUDFLARE_ACCOUNT_ID
Usage: publicPageHomepage_select_urlScanSearch.py <sites.txt> <out.jsonl> [days=30, max 30]
Pure logic is unit-tested in search_public_scans_test.py.
"""
import sys, os, json, time, urllib.request, urllib.parse
from urllib.error import HTTPError

API = "https://api.cloudflare.com/client/v4/accounts/{acct}/urlscanner/v2"
PACE = 12.0   # Cloudflare free plan: "1 per 10 seconds" (scan-limits page, 2026-04-16) + 2 s margin

# ---- pure logic (tested) --------------------------------------------------
def host_of(line):
    line = line.strip().split(",")[0]
    if "://" in line: line = urllib.parse.urlsplit(line).hostname or ""
    return line

MAX_DAYS = 30   # Cloudflare: "Too long a date range. Max is 1 month." (measured 2026-10-22)

def search_query(host, days):
    if days > MAX_DAYS: raise ValueError(f"search range {days}d is over Cloudflare's 1-month maximum")
    return f'page.domain:"{host}" AND date:>now-{days}d'

def newest(j):
    res = (j or {}).get("results") or []
    return max(res, key=lambda r: (r.get("task") or {}).get("time", "")) if res else None

def domain_categories_section(report):
    return ((report or {}).get("meta") or {}).get("processors", {}).get("domainCategories")

# Habib et al., IMC '26 (doi 10.1145/3777912.3839788), Appendix C / Table 4 "explicit risk" labels
EXCLUDE = ["CIPA", "CIPA Filter", "Adult Themes", "Pornography", "Gambling"]

def labels_for(host, section):
    """Our host's own entry in a report's domainCategories (content + inherited.content), in
    Cloudflare's order, deduplicated. None if the report has no entry for our host."""
    for e in (section or {}).get("data") or []:
        if e.get("name") == host:
            raw = (e.get("content") or []) + ((e.get("inherited") or {}).get("content") or [])
            out = []
            for c in raw:
                n = c.get("name")
                if n and n not in out: out.append(n)
            return out
    return None

def excluded_by(labels):
    return [x for x in EXCLUDE if x in (labels or [])]

def summarize(recs):
    return {"searched": len(recs),
            "found a recent public scan": sum(1 for r in recs if r["found"] and not r["error"]),
            "no recent public scan": sum(1 for r in recs if not r["found"] and not r["error"]),
            "lookup failed": sum(1 for r in recs if r["error"])}

# ---- I/O ------------------------------------------------------------------
class Stop(Exception): pass

def _get(url, token):
    time.sleep(PACE)
    req = urllib.request.Request(url, headers={"Authorization": f"Bearer {token}"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return json.load(r)
    except HTTPError as e:
        if e.code == 429: raise Stop()
        raise

def check(host, base, token, days):
    # The scanned page's address is deliberately NOT stored: other people's public scans can
    # carry tracking tokens and email addresses in it (seen 2026-10-22).
    rec = {"host": host, "found": False, "scan_id": None, "scan_time": None,
           "labels": None, "excluded_by": [], "error": ""}
    try:
        j = _get(base + "/search?" + urllib.parse.urlencode({"q": search_query(host, days), "size": 10}), token)
        n = newest(j)
        if n:
            rec.update(found=True, scan_id=n.get("_id"), scan_time=(n.get("task") or {}).get("time"))
            sec = domain_categories_section(_get(base + "/result/" + n["_id"], token))
            rec["labels"] = labels_for(host, sec)
            rec["excluded_by"] = excluded_by(rec["labels"])
    except Stop:
        raise
    except HTTPError as e:
        rec["error"] = f"HTTP {e.code}"
        try: rec["error_body"] = e.read(600).decode("utf-8", "replace")   # Cloudflare's own message
        except Exception: pass
    except Exception as e:
        rec["error"] = type(e).__name__
    return rec

def main():
    if len(sys.argv) not in (3, 4): sys.exit(__doc__)
    token, acct = os.environ.get("CLOUDFLARE_API_TOKEN"), os.environ.get("CLOUDFLARE_ACCOUNT_ID")
    if not (token and acct): sys.exit("set CLOUDFLARE_API_TOKEN (URL Scanner Read) and CLOUDFLARE_ACCOUNT_ID")
    src, out = sys.argv[1], sys.argv[2]; days = int(sys.argv[3]) if len(sys.argv) == 4 else 30
    hosts = [h for h in (host_of(l) for l in open(src)) if h]
    base = API.format(acct=acct); recs = []
    with open(out, "x") as fo:
        try:
            for i, h in enumerate(hosts, 1):
                r = check(h, base, token, days); recs.append(r)
                fo.write(json.dumps(r) + "\n"); fo.flush()
                if i % 50 == 0: print(f"  {i}/{len(hosts)}  {summarize(recs)}", flush=True)
        except Stop:
            print(f"\nSTOPPED at the first 429 after {len(recs)} sites — nothing retried.")
    print("\nDONE", summarize(recs))
    from collections import Counter
    found = [r for r in recs if r["found"] and not r["error"]]
    print(f"  of the {len(found)} found: our host absent from the report: "
          f"{sum(1 for r in found if r['labels'] is None)}, no labels: {sum(1 for r in found if r['labels'] == [])}, "
          f"excluded (paper's list): {sum(1 for r in found if r['excluded_by'])}")
    print("  excluded by label:", dict(Counter(x for r in found for x in r["excluded_by"])))

if __name__ == "__main__": main()
