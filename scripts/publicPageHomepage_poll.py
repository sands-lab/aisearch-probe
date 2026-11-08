#!/usr/bin/env python3
"""Change watch for the public-page validation (doc/public_page_validation_notes.md 4.5).
Downloads each site's homepage once every 10 minutes for 24 hours, to learn which pages change
often enough (within the 20-minute wait of the AI test) to be useful. No AI calls.

Modest by design:
- sites are split into day-groups (default 2) watched one after the other, so the server sends
  half as many requests per second; each site still gets one visit per cycle;
- inside a cycle every site has its own fixed slot, evenly spaced, so requests trickle out
  instead of bursting; a site still answering when its next slot comes is skipped that round;
- a 429 or 503 ("slow down") answer leaves that site alone for an hour;
- robots.txt is obeyed for our own name, re-checked every 6 hours; a site that bars us is dropped;
- one try per visit, 15 s limit; the honest name tag carries $PROBE_CONTACT (never in code);
- a file named STOP in the output folder ends the run cleanly.

Each distinct page text is stored once (gzip, per site) and every visit logs its fingerprint.
Pure logic is unit-tested in watch_pages_test.py.
Usage: PROBE_CONTACT=... publicPageHomepage_poll.py <sites.txt> <out-dir> [--groups 2] [--hours 24]
                                      [--cycle 600] [--first-group 0]
"""
import sys, os, json, time, gzip, hashlib, random, argparse, threading
from html.parser import HTMLParser
from concurrent.futures import ThreadPoolExecutor
from publicPageHomepage_select_sites import name_tag, _get, _decode, failure_type, block_sign, we_may_fetch_homepage, MAX_BODY

SEED = 20261022
ROBOTS_EVERY = 6 * 3600
BACKOFF = 3600

# ---- pure logic (tested) --------------------------------------------------
def split_groups(sites, k, seed=SEED):
    s = list(sites); random.Random(seed).shuffle(s)
    return [s[j::k] for j in range(k)]

def slots(n, cycle):
    return [i * cycle / n for i in range(n)]

class _Lines(HTMLParser):
    SKIP = {"script", "style", "noscript", "template"}
    BLOCK = {"title", "p", "div", "br", "li", "ul", "ol", "tr", "td", "th", "table", "section",
             "article", "header", "footer", "nav", "main", "aside", "h1", "h2", "h3", "h4", "h5",
             "h6", "blockquote", "pre", "dt", "dd", "figcaption", "form", "option", "label"}
    def __init__(self):
        super().__init__(convert_charrefs=True); self.depth = 0; self.buf = []
    def handle_starttag(self, tag, attrs):
        if tag in self.SKIP: self.depth += 1
        elif tag in self.BLOCK: self.buf.append("\n")
    def handle_endtag(self, tag):
        if tag in self.SKIP and self.depth: self.depth -= 1
        elif tag in self.BLOCK: self.buf.append("\n")
    def handle_data(self, data):
        if not self.depth: self.buf.append(data)

def visible_lines(html):
    p = _Lines()
    try: p.feed(html); p.close()
    except Exception: pass
    return [l for l in (" ".join(x.split()) for x in "".join(p.buf).split("\n")) if l]

def backoff_until(status, now):
    return now + BACKOFF if status in (429, 503) else None

def headers_record(items):
    """Every response header except Set-Cookie (identifiers the site hands us; not needed).
    Names lower-cased; a repeated header's values joined with ", ". Kept whole so any CDN's
    cache headers (CF-Cache-Status, X-Cache, Via, Age, X-Served-By, ...) can be read later."""
    out = {}
    for k, v in items:
        k = k.lower()
        if k == "set-cookie": continue
        out[k] = out[k] + ", " + v if k in out else v
    return out

def snapshot_name(origin, lines):
    site = hashlib.sha256(origin.encode()).hexdigest()[:16]
    text = hashlib.sha256("\n".join(lines).encode()).hexdigest()
    return f"{site}/{text}.txt.gz"

