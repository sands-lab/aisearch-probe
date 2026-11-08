#!/usr/bin/env python3
"""Day-0 scan for the public homepage test (method: docs/experiments/publicPage.md).
One polite pass over the CrUX sample, no AI calls: per site, the homepage's status, final
address, caching headers, sizes, visible-text size, body hash, a script-only-shell flag, a
block-page sign, and robots.txt verdicts for every AI fetcher name. Failures are typed
(dns / timeout / reset / tls ...) so a block on your own network can later be told from a
site-side one by rerunning the failed sites from another network.

The name tag is honest and carries a contact address taken from $PROBE_CONTACT only. The
address is never written in code or in the output; the scan refuses to run without it.

Pure logic is unit-tested in publicPageHomepage_select_sites_test.py (TEST-FIRST).
Usage: PROBE_CONTACT=... publicPageHomepage_select_sites.py <crux.csv.gz> <out-dir> [workers]
"""
import sys, os, ssl, socket, json, time, gzip, csv, random, hashlib, re, ipaddress
import http.client, urllib.request, urllib.error, urllib.parse
from html.parser import HTMLParser
from concurrent.futures import ThreadPoolExecutor, as_completed
from publicPageHomepage_select_cacheHeaders import parse_cache_control, classify, asks_fresh

TIMEOUT = 15; socket.setdefaulttimeout(TIMEOUT)
MAX_BODY = 10 * 1024 * 1024
SEED = 20261021
TOP_N, EXTRA_N = 1000, 1000
OUR_TOKEN = "research-cache-probe"   # the name robots.txt rules can address us by

# Every AI fetcher name from internal/probe/bots.go, plus Google-Extended, which is a
# robots.txt-only token (it never appears in a User-Agent, but sites do write rules for it).
ROBOTS_NAMES = ["ChatGPT-User", "OAI-SearchBot", "GPTBot",
                "Claude-User", "Claude-SearchBot", "ClaudeBot",
                "GoogleAgent-URLContext", "Google-Extended", "Googlebot",
                "xAI-Bot", "Grok"]
# The fetcher that takes the page when a user's question names it, one per provider.
# xAI's fetcher name is undocumented, so it is judged by the rules for all bots ("*").
QUERY_FETCHERS = {"openai": "ChatGPT-User", "anthropic": "Claude-User",
                  "google": "GoogleAgent-URLContext", "xai": "*"}

# ---- pure logic (tested) --------------------------------------------------
def pick_sites(rows, top_n=TOP_N, extra_n=EXTRA_N, seed=SEED):
    """rows = [(origin, rank_group)]. CrUX ranks only in groups (1000, 5000, ...), so take the
    whole first group in file order, then a seeded random extra_n from the next group."""
    groups = sorted({g for _, g in rows})
    top = [r for r in rows if r[1] == groups[0]][:top_n]
    nxt = [r for r in rows if len(groups) > 1 and r[1] == groups[1]]
    return top + random.Random(seed).sample(nxt, min(extra_n, len(nxt)))

def name_tag(env):
    contact = (env.get("PROBE_CONTACT") or "").strip()
    if not contact:
        sys.exit("refusing to run: set PROBE_CONTACT to the contact address sites should see")
    return f"{OUR_TOKEN}/0.1 (academic study of AI-fetcher caching; contact: {contact})"

def failure_type(exc):
    e = exc.reason if isinstance(exc, urllib.error.URLError) and not isinstance(exc, urllib.error.HTTPError) else exc
    if isinstance(e, socket.gaierror): return "dns"
    if isinstance(e, (TimeoutError, socket.timeout)): return "timeout"
    if isinstance(e, ConnectionRefusedError): return "refused"
    if isinstance(e, http.client.RemoteDisconnected): return "closed"   # subclass of reset; test first
    if isinstance(e, ConnectionResetError): return "reset"
    if isinstance(e, ssl.SSLCertVerificationError): return "tls-cert"
    if isinstance(e, ssl.SSLError): return "tls"
    if isinstance(e, OSError) and e.errno in (101, 113): return "unreachable"
    if isinstance(e, str): return "other:" + e[:40]
    return "other:" + type(e).__name__

_SIGNS = [
    ("network-filter", re.compile(r"fortiguard|web page blocked|web filter violation|blocked by your (network|administrator)", re.I)),
    ("cloudflare", re.compile(r"attention required! \| cloudflare|just a moment\.\.\.|cf-chl|cf-browser-verification", re.I)),
    ("akamai", re.compile(r"access denied.{0,400}reference #", re.I | re.S)),
    ("captcha", re.compile(r"captcha", re.I)),
]
def block_sign(status, headers, body):
    h = {k.lower(): v for k, v in (headers or {}).items()}
    if h.get("cf-mitigated", "").lower() == "challenge": return "cloudflare"
    for label, rx in _SIGNS:
        if label == "captcha" and len(body) > 50_000: continue   # big real page mentioning captcha
        if rx.search(body[:200_000]): return label
    return None

