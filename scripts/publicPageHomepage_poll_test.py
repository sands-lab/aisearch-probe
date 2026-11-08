#!/usr/bin/env python3
"""TEST-FIRST spec for publicPageHomepage_poll.py (the change watch). Run: python3 -m pytest publicPageHomepage_poll_test.py
Covers: splitting sites into day-groups, evenly spaced slots inside the 10-minute cycle,
visible text as LINES (for spotting which lines changed), the per-site back-off after a
"slow down" answer, and the snapshot naming that stores each distinct page text once.
"""
from publicPageHomepage_poll import split_groups, slots, visible_lines, backoff_until, snapshot_name, headers_record

def test_split_groups_is_repeatable_and_balanced():
    sites = [f"https://s{i}.example" for i in range(9)]
    g = split_groups(sites, 2, seed=1)
    assert len(g) == 2 and sorted(g[0] + g[1]) == sorted(sites)
    assert abs(len(g[0]) - len(g[1])) <= 1
    assert g == split_groups(sites, 2, seed=1)          # same seed -> same groups and order
    assert g != split_groups(sites, 2, seed=2)

def test_slots_evenly_spaced_inside_cycle():
    s = slots(4, 600)
    assert s == [0.0, 150.0, 300.0, 450.0]
    assert slots(1, 600) == [0.0]
    assert all(0 <= x < 600 for x in slots(997, 600))

def test_visible_lines_split_at_blocks():
    html = ("<html><head><title>T</title><script>x=1</script></head><body>"
            "<h1>Top  story</h1><p>First <b>para</b></p><div>Second<br>third</div>"
            "<ul><li>a</li><li></li><li>b</li></ul><noscript>js</noscript></body></html>")
    assert visible_lines(html) == ["T", "Top story", "First para", "Second", "third", "a", "b"]

def test_backoff_only_after_slow_down():
    assert backoff_until(429, 1000.0) == 1000.0 + 3600
    assert backoff_until(503, 1000.0) == 1000.0 + 3600
    assert backoff_until(200, 1000.0) is None
    assert backoff_until(403, 1000.0) is None
    assert backoff_until(None, 1000.0) is None

def test_snapshot_name_is_per_site_and_text():
    a = snapshot_name("https://www.chess.com", ["x", "y"])
    assert a == snapshot_name("https://www.chess.com", ["x", "y"])
    assert a != snapshot_name("https://www.chess.com", ["x", "z"])
    assert a != snapshot_name("https://other.com", ["x", "y"])
    assert a.endswith(".txt.gz") and "/" in a

def test_headers_record_keeps_all_but_cookies():
    items = [("Cache-Control", "max-age=60"), ("ETag", '"abc"'), ("Set-Cookie", "sid=1"),
             ("Via", "1.1 varnish"), ("Via", "1.1 cloudfront"), ("set-cookie", "x=2"),
             ("X-Cache", "Hit from cloudfront"), ("Age", "12")]
    assert headers_record(items) == {"cache-control": "max-age=60", "etag": '"abc"',
                                     "via": "1.1 varnish, 1.1 cloudfront",
                                     "x-cache": "Hit from cloudfront", "age": "12"}
    assert headers_record([]) == {}

def run():
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn(); print("PASS", name)
    print("ALL PASS")

if __name__ == "__main__": run()
