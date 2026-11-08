#!/usr/bin/env python3
"""TEST-FIRST spec for the provider fetch-status extractors in publicPage_run.py.
Every AI-call runner must record each provider's OWN report of what happened to the fetch, not
just the answer and tokens (a Google refusal of every application/xml file once went unexplained
because urlRetrievalStatus was dropped). Run: python3 anchor_probe_test.py
"""
from publicPage_run import (gemini_fetch_status, anthropic_fetch_status, openrouter_fetch_status, stop_reason,
                          anthropic_fetched_texts, pack_raw, unpack_raw)

def test_gemini_url_retrieval_status():
    r = {"candidates": [{"content": {"parts": [{"text": "I cannot read the document."}]},
                         "urlContextMetadata": {"urlMetadata": [
                             {"retrievedUrl": "https://a.com/sitemap.xml", "urlRetrievalStatus": "URL_RETRIEVAL_STATUS_UNSAFE"},
                             {"retrievedUrl": "https://b.com/x", "urlRetrievalStatus": "URL_RETRIEVAL_STATUS_SUCCESS"}]}}]}
    assert gemini_fetch_status(r) == [{"url": "https://a.com/sitemap.xml", "status": "URL_RETRIEVAL_STATUS_UNSAFE"},
                                      {"url": "https://b.com/x", "status": "URL_RETRIEVAL_STATUS_SUCCESS"}]
    # no metadata at all is itself worth recording: the tool may never have been used
    assert gemini_fetch_status({"candidates": [{"content": {}}]}) == []
    assert gemini_fetch_status({}) == []

def test_anthropic_web_fetch_results_and_errors():
    content = [
        {"type": "server_tool_use", "name": "web_fetch", "input": {"url": "https://a.com/s.xml"}},
        {"type": "web_fetch_tool_result", "content": {"type": "web_fetch_tool_error", "error_code": "url_not_allowed"}},
        {"type": "web_fetch_tool_result", "content": {"type": "web_fetch_result", "url": "https://b.com/s.xml",
                                                      "retrieved_at": "2026-10-25T01:00:00Z",
                                                      "content": {"type": "document", "source": {"media_type": "text/plain"}}}},
        {"type": "text", "text": "..."}]
    assert anthropic_fetch_status(content) == [
        {"error_code": "url_not_allowed"},
        {"url": "https://b.com/s.xml", "retrieved_at": "2026-10-25T01:00:00Z", "media_type": "text/plain"}]
    assert anthropic_fetch_status([{"type": "text", "text": "x"}]) == []

def test_anthropic_fetch_error_in_the_shape_anthropic_really_sends():
    # 2 Nov 2026 raw replies: the error type is "web_fetch_tool_result_error"; the guessed name above
    # never matched, so 60 error codes were saved as an empty entry (recovered from the raw replies)
    real = [{"type": "web_fetch_tool_result",
             "content": {"type": "web_fetch_tool_result_error", "error_code": "url_not_accessible"}}]
    assert anthropic_fetch_status(real) == [{"error_code": "url_not_accessible"}]
    assert anthropic_fetched_texts(real) == []

def test_openrouter_annotations_and_tool_errors():
    r = {"choices": [{"message": {"content": "I cannot read the document.",
                                  "annotations": [{"type": "url_citation", "url_citation": {"url": "https://a.com/s.xml", "title": "A"}}]}}]}
    assert openrouter_fetch_status(r) == [{"type": "url_citation", "url": "https://a.com/s.xml", "title": "A", "content": None}]
    assert openrouter_fetch_status({"choices": [{"message": {"content": "x"}}]}) == []