class _Text(HTMLParser):
    SKIP = {"script", "style", "noscript", "template"}
    def __init__(self):
        super().__init__(convert_charrefs=True); self.depth = 0; self.out = []
    def handle_starttag(self, tag, attrs):
        if tag in self.SKIP: self.depth += 1
    def handle_endtag(self, tag):
        if tag in self.SKIP and self.depth: self.depth -= 1
    def handle_data(self, data):
        if not self.depth: self.out.append(data)

def visible_text(html):
    p = _Text()
    try: p.feed(html); p.close()
    except Exception: pass
    return re.sub(r"\s+", " ", " ".join(p.out)).strip()

_APP = re.compile(r'<div[^>]+id=["\'](root|app|__next|__nuxt)["\']', re.I)
_NEEDJS = re.compile(r"<noscript[^>]*>[^<]{0,300}(enable|need|requires?)\s+javascript", re.I)
def is_shell_page(html, text_chars):
    """Little visible text AND a sign the page is built by scripts in the browser."""
    return text_chars < 1000 and bool(_APP.search(html) or _NEEDJS.search(html))

def parse_robots(txt):
    """-> {agent_lower: [(allow, pattern)]}. RFC 9309: consecutive user-agent lines open one
    group; rules for the same agent across groups merge; other lines are ignored."""
    groups, cur, in_rules = {}, [], False
    for line in txt.splitlines():
        line = line.split("#", 1)[0].strip()
        if ":" not in line: continue
        k, v = (s.strip() for s in line.split(":", 1)); k = k.lower()
        if k == "user-agent":
            if in_rules: cur, in_rules = [], False
            cur.append(v.lower()); groups.setdefault(v.lower(), [])
        elif k in ("allow", "disallow") and cur:
            in_rules = True
            if v == "" : continue                       # empty Disallow = no rule
            for a in cur: groups[a].append((k == "allow", v))
    return groups

def _rule_matches(pattern, path):
    anchored = pattern.endswith("$")
    rx = "".join(".*" if c == "*" else re.escape(c) for c in pattern.rstrip("$"))
    return re.match(rx + ("$" if anchored else ""), path) is not None

def robots_allows(groups, agent, path):
    rules = groups.get(agent.lower())
    if rules is None: rules = groups.get("*")
    if not rules: return True
    best = None   # (length, allow)
    for allow, pat in rules:
        if _rule_matches(pat, path):
            cand = (len(pat), allow)
            if best is None or cand > best: best = cand   # longer wins; Allow wins a tie
    return True if best is None else best[1]

def robots_unavailable_verdict(status):
    """RFC 9309 2.3.1: 4xx means no rules (allowed); 5xx or no answer means disallow all."""
    return status is not None and 400 <= status < 500

def we_may_fetch_homepage(robots_status, robots_text):
    """Our own scanner obeys robots.txt for "/": a group naming OUR_TOKEN wins, else "*".
    -> (allowed, skip_reason)."""
    if robots_status == 200:
        ok = robots_allows(parse_robots(robots_text), OUR_TOKEN, "/")
        return (True, None) if ok else (False, "robots")
    if robots_unavailable_verdict(robots_status):
        return True, None
    return False, "robots-unavailable"

def funnel(recs):
    let_in = [r for r in recs if not r.get("skipped")]
    a = [r for r in let_in if r.get("status") is not None]
    ok = [r for r in a if r.get("status") == 200]
    rb = [r for r in ok if r.get("robots_query_ok")]
    real = [r for r in rb if not r.get("shell_page")]
    return [("listed", len(recs)), ("robots.txt lets our scanner in", len(let_in)),
            ("answered", len(a)), ("200 OK", len(ok)),
            ("robots allows all 4 providers", len(rb)), ("not a script-only shell", len(real))]

# ---- I/O ------------------------------------------------------------------
CTX = ssl.create_default_context()

def _get(url, ua):
    req = urllib.request.Request(url, headers={"User-Agent": ua, "Accept": "*/*",
                                               "Accept-Encoding": "gzip"})
    try:
        r = urllib.request.urlopen(req, timeout=TIMEOUT, context=CTX)
    except urllib.error.HTTPError as e:
        r = e
    raw = r.read(MAX_BODY + 1)
    body = gzip.decompress(raw) if (r.headers.get("Content-Encoding") or "").lower() == "gzip" else raw
    return r.status if hasattr(r, "status") else r.code, r.geturl(), r.headers, raw, body

def _decode(body, headers):
    m = re.search(r"charset=([\w-]+)", headers.get("Content-Type") or "", re.I)
    try: return body.decode(m.group(1) if m else "utf-8", "replace")
    except LookupError: return body.decode("utf-8", "replace")

