#!/usr/bin/env python3
"""TEST-FIRST spec for publicPageHomepage_select_changes.py. Run: python3 -m pytest publicPageHomepage_select_changes_test.py
A REAL change is a new line that is still there on the next visit (lasts >= 20 min); lines that
show for one visit only (ads, rotating boxes) and clock/date-like lines are noise.
"""
from publicPageHomepage_select_changes import is_noise_line, real_change_steps, gaps_minutes, median, position_spread, busy_hours

def test_noise_lines():
    assert is_noise_line("10:20")
    assert is_noise_line("Updated 14:05")
    assert is_noise_line("5 minutes ago")
    assert is_noise_line("2026-10-26")
    assert is_noise_line("26.10.2026 13:45")
    assert is_noise_line("short")                     # under 10 characters: labels, buttons
    assert not is_noise_line("Storm hits the northern coast overnight")
    assert not is_noise_line("Vote result: parliament passes the budget")

def test_noise_lines_from_the_watch_data():
    # counters, prices, bylines and relative times in other languages (seen in the first run)
    assert is_noise_line("224コメント3時間前")            # "224 comments, 3 hours ago"
    assert is_noise_line("$7.00incl.")
    assert is_noise_line("13,30 €incl.")
    assert is_noise_line("Sz. Bujalski")                 # a byline: too few letters
    assert is_noise_line("1,234,567 views 2024")         # mostly digits
    # real headlines and post titles in any script stay
    assert not is_noise_line("Политолог: как проигрыш Нетаньяху повлияет на политику Израиля")
    assert not is_noise_line("4科すべてが偏差値60を超えた息子")
    assert not is_noise_line("같은 대검, 엔쇼우와 프레야의 차이는? - 몬헌 아웃랜더스")

def test_busy_hours():
    # share of a site's steps in each hour that brought a real change; busy = share >= 0.5
    steps = [(0, True), (0, True), (0, False), (1, False), (1, False), (2, True)]
    assert busy_hours(steps) == {0: 0.67, 1: 0.0, 2: 1.0}

def test_real_change_needs_the_line_to_last_one_more_visit():
    v = [["Header", "Clock 10:00 ignored", "Storm hits the coast tonight"],
         ["Header", "Clock 10:10 ignored", "Storm hits the coast tonight", "Rotating ad for shoes today"],
         ["Header", "Clock 10:20 ignored", "Vote result: budget passes in parliament"],
         ["Header", "Clock 10:30 ignored", "Vote result: budget passes in parliament"]]
    cycles = [0, 1, 2, 3]
    # step 0->1 adds only the one-visit ad (noise); step 1->2 adds the vote line, still there at 3 -> real
    steps = real_change_steps(v, cycles)
    assert [s["at"] for s in steps] == [2]
    assert steps[0]["lines"] == ["Vote result: budget passes in parliament"]
    assert steps[0]["first_index"] == 2

def test_real_change_skips_gaps_in_visits():
    v = [["a long stable header line"], ["a long stable header line", "brand new story line here"],
         ["a long stable header line", "brand new story line here"]]
    # cycle 1 missing between the first two visits: not a 10-minute step, so not counted
    assert real_change_steps(v, [0, 2, 3]) == []

def test_gaps_and_median():
    assert gaps_minutes([2, 3, 5, 9], cycle_min=10) == [10, 20, 40]
    assert median([10, 20, 40]) == 20
    assert median([]) is None

def test_position_spread_p10_p90():
    assert position_spread([5, 5, 6, 5, 7, 5, 5, 6, 5, 40]) <= 2   # one outlier ignored
    assert position_spread([]) is None

def run():
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn(); print("PASS", name)
    print("ALL PASS")

if __name__ == "__main__": run()
