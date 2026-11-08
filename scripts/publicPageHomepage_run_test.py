#!/usr/bin/env python3
"""TEST-FIRST spec for publicPageHomepage_run.py — the staleness test on public pages (design agreed 2026-10-28).
Per page, in an hour it usually changes: copy A, warm-up question on the PLAIN address, 20 minutes,
then the plain and the random-tag question at the same moment, then copy B. Stale only if the plain
answer has text found only in copy A AND the tagged answer from the same moment does not.
Run: python3 stale_check_test.py
"""
from datetime import datetime, timezone, timedelta
from publicPageHomepage_run import plan, plain, verdict

T0 = datetime(2026, 10, 29, 9, 0, tzinfo=timezone.utc)     # 12:00 Riyadh

def riyadh_hour(iso):
    return (datetime.fromisoformat(iso.replace("Z", "+00:00")) + timedelta(hours=3)).hour

def test_plain_address_is_the_tag_free_twin_of_the_tagged_one():
    assert plain("https://hvg.hu") == "https://hvg.hu/"
    assert plain("https://a.com/p?lang=en") == "https://a.com/p?lang=en"

def test_plan_puts_each_page_in_one_of_its_busy_hours():
    pages = [{"origin": "https://a.com", "anchor": "A", "busy_hours_riyadh": [14]},
             {"origin": "https://b.com", "anchor": "B", "busy_hours_riyadh": list(range(24))},
             {"origin": "https://c.com", "anchor": "C", "busy_hours_riyadh": [12, 13]}]
    p = plan(pages, T0, gap_s=240, seed=1)
    by = {r["origin"]: r for r in p}
    assert set(by) == {"https://a.com", "https://b.com", "https://c.com"}
    assert riyadh_hour(by["https://a.com"]["start"]) == 14
    assert riyadh_hour(by["https://c.com"]["start"]) in (12, 13)
    starts = sorted(datetime.fromisoformat(r["start"].replace("Z", "+00:00")) for r in p)
    assert starts[0] >= T0 and all((b - a).total_seconds() >= 120 for a, b in zip(starts, starts[1:]))
    assert p == plan(pages, T0, gap_s=240, seed=1)                    # same seed, same timetable
    assert [r["start"] for r in p] == sorted(r["start"] for r in p)  # written in time order

def test_plan_never_double_books_a_slot_and_waits_for_the_next_day_if_needed():
    pages = [{"origin": f"https://p{i}.com", "anchor": "A", "busy_hours_riyadh": [12]} for i in range(20)]
    p = plan(pages, T0, gap_s=240, seed=2)
    assert len(p) == 20 and all(riyadh_hour(r["start"]) == 12 for r in p)
    days = {r["start"][:10] for r in p}
    assert len(days) == 2           # 15 four-minute slots in one hour -> 5 pages move to the next day

def test_verdict_stale_only_when_the_tagged_answer_at_the_same_moment_is_not():
    assert verdict(("stale", None), ("fresh", None)) == ("stale", None)
    assert verdict(("stale", None), ("inconclusive", "shared_text_only")) == ("stale", None)
    # both old: the page itself may still have served old text at that moment, not the service's copy
    assert verdict(("stale", None), ("stale", None)) == ("inconclusive", "tagged_also_old")
    # the tagged answer failed: no same-moment check, so no claim
    assert verdict(("stale", None), ("failed", "http_5xx")) == ("inconclusive", "no_tagged_check")
    assert verdict(("fresh", None), ("failed", "http_5xx")) == ("fresh", None)
    assert verdict(("refused", "cannot_read_stated"), ("fresh", None)) == ("refused", "cannot_read_stated")

def run():
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn(); print("PASS", name)
    print("ALL PASS")

if __name__ == "__main__": run()
