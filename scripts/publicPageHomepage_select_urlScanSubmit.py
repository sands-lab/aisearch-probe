#!/usr/bin/env python3
"""Order NEW public Cloudflare URL Scanner scans for the sites the search left empty, then read
their category labels. Second half of the site filter (search first: publicPageHomepage_select_urlScanSearch.py).
Each scan makes Cloudflare's own browser visit the site's homepage once; the report is public.

Two passes, both one request at a time, 12 s apart (Cloudflare's documented free-plan limit is
1 per 10 s; 2 s margin), and both STOP at the first 429:
  submit  — one scan per site, never twice: sites already in <submitted.jsonl> are skipped,
            failed submits included (no retry storm). Safe to restart.
  read    — read each submitted scan's report; "not found" = not ready yet, read again on a later
            run of this pass. Labels and the paper's exclusion come from publicPageHomepage_select_urlScanSearch.
The scanned address is not stored, only the host, scan ID, times and labels.

Env: CLOUDFLARE_API_TOKEN (URL Scanner Write), CLOUDFLARE_ACCOUNT_ID
Usage: publicPageHomepage_select_urlScanSubmit.py submit <sites.txt> <submitted.jsonl>
       publicPageHomepage_select_urlScanSubmit.py read   <submitted.jsonl> <results.jsonl>
       publicPageHomepage_select_urlScanSubmit.py one    <host>            # permission test: submit ONE scan, print the reply
Pure logic is unit-tested in publicPageHomepage_select_urlScanSubmit_test.py.
"""
import sys, os, json, time, urllib.request
from urllib.error import HTTPError
from publicPageHomepage_select_urlScanSearch import host_of, labels_for, excluded_by, domain_categories_section

API = "https://api.cloudflare.com/client/v4/accounts/{acct}/urlscanner/v2"
PACE = 12.0

# ---- pure logic (tested) --------------------------------------------------
def still_to_submit(hosts, submitted):
    seen = {r["host"] for r in submitted}
    return [h for h in hosts if h not in seen]

def parse_submit(resp):
    return {"uuid": resp.get("uuid"), "visibility": resp.get("visibility")}

def still_to_read(submitted, results):
    ready = {(r["host"], r["uuid"]) for r in results if r.get("ready")}
    return [(r["host"], r["uuid"]) for r in submitted if r.get("uuid") and (r["host"], r["uuid"]) not in ready]

def labels_with_fallback(host, section):
    """Our host's own labels; if its entry is missing OR empty (the homepage redirected, e.g.
    s.tabelog.com -> tabelog.com), the labels of the entry the browser ended on (isPrimary).
    -> (hostname the labels came from, labels)."""
    own = labels_for(host, section)
    if own: return host, own
    for e in (section or {}).get("data") or []:
        if e.get("isPrimary"):
            prim = labels_for(e.get("name"), section)
            if prim: return e.get("name"), prim
    return (host, own) if own is not None else (None, None)

def scan_body(host):
    return {"url": f"https://{host}/", "visibility": "Public"}

# ---- I/O ------------------------------------------------------------------
class Stop(Exception): pass

def _call(url, token, body=None):
    time.sleep(PACE)
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method="POST" if body is not None else "GET",
                                 headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return json.load(r)
    except HTTPError as e:
        if e.code == 429: raise Stop()
        raise

def _err(e):
    out = {"error": f"HTTP {e.code}" if isinstance(e, HTTPError) else type(e).__name__}
    if isinstance(e, HTTPError):
        try: out["error_body"] = e.read(600).decode("utf-8", "replace")
        except Exception: pass
    return out

def _load(path):
    return [json.loads(l) for l in open(path)] if os.path.exists(path) else []

def main():
    if len(sys.argv) < 3: sys.exit(__doc__)
    token, acct = os.environ.get("CLOUDFLARE_API_TOKEN"), os.environ.get("CLOUDFLARE_ACCOUNT_ID")
    if not (token and acct): sys.exit("set CLOUDFLARE_API_TOKEN (URL Scanner Write) and CLOUDFLARE_ACCOUNT_ID")
    base = API.format(acct=acct); mode = sys.argv[1]
    try:
        if mode == "one":
            try: print(json.dumps(_call(base + "/scan", token, scan_body(sys.argv[2])), indent=2))
            except HTTPError as e: print(json.dumps(_err(e), indent=2))
        elif mode == "submit":
            src, subs = sys.argv[2], sys.argv[3]
            hosts = [h for h in (host_of(l) for l in open(src)) if h]
            todo = still_to_submit(hosts, _load(subs)); print(f"to submit: {len(todo)} of {len(hosts)}", flush=True)
            with open(subs, "a") as fo:
                for i, h in enumerate(todo, 1):
                    rec = {"host": h, "submitted": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "error": ""}
                    try: rec.update(parse_submit(_call(base + "/scan", token, scan_body(h))))
                    except Stop: raise
                    except Exception as e: rec.update(uuid=None, **_err(e))
                    fo.write(json.dumps(rec) + "\n"); fo.flush()
                    if i % 50 == 0: print(f"  submitted {i}/{len(todo)}", flush=True)
        elif mode == "read":
            subs, res = sys.argv[2], sys.argv[3]
            todo = still_to_read(_load(subs), _load(res)); print(f"to read: {len(todo)}", flush=True)
            with open(res, "a") as fo:
                for i, (h, u) in enumerate(todo, 1):
                    rec = {"host": h, "uuid": u, "read": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                           "ready": False, "labels": None, "excluded_by": [], "error": ""}
                    try:
                        sec = domain_categories_section(_call(base + "/result/" + u, token))
                        lh, labels = labels_with_fallback(h, sec)
                        rec.update(ready=True, labels=labels, labels_host=lh, categories_section=sec)
                        rec["excluded_by"] = excluded_by(labels)
                    except Stop: raise
                    except HTTPError as e:
                        if e.code != 404: rec.update(**_err(e))      # 404 = not ready yet
                    except Exception as e: rec.update(**_err(e))
                    fo.write(json.dumps(rec) + "\n"); fo.flush()
                    if i % 50 == 0: print(f"  read {i}/{len(todo)}", flush=True)
            final = {}
            for r in _load(res): final[(r["host"], r["uuid"])] = r      # latest read wins
            rs = list(final.values())
            print("DONE ready:", sum(r["ready"] for r in rs), " not ready yet:", sum(not r["ready"] for r in rs),
                  " no labels:", sum(1 for r in rs if r["ready"] and not r["labels"]),
                  " excluded (paper's list):", sum(1 for r in rs if r["excluded_by"]))
        else:
            sys.exit(__doc__)
    except Stop:
        print("\nSTOPPED at the first 429 — nothing retried; rerun later to continue.")

if __name__ == "__main__": main()
