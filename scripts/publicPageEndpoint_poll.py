#!/usr/bin/env python3
"""Ground-truth poller for the 3-day lifetime run: polls ONLY the 9 run anchors, every INTERVAL
seconds, self-terminates after duration_sec. Standalone (does not touch the shared anchor_poll.py).
Dates OpenAI/xAI/Google; Anthropic self-dates via retrieved_at and does not need this.
Usage: publicPageEndpoint_poll.py <out.jsonl> [interval_sec=300] [duration_sec=infinite]"""
import sys, os, ssl, socket, json, time, urllib.request
from datetime import datetime, timezone
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from publicPageEndpoint_config import ANCHORS

KEYS = {"hn_maxitem", "nist_beacon", "drand", "osm_minute", "stackoverflow",
        "crates_io", "eth_height", "btc_tip", "usgs_quakes"}
POLL = [a for a in ANCHORS if a["key"] in KEYS]

TIMEOUT = 20; socket.setdefaulttimeout(TIMEOUT)
CTX = ssl.create_default_context(); CTX.check_hostname = False; CTX.verify_mode = ssl.CERT_NONE
UA = "ai-crawler-cache-probe/1.0 (academic research; ground-truth poller; polite 5min cadence, 9 endpoints)"

def now(): return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

def fetch(a):
    url = a["url"]
    if a["key"] == "stackoverflow" and os.environ.get("STACKEXCHANGE_KEY"):
        url += "&key=" + os.environ["STACKEXCHANGE_KEY"]
    req = urllib.request.Request(url, headers={"User-Agent": UA, "Accept": "*/*", "Accept-Encoding": "identity"})
    with urllib.request.urlopen(req, timeout=TIMEOUT, context=CTX) as r:
        return a["extract"](r.read())

def main():
    out = sys.argv[1]
    interval = int(sys.argv[2]) if len(sys.argv) > 2 else 300
    duration = int(sys.argv[3]) if len(sys.argv) > 3 else None
    t0 = time.time(); n = 0
    with open(out, "a") as fo:
        while True:
            for a in POLL:
                if a["key"] == "stackoverflow" and not os.environ.get("STACKEXCHANGE_KEY") and n % 3 != 0:
                    continue
                rec = {"ts": now(), "key": a["key"]}
                try:
                    v = fetch(a); rec["value"] = v; rec["ok"] = v is not None
                except Exception as e:
                    rec["value"] = None; rec["ok"] = False; rec["err"] = type(e).__name__
                fo.write(json.dumps(rec) + "\n")
            fo.flush(); n += 1
            print("[%s] poll %d done (%d endpoints)" % (now(), n, len(POLL)), flush=True)
            if duration is not None and time.time() - t0 >= duration: break
            time.sleep(interval)
    print("lifetime poller stopped after %d rounds" % n, flush=True)

if __name__ == "__main__": main()
