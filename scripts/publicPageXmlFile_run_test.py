#!/usr/bin/env python3
"""TEST-FIRST spec for publicPageXmlFile_run.py (public XML file-type test). Run: python3 -m pytest publicPageXmlFile_run_test.py
Covers picking the test files, the question, lane timing (offsets, 3-minute spacing, Google's pause
window), and scoring an answer against the answer key.
"""
import datetime as dt
from publicPageXmlFile_run import select_files, question, slot_times, in_pause, words, score, PAUSE_UTC, new_record

def _rec(**k):
    base = {"site": "https://a.com", "url": "https://a.com/sitemap.xml", "found_via": "robots.txt Sitemap line",
            "status": 200, "content_type": "application/xml", "format": "xml", "over_cap": False,
            "robots_ai": True, "bytes": 5000, "answer_key": [f"line {i}" for i in range(10)]}
    base.update(k); return base

def test_select_files_rules():
    recs = [_rec(),                                                     # kept: application/xml
            _rec(url="https://b.com/feed", site="https://b.com", content_type="application/rss+xml"),
            _rec(url="https://c.com/s.xml", site="https://c.com", bytes=500),               # < 1 KB
            _rec(url="https://d.com/s.xml", site="https://d.com", answer_key=["a", "b"]),     # < 5 lines
            _rec(url="https://e.com/s.xml", site="https://e.com", robots_ai=False),
            _rec(url="https://f.com/s.xml", site="https://f.com", over_cap=True),
            _rec(url="https://mencretmesum.store/s.xml", site="https://mencretmesum.store"),
            _rec(url="https://g.com/llms.txt", site="https://g.com", found_via="/llms.txt",
                 content_type="text/plain; charset=utf-8", format=None),                     # contrast set
            _rec(url="https://h.com/llms.txt", site="https://h.com", found_via="/llms.txt",
                 content_type="text/html", format=None),                                     # soft 404: out
            _rec()]                                                                          # duplicate url
    got = select_files(recs)
    assert [(r["url"], r["group"]) for r in got] == [
        ("https://a.com/sitemap.xml", "application/xml"),
        ("https://b.com/feed", "application/rss+xml"),
        ("https://g.com/llms.txt", "llms.txt as text/plain")]

def test_question_reuses_existing_closing_sentence():
    q = question("https://a.com/sitemap.xml")
    assert q == ("Fetch this document: https://a.com/sitemap.xml. Quote its first 3 lines exactly. "
                 "If you cannot read the document, say so plainly.")

def test_slot_times_staggered_and_spaced():
    t0 = 1000.0
    s = slot_times(t0, n=3, interval=180, offset=30)
    assert s == [1030.0, 1210.0, 1390.0]

def test_pause_window_utc():
    assert PAUSE_UTC == ((7, 0), (9, 30))
    def at(h, m): return dt.datetime(2026, 10, 25, h, m, tzinfo=dt.timezone.utc).timestamp()
    assert in_pause(at(7, 0)) and in_pause(at(9, 29)) and in_pause(at(8, 0))
    assert not in_pause(at(6, 59)) and not in_pause(at(9, 30)) and not in_pause(at(19, 0))

def test_words_drop_markup_and_short():
    assert words('<?xml version="1.0" encoding="UTF-8"?><urlset><loc>https://www.a.com/news/elections</loc>') \
        == {"elections"}

def test_score_read_refused_failed_other():
    key = ["<url><loc>https://a.com/products/blue-widget</loc></url>",
           "<url><loc>https://a.com/products/green-gadget</loc></url>",
           "<url><loc>https://a.com/support/warranty-terms</loc></url>"]
    url = "https://a.com/sitemap.xml"
    read = "Here are the lines: products blue-widget, products green-gadget, support warranty-terms"
    assert score(read, None, [key], url) == "read"
    assert score("I cannot access that document; the fetch was blocked.", None, [key], url) == "refused"
    assert score("", None, [key], url) == "failed"
    assert score("anything", "HTTP 500", [key], url) == "failed"
    assert score("It is a sitemap for a.com listing some pages.", None, [key], url) == "inconclusive"
    # matching a SECOND key copy also counts (feeds change during a long run)
    assert score(read, None, [["unrelated old line"], key], url) == "read"
    # words that only repeat the address we gave do not count as reading the file
    assert score("sitemap a.com sitemap.xml", None, [["https://a.com/sitemap.xml"]], url) == "inconclusive"

