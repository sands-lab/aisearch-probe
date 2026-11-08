#!/usr/bin/env python3
"""Vendor driver for the monotonic-anchor validation run. For each (anchor, vendor) it asks the
vendor to FETCH the anchor URL and return ONLY the value, logs the answer + usage. Routing matches
the project's proven config: Anthropic + Gemini DIRECT, OpenAI + xAI via OpenRouter.
Request shapes reproduced from the trial/ harness (see the harness-mapping recipe).

Keys from env (source ~/.config/probe-trial/env first): ANTHROPIC_API_KEY, OPENROUTER_API_KEY, GEMINI_API_KEY.
Usage: publicPage_run.py <out.jsonl> [--anchors k1,k2] [--vendors anthropic,openai,xai,google]
                       [--rounds N] [--interval SEC] [--jitter SEC] [--label TAG]
Smoke: publicPage_run.py /tmp/smoke.jsonl --anchors hn_maxitem,nist_beacon --rounds 1
"""
import sys, os, json, time, random, argparse, urllib.request, urllib.error
from datetime import datetime, timezone
from publicPageEndpoint_config import ANCHORS_BY_KEY, ANCHORS, prompt_for

TIMEOUT = 120
def now(): return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")

_TRANSIENT = {429, 500, 502, 503, 504}
def error_record(code, body):
    """The provider's own words on a failed call. Google, Anthropic and OpenRouter all reply
    {"error": {"message", "status"|"type"}}; anything else keeps the body only (first 2000 chars)."""
    msg = status = None
    try:
        err = json.loads(body).get("error") or {}
        msg, status = err.get("message"), err.get("status") or err.get("type")
    except (ValueError, AttributeError): pass
    return {"code": code, "message": msg, "status": status, "body": body[:2000]}

def _post(url, headers, body, retries=5, timeout=None, retry_on_timeout=True, attempts=None, waits=None, failures=None):
    """retries / timeout / retry_on_timeout / waits default to the original behaviour. A timed-out
    call can still be billed (Anthropic), so callers that care pass retry_on_timeout=False.
    `attempts`, if given, is a list each try's start time is appended to (for the record);
    `failures`, if given, gets one error_record (+ "at") per failed try, so a bare code is never all we keep;
    `waits`, if given, are the seconds to wait before retry 1, 2, ... (else 3, 6, 12, ... up to 40)."""
    data = json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, headers={**headers, "Content-Type": "application/json"})
    last = None
    wait = lambda a: waits[min(a, len(waits) - 1)] if waits else min(3 * 2 ** a, 40)
    for attempt in range(retries):
        if attempts is not None: attempts.append(time.time())
        try:
            with urllib.request.urlopen(req, timeout=timeout or TIMEOUT) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            if failures is not None:
                try: text = e.read().decode("utf-8", "replace")
                except Exception: text = ""
                failures.append({**error_record(e.code, text), "at": now()})
            if e.code in _TRANSIENT and attempt < retries - 1:
                last = e; time.sleep(wait(attempt)); continue   # backoff on transient (busy/5xx)
            raise
        except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
            if failures is not None:
                failures.append({"code": None, "message": str(e), "status": type(e).__name__, "body": "", "at": now()})
            timed_out = isinstance(e, TimeoutError) or "timed out" in str(e)
            if attempt < retries - 1 and (retry_on_timeout or not timed_out):
                last = e; time.sleep(wait(attempt)); continue
            raise
    raise last

# ---- each provider's OWN report of what happened to the fetch (tested) ----
# Record these on every call, next to the answer and tokens: without them a refusal like Gemini's
# bare "I cannot read the document" cannot be explained (see anchor_probe_test.py).
def gemini_fetch_status(r):
    cand = ((r or {}).get("candidates") or [{}])[0]
    return [{"url": m.get("retrievedUrl"), "status": m.get("urlRetrievalStatus")}
            for m in ((cand.get("urlContextMetadata") or {}).get("urlMetadata") or [])]

def anthropic_fetch_status(content):
    out = []
    for b in content or []:
        if b.get("type") != "web_fetch_tool_result": continue
        c = b.get("content") or {}
        # the real type is "web_fetch_tool_result_error" (2 Nov raw replies); also key on error_code itself
        if c.get("type") in ("web_fetch_tool_result_error", "web_fetch_tool_error") or c.get("error_code"):
            out.append({"error_code": c.get("error_code")})
        else: out.append({"url": c.get("url"), "retrieved_at": c.get("retrieved_at"),
                          "media_type": ((c.get("content") or {}).get("source") or {}).get("media_type")})
    return out

