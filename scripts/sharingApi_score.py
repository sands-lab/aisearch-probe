#!/usr/bin/env python3
"""Grade a sharing round from the FULL server log, after the round has finished.

Why: origin B's log on this machine (logs/probe-vps.jsonl) is a copy that can lag
the VPS by 15 s or more (bulletin 2026-10-28). The runner reads the log ~15 s after
each answer, so on origin B a real download can be missing at that moment — at
step 2 that reads as a false "shared". This script judges every step once, at the
end, after proving the copy has caught up.

How:
  1. Fence: send one request of our own to the origin (a page id nobody else uses,
     our liveness user-agent, so it never counts as a download) and wait until it
     appears in the log copy. Once it is there, every earlier request is too.
  2. For each step the runner recorded (logs/sharing-redo/<label>-<comparison>.jsonl):
     downloaded = any request for that page, ANY size of it, not our own probes,
     between the step being sent and SENT_WINDOW seconds later — the runner's rule.
  3. Verdict per comparison with the runner's rules (trial/sharing.go reportSharing).
     The code-in-answer and "already in an earlier answer" checks come from the
     runner's records (they depend on the answers, not the log).

Usage:
  scripts/sharingApi_score.py LABEL ORIGIN PROBE_LOG [--no-fence]
  e.g. scripts/sharingApi_score.py ob1 https://agentfetchlab.com logs/probe-vps.jsonl
Writes logs/sharing-redo/<label>-graded.json and prints a table.
"""
import glob
import json
import os
import sys
import time
import urllib.request

SENT_WINDOW = 150.0          # seconds after a step counts as caused by it (as the runner)
FENCE_TIMEOUT = 300.0        # give up waiting for the copy after this long
FENCE_UA = "probe-liveness/1 (+probe-trial sharing-grade fence)"
OUT_DIR = "logs/sharing-redo"


def page_dir(url):
    """/depthhtmlpage/<id>/2048 -> /depthhtmlpage/<id> (every size of the page)."""
    path = url.split("://", 1)[-1]
    path = path[path.find("/"):] if "/" in path else "/"
    path = path.split("?", 1)[0]
    return path.rsplit("/", 1)[0] if path.count("/") > 1 else path


def same_page(req_path, url):
    d = page_dir(url)
    return req_path == d or req_path.startswith(d + "/")


def load_log(path):
    recs = []
    with open(path) as f:
        for line in f:
            try:
                r = json.loads(line)
            except ValueError:
                continue
            recs.append(r)
    return recs


def is_ours(r):
    ua = r.get("user_agent") or ""
    bot = r.get("bot") or ""
    return ua.startswith("probe-") or bot.startswith("probe-")


def downloads(recs, url, sent):
    host = url.split("://", 1)[-1].split("/", 1)[0]
    out = []
    for r in recs:
        if is_ours(r):
            continue
        rurl = r.get("url") or ""
        if host and host not in rurl:
            continue
        t = r.get("ts_unix") or 0
        if sent <= t <= sent + SENT_WINDOW and same_page(r.get("page_key") or "", url):
            out.append(r)
    return out


def verdict(steps):
    """steps: list of dicts with fetched/marker/leaked/error, in step order 1..4."""
    if len(steps) < 3:
        return "INCOMPLETE", ""
    s1, s2, s3 = steps[0], steps[1], steps[2]
    if s1.get("error") or s2.get("error") or s3.get("error"):
        return "VOID", "a call failed"
    if not s1["fetched"]:
        return "VOID", "first caller never downloaded"
    if not s3["fetched"]:
        return "VOID", "control failed"
    if s2["fetched"]:
        return "NOT SHARED", ""
    if not s2.get("marker"):
        return "VOID", "no download and no page code"
    if s2.get("leaked"):
        return "CAN'T TELL", "code already in an earlier answer"
    late = ""
    if len(steps) >= 4:
        s4 = steps[3]
        if s4.get("leaked"):
            late = "late step unreadable"
        elif not s4["fetched"] and s4.get("marker"):
            late = "still shared at the late step"
        elif s4["fetched"]:
            late = "re-downloaded by the late step"
        else:
            late = "late step unreadable"
    return "SHARED", late


def fence(origin, log_path, label):
    """Send one request of our own and wait until the log copy shows it."""
    fid = f"fence-{label}-{int(time.time())}"
    url = f"{origin.rstrip('/')}/depthhtmlpage/{fid}/2048"
    req = urllib.request.Request(url, headers={"User-Agent": FENCE_UA})
    try:
        urllib.request.urlopen(req, timeout=30).read()
    except Exception as e:  # a 4xx/5xx still reaches the log; only a dead origin matters
        if not hasattr(e, "code"):
            raise SystemExit(f"fence request failed: {e}")
    deadline = time.time() + FENCE_TIMEOUT
    while time.time() < deadline:
        with open(log_path) as f:
            if fid in f.read():
                return fid
        time.sleep(5)
    raise SystemExit(f"log copy {log_path} did not show the fence {fid} within "
                     f"{int(FENCE_TIMEOUT)} s — NOT grading (copy may be stalled)")


def main(argv):
    if len(argv) < 4:
        print(__doc__)
        return 2
    label, origin, log_path = argv[1], argv[2], argv[3]
    if "--no-fence" not in argv:
        fid = fence(origin, log_path, label)
        print(f"fence {fid} seen in {log_path} — the copy has caught up")
    recs = load_log(log_path)
    rows = {}
    for f in sorted(glob.glob(os.path.join(OUT_DIR, f"{label}-*.jsonl"))):
        name = os.path.basename(f)[len(label) + 1:-len(".jsonl")]
        if name == "graded":
            continue
        steps = [json.loads(l) for l in open(f) if l.strip()]
        steps.sort(key=lambda s: s["step"])
        for s in steps:
            hits = downloads(recs, s["url"], s["sent_unix"])
            s["fetched"] = bool(hits)
            s["regraded_fetches"] = [f'{h.get("ts","")[11:19]} {h.get("bot","")} {h.get("status_code","")}' for h in hits]
        v, why = verdict(steps)
        rows[name] = {"verdict": v, "note": why,
                      "steps": [{k: s.get(k) for k in ("step", "url", "sent_at", "marker", "leaked",
                                                       "error", "fetched", "regraded_fetches",
                                                       "fetch_status")}
                                for s in steps]}
        print(f"{name:20} {v:11} {why}")
        # The provider's own fetch report (Gemini's urlRetrievalStatus), when it gave one.
        for s in steps:
            if s.get("fetch_status"):
                print(f"{'':20}   step {s['step']}: provider says {s['fetch_status'].split()[0]}"
                      f", downloads seen {len(s['regraded_fetches'])}")
    out = os.path.join(OUT_DIR, f"{label}-graded.json")
    with open(out, "w") as f:
        json.dump(rows, f, indent=1)
    print(f"written {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