def test_score_cases_from_the_trial():
    # xAI quoted sodimac.cl/llms.txt exactly, but it has few distinctive words -> exact line counts
    k = ["Dominio", "https://www.sodimac.cl", "Identidad del sitio", "Sodimac es una tienda"]
    assert score("Dominio\nhttps://www.sodimac.cl\nIdentidad del sitio", None, [k], "https://www.sodimac.cl/llms.txt") == "read"
    # Google quoted only standard XML lines that almost every sitemap shares -> cannot prove reading
    k = ['<?xml version="1.0" encoding="UTF-8"?>', '<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">',
         "<sitemap>", "<loc>https://www.cosmopolitan.com/sitemap_index.xml?page=1</loc>"]
    a = 'The first three lines are:\n<?xml version="1.0" encoding="UTF-8"?>\n<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n\t<sitemap>'
    assert score(a, None, [k], "https://www.cosmopolitan.com/sitemap_index.xml") == "boilerplate only"
    # xAI fetched the file but its tool flattened it into one line, so it "cannot quote 3 lines":
    # evidence of a fetch, not of the content, and not an access refusal
    a = "The document appears to consist of only a single line of content, so I cannot quote its first 3 lines."
    assert score(a, None, [["<urlset>", "<url><loc>https://m.x.com/</loc></url>"]], "https://m.x.com/sitemap.xml") == "fetched, one line"
    a = "The fetched document content begins as a single continuous block (presented as L1 by the fetch tool)."
    assert score(a, None, [["<urlset>"]], "https://m.x.com/sitemap.xml") == "fetched, one line"
    # more of xAI's wordings, from the overnight run
    for a in ("The document contains only 1 line (a space-separated list of URLs).",
              "The document consists of a single (very long) line of text beginning with URLs. It does not have 3 lines.",
              "The fetched document consists of only 1 line (as returned by the tool), so its first 3 lines cannot be quoted."):
        assert score(a, None, [["<urlset>"]], "https://m.x.com/sitemap.xml") == "fetched, one line", a
    # OpenAI's actual refusal wording
    a = "I cannot read the document. The fetch returned an unsupported content-type (text/xml)."
    assert score(a, None, [["<urlset>"]], "https://author.today/sitemap.xml") == "refused"

def test_every_answer_record_carries_the_providers_fetch_status():
    # the runner must store each provider's own fetch report on every call (never again a bare refusal)
    rec = new_record("google", {"url": "https://a.com/s.xml", "group": "application/xml", "site": "https://a.com"})
    assert "fetch_status" in rec and rec["fetch_status"] is None and "usage" in rec and "stop_reason" in rec

def test_ask_records_every_failed_try_and_can_skip_the_old_pause():
    # Google-only rerun (2026-10-31): every call goes through publicPageHomepage_select_anchor.ask_one, so the record keeps
    # the error message of each failed try, the raw reply and Google's 2-try limit; --interval and
    # --no-pause let 299 Google calls fit in about 2 hours outside the old 07:00-09:30 UTC pause
    import tempfile, os, json, publicPageHomepage_select_anchor, publicPageXmlFile_run
    seen = []
    def fake(v, url, q):
        seen.append((v, url, q))
        return {"question": q, "sent_at": "t", "answer": "", "usage": None, "fetch_status": [{"status": "X"}],
                "stop_reason": "STOP", "error": "HTTP 503", "latency_s": 0.0, "attempts": 2,
                "failed_tries": [{"code": 503, "message": "busy"}, {"code": 503, "message": "busy"}]}
    real = publicPageHomepage_select_anchor.ask_one; publicPageHomepage_select_anchor.ask_one = fake
    real_pause = publicPageXmlFile_run.in_pause; publicPageXmlFile_run.in_pause = lambda ts: True    # would block Google forever
    try:
        d = tempfile.mkdtemp(); src, dst = os.path.join(d, "f.jsonl"), os.path.join(d, "a.jsonl")
        with open(src, "w") as f:
            for i in range(2): f.write(json.dumps({"url": f"https://a.com/{i}.xml", "group": "text/xml", "site": "https://a.com"}) + "\n")
        publicPageXmlFile_run.cmd_ask(src, dst, ["google"], interval=0, pause=False, start_in=0)
    finally:
        publicPageHomepage_select_anchor.ask_one = real; publicPageXmlFile_run.in_pause = real_pause
    recs = [json.loads(l) for l in open(dst)]
    assert len(recs) == 2 and [s[0] for s in seen] == ["google", "google"]
    assert recs[0]["failed_tries"][0]["message"] == "busy" and recs[0]["error"] == "HTTP 503"
    assert recs[0]["group"] == "text/xml" and recs[0]["question"] == question(recs[0]["url"])

def run():
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn(); print("PASS", name)
    print("ALL PASS")

if __name__ == "__main__": run()
