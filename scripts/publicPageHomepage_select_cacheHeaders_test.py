#!/usr/bin/env python3
"""TEST-FIRST spec for the cache-header survey's pure logic. Run: python3 survey_cache_headers_test.py
Covers parsing a Cache-Control header into directives and classifying the site's caching stance.
"""
from publicPageHomepage_select_cacheHeaders import parse_cache_control, classify, asks_fresh

def test_parse():
    assert parse_cache_control("no-store, max-age=0") == {"no-store": True, "max-age": "0"}
    assert parse_cache_control("public, max-age=3600") == {"public": True, "max-age": "3600"}
    assert parse_cache_control("") == {}
    assert parse_cache_control(None) == {}
    # case-insensitive token names, whitespace tolerant
    assert parse_cache_control("No-Cache ,  S-MaxAge=60") == {"no-cache": True, "s-maxage": "60"}

def test_classify():
    # priority: no-store > no-cache > max-age=0 > short max-age > cacheable > none
    assert classify({"no-store": True, "max-age": "0"}) == "no-store"
    assert classify({"no-cache": True, "max-age": "3600"}) == "no-cache"
    assert classify({"max-age": "0"}) == "max-age=0"
    assert classify({"max-age": "120"}) == "max-age<5min"
    assert classify({"max-age": "3600"}) == "cacheable"
    assert classify({"public": True}) == "cacheable"   # public, no max-age given
    assert classify({}) == "no-header"
    assert classify({"max-age": "notanumber"}) == "cacheable"  # unparseable -> not a don't-cache signal

def test_asks_fresh():
    # "asks for a fresh copy every time" = any of no-store/no-cache/max-age=0/max-age<300
    assert asks_fresh({"no-store": True}) is True
    assert asks_fresh({"no-cache": True}) is True
    assert asks_fresh({"max-age": "0"}) is True
    assert asks_fresh({"max-age": "299"}) is True
    assert asks_fresh({"max-age": "300"}) is False
    assert asks_fresh({"max-age": "3600"}) is False
    assert asks_fresh({}) is False

def run():
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn(); print("PASS", name)
    print("ALL PASS")

if __name__ == "__main__": run()
