#!/usr/bin/env python3
"""3-day public-endpoint staleness + cache-lifetime run (aligned 45-min sampling, all vendors).

Every (vendor, anchor) is sampled on the SAME 45-min, 24/7 schedule, so vendors are comparable.
- OpenAI, xAI, Google: 9 anchors.  Anthropic: 4 (one per Cache-Control stance).
- Anthropic uses the BASIC web_fetch (web_fetch_20250910, dynamic filtering OFF): cheap (~1-2c/call,
  no 20x bloat) AND its reply carries the fetched text + retrieved_at, so Anthropic self-dates
  (no poller needed for it). The other three are dated against the 1-min/5-min ground-truth poll.
- Safety: a running count of Anthropic's estimated spend stops Anthropic calls at --anthropic-cap
  (default $30); the other vendors keep going. Append-only log; one JSON line per call.

This script does NOT start the poller (start publicPageEndpoint_poll.py next to it) and makes NO call under --dry.
Keys from the environment: ANTHROPIC_API_KEY, OPENROUTER_API_KEY, GEMINI_API_KEY.
Usage: publicPageEndpoint_run.py <out.jsonl> [--rounds 96] [--interval 2700] [--jitter 300]
                          [--anthropic-cap 30] [--max-content-tokens 8000] [--label lifetime] [--dry]
"""
import sys, os, json, time, random, argparse, urllib.error
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))   # so `import publicPage_run` works from anywhere
import publicPage_run as ap
from publicPageEndpoint_config import ANCHORS_BY_KEY

# 9 anchors for the live-fetch-dated vendors; 4 (one per Cache-Control stance) for Anthropic.
NINE = ["hn_maxitem", "nist_beacon", "drand", "osm_minute", "stackoverflow",
        "crates_io", "eth_height", "btc_tip", "usgs_quakes"]
FOUR = ["hn_maxitem", "nist_beacon", "usgs_quakes", "eth_height"]    # no-cache / no-store / max-age / none
VENDOR_ANCHORS = {"openai": NINE, "xai": NINE, "google": NINE, "anthropic": FOUR}
VENDOR_ORDER = ["openai", "xai", "google", "anthropic"]             # ask all a page's vendors back-to-back

# $/token for the running cost counter (conservative; direct Anthropic billed at Anthropic list $3/$15 per M).
RATE = {"anthropic": (3e-6, 15e-6), "openai": (0.25e-6, 2e-6), "xai": (3e-6, 9e-6), "google": (0.0, 0.0)}
PERCALL_FEE = {"openai": 0.01, "xai": 0.006, "anthropic": 0.0, "google": 0.0}   # search/tool fee per call

def anth_tool(max_content_tokens):
    # basic web_fetch: filtering OFF (cheap), still returns web_fetch_tool_result with retrieved_at + text
    return {"type": "web_fetch_20250910", "name": "web_fetch", "max_uses": 1,
            "max_content_tokens": max_content_tokens}

def call(vendor, anchor, max_content_tokens):
    """One vendor call for one anchor. Returns (answer, usage, meta)."""
    meta = {}
    u, q = anchor["url"], anchor["question"]
    if vendor == "anthropic":
        ans, usage = ap.ask_anthropic(u, q, tool=anth_tool(max_content_tokens),
                                      post_kw={"retry_on_timeout": False}, meta=meta)
    elif vendor == "openai":
        ans, usage = ap.ask_openrouter("openai/gpt-5-mini", u, q, meta=meta)
    elif vendor == "xai":
        ans, usage = ap.ask_openrouter("x-ai/grok-4.3", u, q, meta=meta)
    elif vendor == "google":
        ans, usage = ap.ask_gemini(u, q, meta=meta)
    else:
        raise ValueError(vendor)
    return ans, usage, meta

def est_cost(vendor, usage):
    ri, ro = RATE[vendor]
    toks_in = usage.get("input_tokens", 0) + usage.get("tool_use_input_tokens", 0)
    return PERCALL_FEE.get(vendor, 0.0) + toks_in * ri + usage.get("output_tokens", 0) * ro

def retrieved_at_of(meta):
    for t in meta.get("fetched_texts") or []:
        if t.get("retrieved_at"): return t["retrieved_at"]
    for s in meta.get("fetch_status") or []:
        if s.get("retrieved_at"): return s["retrieved_at"]
    return None

