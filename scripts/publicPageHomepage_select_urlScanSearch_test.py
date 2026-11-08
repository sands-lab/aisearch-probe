#!/usr/bin/env python3
"""TEST-FIRST spec for publicPageHomepage_select_urlScanSearch.py. Run: python3 search_public_scans_test.py
Covers the search query, picking the newest scan from a search reply, pulling the category
section out of a scan report untouched, and the summary counts.
"""
from publicPageHomepage_select_urlScanSearch import (search_query, newest, domain_categories_section, summarize, host_of,
                                 labels_for, excluded_by)

def test_search_query():
    assert search_query("www.chess.com", 30) == 'page.domain:"www.chess.com" AND date:>now-30d'
    # Cloudflare rejects ranges over one month ("Too long a date range. Max is 1 month.")
    try:
        search_query("www.chess.com", 31); assert False, "must refuse > 30 days"
    except ValueError: pass

def test_newest_picks_latest_time():
    j = {"results": [
        {"_id": "a", "task": {"time": "2026-09-01T00:00:00Z", "url": "https://x/"}, "result": "ra"},
        {"_id": "b", "task": {"time": "2026-10-20T10:00:00Z", "url": "https://x/"}, "result": "rb"},
        {"_id": "c", "task": {"time": "2026-10-01T00:00:00Z", "url": "https://x/"}, "result": "rc"}]}
    assert newest(j)["_id"] == "b"
    assert newest({"results": []}) is None
    assert newest({}) is None

def test_domain_categories_section_kept_whole():
    rep = {"meta": {"processors": {"domainCategories": {"data": [{"name": "x.com", "isPrimary": True}]}}}}
    assert domain_categories_section(rep) == {"data": [{"name": "x.com", "isPrimary": True}]}
    assert domain_categories_section({"meta": {}}) is None
    assert domain_categories_section({}) is None

def test_labels_for_reads_our_hosts_entry_only():
    # real shape (2026-10-22): one entry per hostname the scanned page touched;
    # our host's labels are its content (+ inherited.content), not the other hostnames'
    sec = {"data": [
        {"name": "cdn.cookielaw.org", "isPrimary": False},
        {"name": "www.chess.com", "isPrimary": True, "inherited": {},
         "content": [{"id": 100, "super_category_id": 7, "name": "Gaming"}, {"id": 7, "name": "Entertainment"}]},
        {"name": "ads.example", "isPrimary": False, "content": [{"id": 133, "name": "Pornography"}]}]}
    assert labels_for("www.chess.com", sec) == ["Gaming", "Entertainment"]
    sec2 = {"data": [{"name": "m.x.com", "isPrimary": True, "content": [{"name": "News & Media"}],
                      "inherited": {"content": [{"name": "Gambling"}, {"name": "News & Media"}]}}]}
    assert labels_for("m.x.com", sec2) == ["News & Media", "Gambling"]
    assert labels_for("absent.com", sec) is None          # our host not in that report
    assert labels_for("x", None) is None

def test_excluded_by_paper_list():
    assert excluded_by(["Gaming", "Entertainment"]) == []
    assert excluded_by(["Gambling", "CIPA Filter", "Gaming"]) == ["CIPA Filter", "Gambling"]
    assert excluded_by(None) == []

def test_summarize():
    recs = [{"found": True, "error": ""}, {"found": True, "error": ""},
            {"found": False, "error": ""}, {"found": False, "error": "HTTP 403"}]
    assert summarize(recs) == {"searched": 4, "found a recent public scan": 2,
                               "no recent public scan": 1, "lookup failed": 1}

def test_host_of():
    assert host_of("https://www.chess.com,1000") == "www.chess.com"

def run():
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn(); print("PASS", name)
    print("ALL PASS")

if __name__ == "__main__": run()
