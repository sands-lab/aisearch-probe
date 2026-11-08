#!/usr/bin/env python3
"""Find public XML, Markdown and CSV files published by the watched sites, for the file-type check
(method: docs/experiments/publicPage.md). Looks only in the standard places sites announce
files — no crawling:
  XML       Sitemap: lines in robots.txt (else /sitemap.xml); RSS/Atom <link rel=alternate> on the homepage
  Markdown  /llms.txt
  CSV       <a href="*.csv"> on the homepage; .csv entries in the first sitemap file
A file counts by the type the server DECLARES (Content-Type), because that is what the finding
under test turns on. Files over 200 KB are recorded but not kept (cost: some vendors read and
bill all of it). Each file is checked against ITS OWN host's robots.txt, for our name and the
4 AI fetchers. At most ~5 requests per site, once; 4 sites at a time; honest name tag
($PROBE_CONTACT, never in code). The first 20 text lines of each kept file are stored as the
answer key for the later AI question.
Pure logic is unit-tested in publicPageXmlFile_select_test.py.
Usage: PROBE_CONTACT=... publicPageXmlFile_select.py <sites.txt> <out-dir> [per_format=20]
"""
import sys, os, re, json, time, random, threading, urllib.request, urllib.parse
from urllib.error import HTTPError
from html.parser import HTMLParser
from concurrent.futures import ThreadPoolExecutor, as_completed
from publicPageHomepage_select_sites import (name_tag, failure_type, parse_robots, robots_allows, robots_unavailable_verdict,
                       OUR_TOKEN, QUERY_FETCHERS, CTX, TIMEOUT)

CAP = 200 * 1024
SEED = 20261024
FORMATS = {"xml": {"application/xml", "text/xml", "application/rss+xml", "application/atom+xml"},
           "markdown": {"text/markdown", "text/x-markdown"},
           "csv": {"text/csv", "application/csv"}}

# ---- pure logic (tested) --------------------------------------------------
def sitemap_urls(robots_text):
    out = []
    for line in robots_text.splitlines():
        line = line.strip()
        if line.startswith("#"): continue
        m = re.match(r"(?i)sitemap\s*:\s*(\S+)", line)
        if m: out.append(m.group(1))
    return out

class _Links(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True); self.feeds = []; self.anchors = []
    def handle_starttag(self, tag, attrs):
        a = {k.lower(): (v or "") for k, v in attrs}
        if tag == "link" and "alternate" in a.get("rel", "").lower().split() \
                and a.get("type", "").lower() in ("application/rss+xml", "application/atom+xml") and a.get("href"):
            self.feeds.append(a["href"])
        elif tag == "a" and a.get("href"):
            self.anchors.append(a["href"])

def _parse(html):
    p = _Links()
    try: p.feed(html); p.close()
    except Exception: pass
    return p

def feed_links(html, base):
    return [urllib.parse.urljoin(base, h) for h in _parse(html).feeds]

def csv_links(html, base):
    return [urllib.parse.urljoin(base, h) for h in _parse(html).anchors
            if urllib.parse.urlsplit(h).path.lower().endswith(".csv")]

def sitemap_csv_locs(xml):
    return [u for u in re.findall(r"<loc>\s*([^<\s]+)\s*</loc>", xml)
            if urllib.parse.urlsplit(u).path.lower().endswith(".csv")]

def declared_format(content_type):
    mime = (content_type or "").split(";")[0].strip().lower()
    return next((f for f, s in FORMATS.items() if mime in s), None)

def named_format(url):
    """The format the file's NAME claims (llms.txt is Markdown by convention), kept next to the
    declared Content-Type so 'named Markdown, served as plain text' is visible."""
    path = urllib.parse.urlsplit(url).path.lower()
    if path.endswith("/llms.txt") or path.endswith(".md"): return "markdown"
    if path.endswith(".csv"): return "csv"
    if path.endswith(".xml") or path.endswith(".rss") or path.endswith(".atom"): return "xml"
    return None

def unpack(body):
    """Some servers send gzip bytes even when asked for identity encoding (seen 2026-10-25); unpack
    them so the stored text (answer key) is the real file, not compressed bytes."""
    if body[:2] == b"\x1f\x8b":
        import gzip, zlib
        try: return gzip.decompress(body)
        except (OSError, EOFError, zlib.error): return body
    return body

def pick(cands, per_format=20, seed=SEED):
    out = []
    for fmt in sorted({c["format"] for c in cands}):
        pool = [c for c in cands if c["format"] == fmt]
        random.Random(seed).shuffle(pool)
        seen = set()
        for c in pool:
            if c["site"] in seen: continue
            seen.add(c["site"]); out.append(c)
            if len(seen) == per_format: break
    return out

# ---- I/O ------------------------------------------------------------------
def get_capped(url, ua, cap=CAP, accept="*/*", with_headers=False):
    """-> (status, final_url, content_type, bytes_read, body_or_None_if_over_cap). Uncompressed on
    purpose, so the size read is the real size."""
    req = urllib.request.Request(url, headers={"User-Agent": ua, "Accept": accept, "Accept-Encoding": "identity"})
    try: r = urllib.request.urlopen(req, timeout=TIMEOUT, context=CTX)
    except HTTPError as e: r = e
    body = r.read(cap + 1)
    st = r.status if hasattr(r, "status") else r.code
    out = (st, r.geturl(), r.headers.get("Content-Type"), len(body), (body if len(body) <= cap else None))
    return out + (list(r.headers.items()),) if with_headers else out