def run(out_path, rounds, interval, jitter, cap, mct, label, dry,
        vendors=None, vendor_anchors=None, anchor_order=None):
    vendors = vendors or VENDOR_ORDER            # subset override for follow-up runs (e.g. Anthropic-only)
    vendor_anchors = vendor_anchors or VENDOR_ANCHORS
    anchor_order = anchor_order or NINE
    anth_spent, anth_stopped = 0.0, False
    with open(out_path, "a") as fo:
        for rnd in range(rounds):
            for akey in anchor_order:
                a = ANCHORS_BY_KEY[akey]
                for v in vendors:
                    if akey not in vendor_anchors.get(v, []): continue
                    if v == "anthropic" and anth_stopped: continue
                    rec = {"ts": ap.now(), "round": rnd, "anchor": akey, "vendor": v,
                           "model": ap.VENDOR_MODEL[v], "url": a["url"], "label": label}
                    t0 = time.time()
                    try:
                        if dry:
                            ans, usage, meta = _stub(v, a)
                        else:
                            ans, usage, meta = call(v, a, mct)
                        c = est_cost(v, usage)
                        rec.update(answer=ans, usage=usage, ok=True, latency_s=round(time.time() - t0, 1),
                                   est_cost_usd=round(c, 5), fetch_status=meta.get("fetch_status"),
                                   stop_reason=meta.get("stop_reason"), retrieved_at=retrieved_at_of(meta))
                        if v == "anthropic":
                            rec["fetched_texts"] = meta.get("fetched_texts")   # copy text + retrieved_at
                            anth_spent += c
                    except urllib.error.HTTPError as e:
                        rec.update(ok=False, err="HTTP %s" % e.code, body=e.read().decode("utf-8", "replace")[:400])
                    except Exception as e:
                        rec.update(ok=False, err="%s: %s" % (type(e).__name__, e))
                    fo.write(json.dumps(rec, ensure_ascii=False) + "\n"); fo.flush()
                    if v == "anthropic" and anth_spent >= cap and not anth_stopped:
                        anth_stopped = True
                        fo.write(json.dumps({"ts": ap.now(), "event": "anthropic_cap_hit",
                                             "spent": round(anth_spent, 2), "cap": cap}) + "\n"); fo.flush()
                        print("[%s] ANTHROPIC CAP $%.0f hit (spent $%.2f) — Anthropic stopped, others continue"
                              % (ap.now(), cap, anth_spent), flush=True)
            print("[%s] round %d/%d done; anthropic spent ~$%.2f" % (ap.now(), rnd + 1, rounds, anth_spent), flush=True)
            if rnd + 1 < rounds:
                time.sleep(max(0, interval + random.randint(-jitter, jitter)))
    return anth_spent

def _stub(vendor, anchor):
    """--dry: a fake answer so the loop/record/schedule can be validated with no API call or key."""
    v = 1000000 + random.randint(0, 999)
    usage = {"input_tokens": 1200, "output_tokens": 30}
    meta = {"fetch_status": [{"url": anchor["url"], "status": "ok"}], "stop_reason": "end_turn"}
    if vendor == "anthropic":
        meta["fetched_texts"] = [{"url": anchor["url"], "retrieved_at": ap.now(), "text": "stub %d" % v}]
    return str(v), usage, meta

def main():
    ap_ = argparse.ArgumentParser()
    ap_.add_argument("out")
    ap_.add_argument("--rounds", type=int, default=96)          # 45-min x 96 ~= 3 days
    ap_.add_argument("--interval", type=int, default=2700)      # 45 min
    ap_.add_argument("--jitter", type=int, default=300)         # +/- 5 min, so probes don't phase-lock
    ap_.add_argument("--anthropic-cap", type=float, default=30.0)
    ap_.add_argument("--max-content-tokens", type=int, default=8000)
    ap_.add_argument("--label", default="lifetime")
    ap_.add_argument("--vendors", default="")    # CSV subset (e.g. "anthropic"); empty = all four
    ap_.add_argument("--anchors", default="")     # CSV; empty = default per-vendor mapping. Applies to the selected vendors.
    ap_.add_argument("--dry", action="store_true")
    a = ap_.parse_args()
    vendors = a.vendors.split(",") if a.vendors else None
    if a.anchors:
        order = a.anchors.split(",")
        vanch = {v: order for v in (vendors or VENDOR_ORDER)}
    else:
        order, vanch = None, None
    print("[%s] start: rounds=%d interval=%ds jitter=%ds cap=$%.0f mct=%d vendors=%s anchors=%s dry=%s -> %s"
          % (ap.now(), a.rounds, a.interval, a.jitter, a.anthropic_cap, a.max_content_tokens,
             a.vendors or "all", a.anchors or "default", a.dry, a.out), flush=True)
    spent = run(a.out, a.rounds, a.interval, a.jitter, a.anthropic_cap, a.max_content_tokens, a.label, a.dry,
                vendors=vendors, vendor_anchors=vanch, anchor_order=order)
    print("[%s] done; anthropic estimated spend ~$%.2f" % (ap.now(), spent), flush=True)

if __name__ == "__main__": main()
