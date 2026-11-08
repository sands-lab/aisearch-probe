#!/usr/bin/env python3
"""TEST-FIRST spec for publicPageXmlFile_select.py (public XML / Markdown / CSV files for the file-type check).
Run: python3 -m pytest publicPageXmlFile_select_test.py
"""
from publicPageXmlFile_select import sitemap_urls, feed_links, csv_links, declared_format, sitemap_csv_locs, pick, named_format, unpack

def test_sitemap_urls_from_robots():
    txt = "User-agent: *\nDisallow: /x\nSitemap: https://a.com/sitemap.xml\nsitemap:https://a.com/news.xml\n# Sitemap: no\n"
    assert sitemap_urls(txt) == ["https://a.com/sitemap.xml", "https://a.com/news.xml"]
    assert sitemap_urls("") == []

def test_feed_links_resolved():
    html = ('<head><link rel="alternate" type="application/rss+xml" href="/rss.xml">'
            '<link rel="alternate stylesheet" type="text/css" href="/a.css">'
            '<link type="application/atom+xml" rel="alternate" href="https://feeds.x.com/atom">'
            '<link rel="alternate" hreflang="fr" href="/fr/"></head>')
    assert feed_links(html, "https://www.x.com/") == ["https://www.x.com/rss.xml", "https://feeds.x.com/atom"]

def test_csv_links():
    html = '<a href="/data/pop.csv">x</a><a href="/data/pop.CSV?v=2">y</a><a href="/a.csv.html">z</a><a href="/b.pdf">p</a>'
    assert csv_links(html, "https://gov.x/") == ["https://gov.x/data/pop.csv", "https://gov.x/data/pop.CSV?v=2"]

def test_sitemap_csv_locs():
    xml = "<urlset><url><loc>https://g.x/a.csv</loc></url><url><loc>https://g.x/b.html</loc></url></urlset>"
    assert sitemap_csv_locs(xml) == ["https://g.x/a.csv"]

def test_declared_format_by_content_type_only():
    assert declared_format("application/xml; charset=utf-8") == "xml"
    assert declared_format("text/xml") == "xml"
    assert declared_format("application/rss+xml") == "xml"
    assert declared_format("application/atom+xml") == "xml"
    assert declared_format("text/markdown; charset=UTF-8") == "markdown"
    assert declared_format("text/x-markdown") == "markdown"
    assert declared_format("text/csv") == "csv"
    assert declared_format("application/csv") == "csv"
    assert declared_format("text/plain") is None          # e.g. an llms.txt served as plain text
    assert declared_format("text/html") is None
    assert declared_format(None) is None

def test_named_format():
    # llms.txt is Markdown by name; what the server declares is recorded separately
    assert named_format("https://a.com/llms.txt") == "markdown"
    assert named_format("https://a.com/data/x.csv") == "csv"
    assert named_format("https://a.com/sitemap.xml") == "xml"
    assert named_format("https://a.com/feed") is None

def test_unpack_gzip_even_when_identity_was_asked():
    # some servers send gzip bytes despite Accept-Encoding: identity (seen 2026-10-25: bilibili llms.txt,
    # several sitemaps) — the answer key must be the real text, not compressed bytes
    import gzip
    raw = "# Bilibili\n> A leading Chinese video sharing site\n".encode()
    assert unpack(gzip.compress(raw)) == raw
    assert unpack(raw) == raw
    assert unpack(b"\x1f\x8b not really gzip") == b"\x1f\x8b not really gzip"

def test_pick_one_per_site_per_format_capped_and_repeatable():
    c = [{"site": f"s{i % 5}", "format": "xml", "url": f"u{i}"} for i in range(20)] + \
        [{"site": "s0", "format": "csv", "url": "c0"}]
    p = pick(c, per_format=3, seed=1)
    xml = [x for x in p if x["format"] == "xml"]
    assert len(xml) == 3 and len({x["site"] for x in xml}) == 3
    assert [x for x in p if x["format"] == "csv"] == [{"site": "s0", "format": "csv", "url": "c0"}]
    assert p == pick(c, per_format=3, seed=1)

def run():
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn(); print("PASS", name)
    print("ALL PASS")

if __name__ == "__main__": run()
