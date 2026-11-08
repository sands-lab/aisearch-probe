#!/usr/bin/env python3
"""Tests for publicPageEndpoint_run.py — no network, no keys. Monkeypatches the vendor callers and checks
the schedule, the per-vendor anchor split, the record shape (Anthropic self-dating), the cost
estimate, and the Anthropic spend cap. Run: python3 anchor_lifetime_test.py"""
import os, sys, json, tempfile
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import publicPage_run as ap
import publicPageEndpoint_run as L

calls = []   # (vendor, url, tool-or-None)

def fake_anthropic(url, question, model="claude-sonnet-5", prompt=None, tool=None, post_kw=None, meta=None):
    calls.append(("anthropic", url, tool))
    if meta is not None:
        meta.update(fetch_status=[{"url": url, "retrieved_at": "2026-11-02T10:00:00Z"}],
                    stop_reason="end_turn",
                    fetched_texts=[{"url": url, "retrieved_at": "2026-11-02T10:00:00Z", "text": "copy"}])
    return "111", {"input_tokens": 1500, "output_tokens": 40}

def fake_openrouter(model, url, question, prompt=None, post_kw=None, meta=None):
    calls.append((model, url, None))
    if meta is not None: meta.update(fetch_status=[{"type": "url_citation", "url": url}], stop_reason="stop")
    return "222", {"input_tokens": 5000, "output_tokens": 500}

def fake_gemini(url, question, model="gemini-3.1-flash-lite", prompt=None, post_kw=None, meta=None):
    calls.append(("google", url, None))
    if meta is not None: meta.update(fetch_status=[{"url": url, "status": "ok"}], stop_reason="STOP")
    return "333", {"input_tokens": 50, "output_tokens": 30, "tool_use_input_tokens": 400, "total_tokens": 480}

def read(p): return [json.loads(l) for l in open(p)]

def test_one_round_shape():
    calls.clear()
    ap.ask_anthropic, ap.ask_openrouter, ap.ask_gemini = fake_anthropic, fake_openrouter, fake_gemini
    with tempfile.NamedTemporaryFile("w", suffix=".jsonl", delete=False) as f: out = f.name
    L.run(out, rounds=1, interval=0, jitter=0, cap=30.0, mct=8000, label="t", dry=False)
    recs = [r for r in read(out) if "vendor" in r]
    # 9 anchors x (openai,xai,google) + 4 x anthropic = 27 + 4 = 31
    assert len(recs) == 31, len(recs)
    by = lambda v: [r for r in recs if r["vendor"] == v]
    assert len(by("openai")) == 9 and len(by("xai")) == 9 and len(by("google")) == 9, "9-anchor vendors"
    assert len(by("anthropic")) == 4, "anthropic 4 anchors"
    assert {r["anchor"] for r in by("anthropic")} == set(L.FOUR), "anthropic = the four stances"
    # Anthropic self-dates: retrieved_at + fetched_texts present
    for r in by("anthropic"):
        assert r["retrieved_at"] == "2026-11-02T10:00:00Z", r
        assert r["fetched_texts"] and r["fetched_texts"][0]["text"] == "copy", r
    # Anthropic got the BASIC fetch tool with a content cap
    atools = [t for (v, _, t) in calls if v == "anthropic"]
    assert atools and all(t["type"] == "web_fetch_20250910" and t["max_content_tokens"] == 8000 for t in atools), atools
    # cost: google free, paid vendors > 0
    assert all(r["est_cost_usd"] == 0.0 for r in by("google")), "google free"
    assert all(r["est_cost_usd"] > 0 for r in by("openai") + by("anthropic")), "paid vendors cost > 0"
    os.unlink(out)
    print("ok test_one_round_shape (31 records, anthropic self-dated, basic tool, google free)")

def test_anthropic_cap_stops_only_anthropic():
    calls.clear()
    ap.ask_anthropic, ap.ask_openrouter, ap.ask_gemini = fake_anthropic, fake_openrouter, fake_gemini
    with tempfile.NamedTemporaryFile("w", suffix=".jsonl", delete=False) as f: out = f.name
    # cap tiny -> trips inside round 1; 3 rounds so we can see others continue after
    L.run(out, rounds=3, interval=0, jitter=0, cap=0.01, mct=8000, label="t", dry=False)
    rows = read(out)
    assert any(r.get("event") == "anthropic_cap_hit" for r in rows), "cap event written"
    recs = [r for r in rows if "vendor" in r]
    anth = [r for r in recs if r["vendor"] == "anthropic"]
    assert len(anth) <= 4, "anthropic stopped after the first page or two, not all 12"
    # the cheap vendors ran all 3 rounds
    assert len([r for r in recs if r["vendor"] == "openai"]) == 27, "openai kept going 3x9"
    os.unlink(out)
    print("ok test_anthropic_cap_stops_only_anthropic (anthropic halted, others ran 3 rounds)")

def test_anthropic_only_custom_anchors():
    calls.clear()
    ap.ask_anthropic, ap.ask_openrouter, ap.ask_gemini = fake_anthropic, fake_openrouter, fake_gemini
    SIX = ["nist_beacon", "drand", "osm_minute", "stackoverflow", "crates_io", "btc_tip"]
    with tempfile.NamedTemporaryFile("w", suffix=".jsonl", delete=False) as f: out = f.name
    L.run(out, rounds=1, interval=0, jitter=0, cap=30.0, mct=8000, label="fill", dry=False,
          vendors=["anthropic"], vendor_anchors={"anthropic": SIX}, anchor_order=SIX)
    recs = [r for r in read(out) if "vendor" in r]
    assert len(recs) == 6, len(recs)                                  # anthropic only, 6 anchors
    assert all(r["vendor"] == "anthropic" for r in recs), "anthropic only"
    assert {r["anchor"] for r in recs} == set(SIX), "the six endpoints"
    assert all(r["retrieved_at"] and r["fetched_texts"] for r in recs), "self-dated"
    assert not any(v == "google" for v, _, _ in calls), "no other vendor called"
    os.unlink(out)
    print("ok test_anthropic_only_custom_anchors (6 anthropic records, self-dated, no other vendor)")

def test_dry_needs_no_keys():
    with tempfile.NamedTemporaryFile("w", suffix=".jsonl", delete=False) as f: out = f.name
    L.run(out, rounds=1, interval=0, jitter=0, cap=30.0, mct=8000, label="t", dry=True)
    recs = [r for r in read(out) if "vendor" in r]
    assert len(recs) == 31 and all(r["ok"] for r in recs), "dry produces 31 ok stub records"
    os.unlink(out)
    print("ok test_dry_needs_no_keys (31 stub records, no API/keys)")

if __name__ == "__main__":
    test_one_round_shape()
    test_anthropic_cap_stops_only_anthropic()
    test_anthropic_only_custom_anchors()
    test_dry_needs_no_keys()
    print("ALL PASS")