class Robots:
    def __init__(self, ua): self.ua, self.cache, self.lock = ua, {}, threading.Lock()
    def text(self, host_root):
        with self.lock:
            if host_root in self.cache: return self.cache[host_root]
        try:
            st, _, _, _, b = get_capped(host_root + "/robots.txt", self.ua)
            val = (st, (b or b"").decode("utf-8", "replace") if st == 200 else "")
        except Exception:
            val = (None, "")
        with self.lock: self.cache[host_root] = val
        return val
    def verdicts(self, url):
        p = urllib.parse.urlsplit(url); root = f"{p.scheme}://{p.netloc}"
        path = p.path or "/"
        if p.query: path += "?" + p.query
        st, txt = self.text(root)
        if st == 200:
            g = parse_robots(txt); ok = lambda n: robots_allows(g, n, path)
        else:
            v = robots_unavailable_verdict(st); ok = lambda n: v
        return {"us": ok(OUR_TOKEN), "ai": all(ok(n) for n in QUERY_FETCHERS.values())}

def check_file(site, url, via, ua, robots):
    rec = {"site": site, "url": url, "found_via": via, "named_format": named_format(url)}
    v = robots.verdicts(url); rec["robots_us"], rec["robots_ai"] = v["us"], v["ai"]
    if not v["us"]: rec["skipped"] = "robots"; return rec, ""
    text = ""
    try:
        st, final, ct, n, body = get_capped(url, ua)
        rec.update(status=st, final_url=final, content_type=ct, bytes=n, over_cap=body is None,
                   format=declared_format(ct))
        if body is not None and st == 200:
            text = unpack(body).decode("utf-8", "replace")
            rec["answer_key"] = [l.strip() for l in text.splitlines() if l.strip()][:20]
    except Exception as e:
        rec["error"] = failure_type(e)
    return rec, text

def scan_site(site, ua, robots):
    site = site.rstrip("/"); out = []
    st, rtxt = robots.text(site)
    sitemaps = sitemap_urls(rtxt) if st == 200 else []
    cands = [(u, "robots.txt Sitemap line") for u in sitemaps[:1]] or [(site + "/sitemap.xml", "/sitemap.xml")]
    try:
        hst, hfinal, _, _, hbody = get_capped(site + "/", ua, cap=2 * 1024 * 1024)
        if hst == 200 and hbody:
            html = hbody.decode("utf-8", "replace")
            cands += [(u, "homepage feed link") for u in feed_links(html, hfinal)[:2]]
            cands += [(u, "homepage .csv link") for u in csv_links(html, hfinal)[:2]]
    except Exception:
        pass
    cands.append((site + "/llms.txt", "/llms.txt"))
    sitemap_text = ""
    for u, via in cands:
        r, text = check_file(site, u, via, ua, robots); out.append(r)
        if via in ("robots.txt Sitemap line", "/sitemap.xml") and r.get("status") == 200: sitemap_text = text
    # .csv entries listed in the first sitemap (already downloaded above, if within the size cap)
    for u in sitemap_csv_locs(sitemap_text)[:1]:
        out.append(check_file(site, u, "sitemap .csv entry", ua, robots)[0])
    return out

def main():
    if len(sys.argv) not in (3, 4): sys.exit(__doc__)
    ua = name_tag(os.environ)
    src, outdir = sys.argv[1], sys.argv[2]; per = int(sys.argv[3]) if len(sys.argv) == 4 else 20
    sites = [l.strip() for l in open(src) if l.strip()]
    os.makedirs(outdir, exist_ok=True)
    stamp = time.strftime("%Y%m%d-%H%M%S", time.gmtime())
    allf = os.path.join(outdir, f"files-all-{stamp}.jsonl")
    robots = Robots(ua); recs = []; t0 = time.time()
    with open(allf, "x") as fo, ThreadPoolExecutor(max_workers=4) as ex:
        for i, f in enumerate(as_completed({ex.submit(scan_site, s, ua, robots): s for s in sites}), 1):
            for r in f.result():
                recs.append(r); fo.write(json.dumps(r) + "\n")
            fo.flush()
            if i % 50 == 0: print(f"  {i}/{len(sites)} sites  {time.time()-t0:.0f}s", flush=True)
    keep = [r for r in recs if r.get("status") == 200 and r.get("format") and not r.get("over_cap")
            and r.get("robots_ai") and r.get("answer_key")]
    picked = pick([{"site": r["site"], "format": r["format"], "url": r["url"], "content_type": r["content_type"],
                    "bytes": r["bytes"], "found_via": r["found_via"], "answer_key": r["answer_key"]} for r in keep], per)
    with open(os.path.join(outdir, f"files-picked-{stamp}.jsonl"), "x") as f:
        f.writelines(json.dumps(r) + "\n" for r in picked)
    from collections import Counter
    print("\nDONE", len(sites), "sites,", len(recs), "files checked")
    print("  declared formats found (status 200):", dict(Counter(r.get("format") for r in recs if r.get("status") == 200)))
    print("  usable (200, xml/markdown/csv, <=200 KB, robots allows us + 4 AI fetchers):", dict(Counter(r["format"] for r in keep)))
    print("  named Markdown (llms.txt / .md) found, by declared type:",
          dict(Counter((r.get("content_type") or "(none)").split(";")[0].strip().lower()
                       for r in recs if r.get("named_format") == "markdown" and r.get("status") == 200)))
    print("  named Markdown, served as plain text (recorded, not counted as Markdown):",
          sum(1 for r in recs if r.get("named_format") == "markdown" and r.get("status") == 200
              and (r.get("content_type") or "").lower().startswith("text/plain")))
    print("  picked:", dict(Counter(r["format"] for r in picked)))

if __name__ == "__main__": main()
