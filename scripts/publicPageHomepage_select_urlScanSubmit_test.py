#!/usr/bin/env python3
"""TEST-FIRST spec for publicPageHomepage_select_urlScanSubmit.py. Run: python3 submit_scans_test.py
Covers: which sites still need a scan (a site is never submitted twice, even across restarts),
reading the submit reply, and which submitted scans still need their report read.
"""
from publicPageHomepage_select_urlScanSubmit import still_to_submit, parse_submit, still_to_read, scan_body, labels_with_fallback

def test_never_resubmit():
    hosts = ["a.com", "b.com", "c.com"]
    done = [{"host": "a.com", "uuid": "u1", "error": ""},
            {"host": "b.com", "uuid": None, "error": "HTTP 400"}]   # a failed submit also counts: no retry storm
    assert still_to_submit(hosts, done) == ["c.com"]
    assert still_to_submit(hosts, []) == hosts

def test_parse_submit():
    r = {"uuid": "095be615-a8ad-4c33-8e9c-c7612fbf6c9f", "api": "https://x/result/095b",
         "visibility": "public", "url": "https://a.com/", "message": "Submission successful"}
    assert parse_submit(r) == {"uuid": "095be615-a8ad-4c33-8e9c-c7612fbf6c9f", "visibility": "public"}
    assert parse_submit({}) == {"uuid": None, "visibility": None}

def test_still_to_read():
    subs = [{"host": "a.com", "uuid": "u1", "error": ""}, {"host": "b.com", "uuid": "u2", "error": ""},
            {"host": "c.com", "uuid": None, "error": "HTTP 400"}]
    got = [{"host": "a.com", "uuid": "u1", "ready": True}]
    assert still_to_read(subs, got) == [("b.com", "u2")]
    # a report that was not ready yet is read again
    got2 = [{"host": "a.com", "uuid": "u1", "ready": False}]
    assert still_to_read(subs, got2) == [("a.com", "u1"), ("b.com", "u2")]

def test_labels_with_primary_fallback():
    sec = {"data": [{"name": "cdn.x.net", "isPrimary": False},
                    {"name": "www.landing.com", "isPrimary": True, "inherited": {},
                     "content": [{"name": "News & Media"}, {"name": "Gambling"}]}]}
    # our host has its own entry -> use it
    own = {"data": [{"name": "a.com", "isPrimary": False, "content": [{"name": "Gaming"}]}] + sec["data"]}
    assert labels_with_fallback("a.com", own) == ("a.com", ["Gaming"])
    # redirected: no entry for our host -> the page the browser ended on
    assert labels_with_fallback("m.a.com", sec) == ("www.landing.com", ["News & Media", "Gambling"])
    # real shape (s.tabelog.com, 2026-10-23): our entry is listed but has no labels; the
    # browser ended on tabelog.com (isPrimary), which carries them
    tab = {"data": [{"name": "s.tabelog.com", "isPrimary": False},
                    {"name": "tabelog.com", "isPrimary": True, "inherited": {},
                     "content": [{"name": "Shopping"}, {"name": "Food & Drink"}]}]}
    assert labels_with_fallback("s.tabelog.com", tab) == ("tabelog.com", ["Shopping", "Food & Drink"])
    # our entry empty and no primary with labels -> report our own empty list
    assert labels_with_fallback("a.com", {"data": [{"name": "a.com", "isPrimary": True}]}) == ("a.com", [])
    # nothing usable
    assert labels_with_fallback("a.com", {"data": []}) == (None, None)
    assert labels_with_fallback("a.com", None) == (None, None)

def test_scan_body_public_homepage_only():
    assert scan_body("www.chess.com") == {"url": "https://www.chess.com/", "visibility": "Public"}

def run():
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn(); print("PASS", name)
    print("ALL PASS")

if __name__ == "__main__": run()