# ---- I/O ------------------------------------------------------------------
class Watch:
    def __init__(self, out, ua, log):
        self.out, self.ua, self.log = out, ua, log
        self.lock = threading.Lock(); self.inflight = set()
        self.paused = {}; self.robots = {}          # origin -> until ; origin -> (allowed, checked_at)

    def _robots_ok(self, o):
        allowed, at = self.robots.get(o, (None, 0))
        if time.time() - at < ROBOTS_EVERY: return allowed
        try:
            st, _, h, _, b = _get(o.rstrip("/") + "/robots.txt", self.ua)
            allowed, _ = we_may_fetch_homepage(st, _decode(b, h) if st == 200 else "")
        except Exception:
            allowed = False
        self.robots[o] = (allowed, time.time())
        return allowed

    def visit(self, o, group, cycle):
        rec = {"ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "origin": o,
               "group": group, "cycle": cycle}
        try:
            if not self._robots_ok(o):
                rec["skipped"] = "robots"; return self._write(rec)
            t0 = time.time()
            st, final, h, raw, body = _get(o, self.ua)
            html = _decode(body, h); lines = visible_lines(html)
            name = snapshot_name(o, lines); path = os.path.join(self.out, "snapshots", name)
            if not os.path.exists(path):
                os.makedirs(os.path.dirname(path), exist_ok=True)
                with gzip.open(path, "wt") as f: f.write("\n".join(lines))
            rec.update(status=st, final_url=final, elapsed_s=round(time.time() - t0, 2),
                       bytes_sent=len(raw), bytes_unzipped=len(body), truncated=len(raw) > MAX_BODY,
                       text_chars=len("\n".join(lines)), n_lines=len(lines),
                       text_sha=name.split("/")[1][:-7],
                       block_sign=block_sign(st, dict(h.items()), html),
                       headers=headers_record(h.items()))
            until = backoff_until(st, time.time())
            if until: self.paused[o] = until; rec["paused_until"] = until
        except Exception as e:
            rec["error"] = failure_type(e)
        finally:
            with self.lock: self.inflight.discard(o)
        self._write(rec)

    def _write(self, rec):
        with self.lock:
            self.log.write(json.dumps(rec) + "\n"); self.log.flush()

def main():
    ap = argparse.ArgumentParser(usage=__doc__)
    ap.add_argument("sites"); ap.add_argument("out")
    ap.add_argument("--groups", type=int, default=2); ap.add_argument("--hours", type=float, default=24)
    ap.add_argument("--cycle", type=float, default=600); ap.add_argument("--first-group", type=int, default=0)
    a = ap.parse_args()
    ua = name_tag(os.environ)
    sites = [l.strip().split(",")[0] for l in open(a.sites) if l.strip()]
    groups = split_groups(sites, a.groups)
    os.makedirs(a.out, exist_ok=True)
    stop = os.path.join(a.out, "STOP")
    stamp = time.strftime("%Y%m%d-%H%M%S", time.gmtime())
    with open(os.path.join(a.out, f"watch-{stamp}.jsonl"), "x") as log, ThreadPoolExecutor(max_workers=8) as ex:
        w = Watch(a.out, ua, log)
        for gi in range(a.first_group, a.groups):
            g = groups[gi]; offs = slots(len(g), a.cycle); ncycles = int(a.hours * 3600 / a.cycle)
            print(f"group {gi}: {len(g)} sites, {ncycles} cycles of {a.cycle:.0f}s, "
                  f"one request every {a.cycle/len(g):.2f}s", flush=True)
            t0 = time.time()
            for c in range(ncycles):
                for o, off in zip(g, offs):
                    target = t0 + c * a.cycle + off
                    delay = target - time.time()
                    if delay > 0: time.sleep(delay)
                    if os.path.exists(stop):
                        print("STOP file found; ending cleanly.", flush=True); return
                    with w.lock:
                        if o in w.inflight or w.paused.get(o, 0) > time.time(): continue
                        w.inflight.add(o)
                    ex.submit(w.visit, o, gi, c)
                print(f"  group {gi} cycle {c+1}/{ncycles} done", flush=True)
    print("DONE", flush=True)

if __name__ == "__main__": main()