def openrouter_fetch_status(r):
    msg = (((r or {}).get("choices") or [{}])[0].get("message") or {})
    out = []
    for a in msg.get("annotations") or []:
        d = a.get(a.get("type"), {}) if isinstance(a.get(a.get("type")), dict) else {}
        # "content" = an excerpt "added by OpenRouter if available" (docs, 2026-10-28)
        out.append({"type": a.get("type"), "url": d.get("url"), "title": d.get("title"), "content": d.get("content")})
    return out

def anthropic_fetched_texts(content):
    """The full text Anthropic's web_fetch handed the model, with its retrieval time — the one
    provider whose response includes the fetched page (PROVIDERS.md, 2026-10-28)."""
    out = []
    for b in content or []:
        c = (b.get("content") or {}) if b.get("type") == "web_fetch_tool_result" else {}
        src = ((c.get("content") or {}).get("source") or {}) if c.get("type") == "web_fetch_result" else {}
        if src.get("type") == "text" and src.get("data") is not None:
            out.append({"url": c.get("url"), "retrieved_at": c.get("retrieved_at"), "text": src["data"]})
    return out

def pack_raw(obj):
    """Full raw provider response, gzip + base64, with a fingerprint — kept on every call."""
    import gzip, base64, hashlib
    b = json.dumps(obj, ensure_ascii=False, sort_keys=True).encode()
    return {"gz_b64": base64.b64encode(gzip.compress(b)).decode(), "sha256": hashlib.sha256(b).hexdigest(), "bytes": len(b)}

def unpack_raw(p):
    import gzip, base64
    return json.loads(gzip.decompress(base64.b64decode(p["gz_b64"])))

def stop_reason(kind, r):
    """Why the model stopped, as the provider reports it (an empty answer is unexplained without it:
    Gemini RECITATION / SAFETY, Anthropic end_turn / max_tokens, OpenRouter finish_reason)."""
    r = r or {}
    if kind == "google":
        block = (r.get("promptFeedback") or {}).get("blockReason")
        if block: return "blocked:" + block
        return ((r.get("candidates") or [{}])[0]).get("finishReason")
    if kind == "anthropic":
        return r.get("stop_reason")
    ch = (r.get("choices") or [{}])[0]
    fr, nat = ch.get("finish_reason"), ch.get("native_finish_reason")
    return f"{fr}/{nat}" if nat else fr

# ---- per-vendor callers (return (answer_text, usage_dict)) ----------------
# meta: optional dict; if given, filled with meta["fetch_status"] (the provider's own fetch report)
# and meta["stop_reason"] (why the model stopped).
def ask_anthropic(url, question, model="claude-sonnet-5", prompt=None, tool=None, post_kw=None, meta=None):   # sonnet-5 per policy (Haiku can't fetch on the default path)
    """prompt: raw prompt text instead of prompt_for(); tool: a replacement web_fetch tool dict
    (e.g. the basic web_fetch_20250910 with max_content_tokens); post_kw: passed to _post."""
    key = os.environ["ANTHROPIC_API_KEY"]
    hdr = {"x-api-key": key, "anthropic-version": "2023-06-01"}
    msgs = [{"role": "user", "content": prompt or prompt_for({"url": url, "question": question})}]
    usage = {"input_tokens": 0, "output_tokens": 0}
    status = []; raws = []; texts = []
    if meta is not None: meta.update(fetch_status=status, raw=raws, fetched_texts=texts)
    for _ in range(6):                       # web_fetch can pause_turn; resume until done
        body = {"model": model, "max_tokens": 1024, "messages": msgs,
                "tools": [tool or {"type": "web_fetch_20260209", "name": "web_fetch", "max_uses": 3,
                                   "allowed_callers": ["direct"]}],   # haiku needs web_fetch as a direct-caller tool
                "tool_choice": {"type": "any"}}
        r = _post("https://api.anthropic.com/v1/messages", hdr, body, **(post_kw or {}))
        u = r.get("usage", {}); usage["input_tokens"] += u.get("input_tokens", 0); usage["output_tokens"] += u.get("output_tokens", 0)
        status.extend(anthropic_fetch_status(r.get("content")))
        texts.extend(anthropic_fetched_texts(r.get("content"))); raws.append(r)
        if meta is not None: meta["stop_reason"] = stop_reason("anthropic", r)
        if r.get("stop_reason") == "pause_turn":
            msgs.append({"role": "assistant", "content": r.get("content", [])}); continue
        txt = "".join(b.get("text", "") for b in r.get("content", []) if b.get("type") == "text")
        return txt.strip(), usage
    return "", usage

