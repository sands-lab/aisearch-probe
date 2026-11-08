#!/usr/bin/env python3
"""Survey the real pages' CACHE headers, from our runtime fetcher's point of view. Records, per
site, what the origin DECLARES about caching (Cache-Control / Expires / Pragma) and whether it
sends the VALIDATORS a crawler would need to revalidate (ETag / Last-Modified). Feeds two things:
the 'crawlers disregard declared Cache-Control' finding, and a realistic directive set to replicate
in the controlled server.

Pure logic (parse_cache_control / classify / asks_fresh) is unit-tested in survey_cache_headers_test.py.
Usage: publicPageHomepage_select_cacheHeaders.py <origins.txt> <out.jsonl> [workers]
"""
import sys, ssl, socket, json, time, urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed

TIMEOUT = 15; socket.setdefaulttimeout(TIMEOUT)
CTX = ssl.create_default_context(); CTX.check_hostname = False; CTX.verify_mode = ssl.CERT_NONE
BOTUA = "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; ChatGPT-User/1.0; +https://openai.com/bot"

# ---- pure logic (tested) --------------------------------------------------
def parse_cache_control(s):
    """'no-store, max-age=0' -> {'no-store': True, 'max-age': '0'}. Token names lowercased."""
    out = {}
    if not s: return out
    for part in s.split(","):
        part = part.strip()
        if not part: continue
        if "=" in part:
            k, v = part.split("=", 1); out[k.strip().lower()] = v.strip().strip('"')
        else:
            out[part.lower()] = True
    return out

def _maxage(cc):
    try: return int(cc.get("max-age"))
    except (TypeError, ValueError): return None

def classify(cc):
    """One strongest-signal label. Priority: no-store > no-cache > max-age=0 > max-age<5min >
    cacheable > no-header."""
    if not cc: return "no-header"
    if "no-store" in cc: return "no-store"
    if "no-cache" in cc: return "no-cache"
    ma = _maxage(cc)
    if ma == 0: return "max-age=0"
    if ma is not None and ma < 300: return "max-age<5min"
    return "cacheable"

def asks_fresh(cc):
    """True if the site effectively tells caches to get a fresh copy every time."""
    if "no-store" in cc or "no-cache" in cc: return True
    ma = _maxage(cc)
    return ma is not None and ma < 300

# ---- I/O ------------------------------------------------------------------
def probe(origin):
    rec = {"origin": origin}
    try:
        req = urllib.request.Request(origin, headers={"User-Agent": BOTUA, "Accept": "*/*"})
        with urllib.request.urlopen(req, timeout=TIMEOUT, context=CTX) as r:
            h = r.headers; rec["status"] = r.status
    except urllib.error.HTTPError as e:
        h = e.headers; rec["status"] = e.code
    except Exception as e:
        rec["status"] = None; rec["error"] = type(e).__name__; return rec
    cc_raw = h.get("Cache-Control")
    cc = parse_cache_control(cc_raw)
    rec.update({
        "cache_control": cc_raw,
        "stance": classify(cc),
        "asks_fresh": asks_fresh(cc),
        "expires": h.get("Expires"),
        "pragma": h.get("Pragma"),
        "has_etag": h.get("ETag") is not None,
        "has_last_modified": h.get("Last-Modified") is not None,
        "age": h.get("Age"),
        "cf_cache_status": h.get("CF-Cache-Status"),
        "server": h.get("Server"),
    })
    return rec

def main():
    src, out = sys.argv[1], sys.argv[2]
    workers = int(sys.argv[3]) if len(sys.argv) > 3 else 16
    origins = [l.strip() for l in open(src) if l.strip()]
    from collections import Counter
    stance = Counter(); fresh = 0; val = 0; done = 0; t0 = time.time()
    with open(out, "w") as fo, ThreadPoolExecutor(max_workers=workers) as ex:
        for f in as_completed({ex.submit(probe, o): o for o in origins}):
            r = f.result(); fo.write(json.dumps(r) + "\n"); done += 1
            if r.get("status"):
                stance[r.get("stance", "no-header")] += 1
                fresh += 1 if r.get("asks_fresh") else 0
                val += 1 if (r.get("has_etag") or r.get("has_last_modified")) else 0
            if done % 100 == 0: print(f"  {done}/{len(origins)} {done/(time.time()-t0):.1f}/s", flush=True)
    answered = sum(stance.values())
    print(f"\nsurveyed {done} sites, {answered} answered")
    print("  stance:", dict(stance))
    print(f"  asks for a fresh copy every time: {fresh}/{answered} ({100*fresh/max(answered,1):.0f}%)")
    print(f"  send a validator (ETag/Last-Modified): {val}/{answered} ({100*val/max(answered,1):.0f}%)")

if __name__ == "__main__": main()
