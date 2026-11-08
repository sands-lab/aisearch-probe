#!/usr/bin/env python3
"""TEST-FIRST spec for publicPageHomepage_score_anthropicCopies.py: staleness from the copy Anthropic actually fetched (its saved
page text and its own retrieved_at), not from how it worded the answer.
Run: python3 -m pytest publicPageHomepage_score_anthropicCopies_test.py
"""
from publicPageHomepage_score_anthropicCopies import age_group, reuse, share, fetch_error

def test_age_groups():
    assert age_group(None) == "no copy"
    assert age_group(0.5) == "fresh (under 2 min)"
    assert age_group(2) == "2 min to 1 h"
    assert age_group(60) == "1 to 24 h"
    assert age_group(24 * 60) == "1 to 7 days"
    assert age_group(7 * 24 * 60) == "over 7 days"

def test_reuse_compares_the_two_fetch_times():
    w = "2026-11-02T05:06:25+00:00"
    assert reuse(w, w) == "same copy as the warm-up"
    assert reuse(w, "2026-11-02T05:30:00+00:00") == "newer copy than the warm-up"
    assert reuse(w, "2026-11-01T05:06:25+00:00") == "older copy than the warm-up"
    assert reuse(None, w) == "no warm-up copy" and reuse(w, None) == "no copy"

def test_share_counts_5_word_runs_of_the_copy_found_in_our_download():
    page = ["Nietypowy komunikat z Korei Północnej ws. arsenału nuklearnego", "Dzisiaj 2 listopada"]
    same = "[Nietypowy komunikat](https://x.pl/a) z Korei Północnej ws. arsenału nuklearnego\nDzisiaj 2 listopada"
    assert share(same, page, False) == 1.0                      # link addresses do not count
    assert share("Sikorski o decyzji Nawrockiego chodzi o wprowadzenie ludzi Brauna", page, False) == 0.0
    assert share("", page, False) is None                       # nothing to compare
    jp = ["スーパーGT第3戦／第8戦もてぎのフォーマットが確定"]
    assert share("スーパーGT第3戦／第8戦もてぎのフォーマットが確定", jp, True) == 1.0

def test_fetch_error_read_from_the_raw_reply():
    raw = [{"content": [{"type": "server_tool_use", "name": "web_fetch"},
                        {"type": "web_fetch_tool_result",
                         "content": {"type": "web_fetch_tool_result_error", "error_code": "url_not_allowed"}}]}]
    assert fetch_error(raw) == "url_not_allowed"
    assert fetch_error([{"content": [{"type": "text", "text": "x"}]}]) == "no fetch attempted"

def test_each_copy_is_compared_with_our_download_closest_in_time():
    # the warm-up goes out at 0:00 with our start download A; the two later questions at 0:20, just
    # before our end download E. Comparing the warm-up with E (21 min later) understated it (62% vs 70%).
    from publicPageHomepage_score_anthropicCopies import closest_download
    assert closest_download("warmup") == "older"
    assert closest_download("plain") == "newer" and closest_download("tagged") == "newer"

def run():
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn(); print("PASS", name)
    print("ALL PASS")

if __name__ == "__main__": run()