def test_stop_reasons_are_recorded():
    # why the model stopped: an empty Gemini answer with 0 output tokens (2026-10-28, hvg.hu)
    # could be RECITATION or SAFETY — without this field it cannot be told apart
    assert stop_reason("google", {"candidates": [{"finishReason": "RECITATION"}]}) == "RECITATION"
    assert stop_reason("google", {"promptFeedback": {"blockReason": "SAFETY"}, "candidates": []}) == "blocked:SAFETY"
    assert stop_reason("anthropic", {"stop_reason": "end_turn"}) == "end_turn"
    assert stop_reason("openrouter", {"choices": [{"finish_reason": "stop", "native_finish_reason": "completed"}]}) == "stop/completed"
    assert stop_reason("google", {}) is None

def test_anthropic_fetched_text_is_kept():
    # Anthropic returns the full text its model was given; keep it (with when it was retrieved)
    content = [{"type": "web_fetch_tool_result", "content": {
        "type": "web_fetch_result", "url": "https://a.com/?x=1", "retrieved_at": "2026-10-28T13:04:49+00:00",
        "content": {"type": "document", "source": {"type": "text", "media_type": "text/plain",
                                                   "data": "Tovább a friss hírekhez\nA nagyi almája"}}}}]
    assert anthropic_fetched_texts(content) == [{"url": "https://a.com/?x=1", "retrieved_at": "2026-10-28T13:04:49+00:00",
                                                 "text": "Tovább a friss hírekhez\nA nagyi almája"}]
    assert anthropic_fetched_texts([{"type": "web_fetch_tool_result",
                                     "content": {"type": "web_fetch_tool_error", "error_code": "url_not_allowed"}}]) == []

def test_openrouter_citation_content_is_kept():
    r = {"choices": [{"message": {"content": "x", "annotations": [
        {"type": "url_citation", "url_citation": {"url": "https://a.com/", "title": "A", "content": "excerpt here"}}]}}]}
    assert openrouter_fetch_status(r) == [{"type": "url_citation", "url": "https://a.com/", "title": "A",
                                           "content": "excerpt here"}]

def test_raw_response_packing_round_trips():
    raw = {"candidates": [{"finishReason": "STOP"}], "big": "x" * 5000}
    packed = pack_raw(raw)
    assert set(packed) == {"gz_b64", "sha256", "bytes"} and unpack_raw(packed) == raw

def test_every_failed_try_keeps_the_providers_own_message():
    # a bare "HTTP 503" cannot tell "model overloaded" from "quota used up" (2026-10-28: all 4 Google calls)
    from publicPage_run import error_record
    g = '{"error": {"code": 503, "message": "The model is overloaded. Please try again later.", "status": "UNAVAILABLE"}}'
    assert error_record(503, g) == {"code": 503, "message": "The model is overloaded. Please try again later.",
                                    "status": "UNAVAILABLE", "body": g}
    a = '{"type": "error", "error": {"type": "overloaded_error", "message": "Overloaded"}}'
    assert error_record(529, a)["message"] == "Overloaded" and error_record(529, a)["status"] == "overloaded_error"
    assert error_record(502, "<html>Bad gateway</html>")["message"] is None   # not JSON: the body is still kept
    assert len(error_record(500, "x" * 9000)["body"]) == 2000

def test_post_records_each_failed_try():
    import io, urllib.error, publicPage_run
    body = b'{"error": {"code": 503, "message": "The model is overloaded.", "status": "UNAVAILABLE"}}'
    calls = []
    def fake(req, timeout=None):
        calls.append(1)
        if len(calls) < 3: raise urllib.error.HTTPError("u", 503, "busy", {}, io.BytesIO(body))
        return io.BytesIO(b'{"ok": true}')
    real = publicPage_run.urllib.request.urlopen; publicPage_run.urllib.request.urlopen = fake
    try:
        failures = []
        assert publicPage_run._post("https://x", {}, {}, retries=4, waits=[0], failures=failures) == {"ok": True}
    finally: publicPage_run.urllib.request.urlopen = real
    assert [f["message"] for f in failures] == ["The model is overloaded."] * 2 and all("at" in f for f in failures)

def run():
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn(); print("PASS", name)
    print("ALL PASS")

if __name__ == "__main__": run()