def _resolve(host):
    try:
        ips = sorted({ai[4][0] for ai in socket.getaddrinfo(host, 443, proto=socket.IPPROTO_TCP)})
        return ips, any(ipaddress.ip_address(i).is_private or ipaddress.ip_address(i).is_loopback for i in ips)
    except Exception:
        return [], False

def scan(origin, group, ua, get=_get, resolve=_resolve):
    rec = {"origin": origin, "rank_group": group, "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
    host = urllib.parse.urlsplit(origin).hostname
    rec["ips"], rec["ip_private"] = resolve(host)
    # robots.txt
    robots_text = ""
    try:
        st, _, h, _, b = get(origin.rstrip("/") + "/robots.txt", ua)
        rec["robots_status"] = st
        if st == 200:
            robots_text = _decode(b, h)
            g = parse_robots(robots_text)
            verdict = lambda n: robots_allows(g, n, "/")
        else:
            v = robots_unavailable_verdict(st); verdict = lambda n: v
    except Exception as e:
        rec["robots_status"] = None; rec["robots_error"] = failure_type(e)
        verdict = lambda n: False
    rec["robots"] = {n: verdict(n) for n in ROBOTS_NAMES + ["*"]}
    rec["robots_query_ok"] = all(rec["robots"][n] for n in QUERY_FETCHERS.values())
    allowed, why = we_may_fetch_homepage(rec["robots_status"], robots_text)
    if not allowed:
        rec["status"] = None; rec["skipped"] = why
        return rec
    # homepage
    t0 = time.time()
    try:
        st, final, h, raw, body = get(origin, ua)
    except Exception as e:
        rec["status"] = None; rec["error"] = failure_type(e); rec["elapsed_s"] = round(time.time() - t0, 2)
        return rec
    html = _decode(body, h); text = visible_text(html)
    cc_raw = h.get("Cache-Control"); cc = parse_cache_control(cc_raw)
    rec.update({
        "status": st, "final_url": final, "elapsed_s": round(time.time() - t0, 2),
        "content_type": h.get("Content-Type"),
        "cache_control": cc_raw, "stance": classify(cc), "asks_fresh": asks_fresh(cc),
        "expires": h.get("Expires"), "has_etag": h.get("ETag") is not None,
        "has_last_modified": h.get("Last-Modified") is not None,
        "age": h.get("Age"), "cf_cache_status": h.get("CF-Cache-Status"), "server": h.get("Server"),
        "bytes_sent": len(raw), "bytes_unzipped": len(body), "truncated": len(raw) > MAX_BODY,
        "text_chars": len(text), "body_sha256": hashlib.sha256(body).hexdigest(),
        "shell_page": is_shell_page(html, len(text)),
        "block_sign": block_sign(st, dict(h.items()), html),
    })
    return rec

def main():
    if len(sys.argv) < 3: sys.exit(__doc__)
    ua = name_tag(os.environ)
    src, outdir = sys.argv[1], sys.argv[2]
    workers = int(sys.argv[3]) if len(sys.argv) > 3 else 4
    with gzip.open(src, "rt") as f:
        rows = [(r["origin"], int(r["rank"])) for r in csv.DictReader(f)]
    sites = pick_sites(rows)
    os.makedirs(outdir, exist_ok=True)
    stamp = time.strftime("%Y%m%d-%H%M%S", time.gmtime())
    with open(os.path.join(outdir, f"sites-seed{SEED}.txt"), "w") as fs:
        fs.writelines(f"{o},{g}\n" for o, g in sites)
    out = os.path.join(outdir, f"scan-{stamp}.jsonl")
    if os.path.exists(out): sys.exit(f"refusing to overwrite {out}")
    recs, t0 = [], time.time()
    with open(out, "x") as fo, ThreadPoolExecutor(max_workers=workers) as ex:
        for f in as_completed({ex.submit(scan, o, g, ua): o for o, g in sites}):
            r = f.result(); recs.append(r); fo.write(json.dumps(r) + "\n"); fo.flush()
            if len(recs) % 100 == 0:
                print(f"  {len(recs)}/{len(sites)}  {len(recs)/(time.time()-t0):.1f} sites/s", flush=True)
    print(f"\nwrote {out}")
    for label, n in funnel(recs): print(f"  {label:32s} {n}")
    from collections import Counter
    print("  failure types:", dict(Counter(r["error"] for r in recs if r.get("error"))))
    print("  skipped by robots.txt:", dict(Counter(r["skipped"] for r in recs if r.get("skipped"))))
    print("  block-page signs:", dict(Counter(r["block_sign"] for r in recs if r.get("block_sign"))))

if __name__ == "__main__": main()