def ask_openrouter(model, url, question, prompt=None, post_kw=None, meta=None):
    key = os.environ["OPENROUTER_API_KEY"]
    hdr = {"Authorization": f"Bearer {key}"}
    body = {"model": model,
            "messages": [{"role": "user", "content": prompt or prompt_for({"url": url, "question": question})}],
            "tools": [{"type": "openrouter:web_search", "parameters": {"engine": "native"}}],
            "tool_choice": "required"}
    r = _post("https://openrouter.ai/api/v1/chat/completions", hdr, body, **(post_kw or {}))
    if meta is not None: meta.update(fetch_status=openrouter_fetch_status(r), stop_reason=stop_reason("openrouter", r), raw=[r])
    txt = (r.get("choices", [{}])[0].get("message", {}) or {}).get("content", "") or ""
    u = r.get("usage", {})
    return txt.strip(), {"input_tokens": u.get("prompt_tokens", 0), "output_tokens": u.get("completion_tokens", 0)}

def ask_gemini(url, question, model="gemini-3.1-flash-lite", prompt=None, post_kw=None, meta=None):
    key = os.environ["GEMINI_API_KEY"]
    hdr = {"x-goog-api-key": key}
    body = {"contents": [{"role": "user", "parts": [{"text": prompt or prompt_for({"url": url, "question": question})}]}],
            "tools": [{"urlContext": {}}]}
    r = _post(f"https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent", hdr, body, **(post_kw or {}))
    if meta is not None: meta.update(fetch_status=gemini_fetch_status(r), stop_reason=stop_reason("google", r), raw=[r])
    cand = (r.get("candidates") or [{}])[0]
    txt = "".join(p.get("text", "") for p in (cand.get("content", {}).get("parts") or []))
    um = r.get("usageMetadata", {})
    # url_context's fetched content is billed as input but reported apart from the prompt, in
    # toolUsePromptTokenCount; thinking tokens are reported apart too. Keep them, plus the total.
    return txt.strip(), {"input_tokens": um.get("promptTokenCount", 0), "output_tokens": um.get("candidatesTokenCount", 0),
                         "tool_use_input_tokens": um.get("toolUsePromptTokenCount", 0),
                         "thinking_tokens": um.get("thoughtsTokenCount", 0), "total_tokens": um.get("totalTokenCount", 0)}

VENDORS = {
    "anthropic": lambda a: ask_anthropic(a["url"], a["question"]),
    "openai":    lambda a: ask_openrouter("openai/gpt-5-mini", a["url"], a["question"]),
    "xai":       lambda a: ask_openrouter("x-ai/grok-4.3", a["url"], a["question"]),
    "google":    lambda a: ask_gemini(a["url"], a["question"]),
}
VENDOR_MODEL = {"anthropic": "claude-sonnet-5", "openai": "openai/gpt-5-mini",
                "xai": "x-ai/grok-4.3", "google": "gemini-3.1-flash-lite"}

def probe(anchor, vendor, out_fo, label):
    rec = {"ts": now(), "anchor": anchor["key"], "vendor": vendor, "model": VENDOR_MODEL[vendor],
           "url": anchor["url"], "label": label}
    t0 = time.time()
    try:
        ans, usage = VENDORS[vendor](anchor)
        rec.update({"answer": ans, "usage": usage, "ok": True, "latency_s": round(time.time() - t0, 1)})
    except urllib.error.HTTPError as e:
        rec.update({"ok": False, "err": f"HTTP {e.code}", "body": e.read().decode("utf-8", "replace")[:400]})
    except Exception as e:
        rec.update({"ok": False, "err": f"{type(e).__name__}: {e}"})
    out_fo.write(json.dumps(rec, ensure_ascii=False) + "\n"); out_fo.flush()
    return rec

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("out")
    ap.add_argument("--anchors", default="")
    ap.add_argument("--vendors", default="anthropic,openai,xai,google")
    ap.add_argument("--rounds", type=int, default=1)
    ap.add_argument("--interval", type=int, default=0)   # seconds between rounds
    ap.add_argument("--jitter", type=int, default=0)     # +/- seconds random added to interval
    ap.add_argument("--label", default="run")
    args = ap.parse_args()
    anchors = [ANCHORS_BY_KEY[k] for k in args.anchors.split(",")] if args.anchors else ANCHORS
    vendors = args.vendors.split(",")
    with open(args.out, "a") as fo:
        for rnd in range(args.rounds):
            print(f"[{now()}] round {rnd+1}/{args.rounds}", flush=True)
            for a in anchors:
                for v in vendors:
                    r = probe(a, v, fo, args.label)
                    tag = "ok" if r["ok"] else r.get("err")
                    print(f"  {a['key']:14} {v:10} {tag}: {str(r.get('answer',''))[:40]}", flush=True)
            if rnd + 1 < args.rounds:
                time.sleep(max(0, args.interval + random.randint(-args.jitter, args.jitter)))

if __name__ == "__main__": main()
