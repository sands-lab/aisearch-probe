#!/usr/bin/env python3
"""TEST-FIRST spec for publicPageHomepage_select_anchor.py: choosing a page's anchor (a stable short label from the watch
data), checking it on the live page just before asking, the approved question — the first 12 WORDS
after the anchor (no line counting: each service splits pages into lines differently; 12 words is
under OpenAI's ~25-word quote limit and light for Google's RECITATION filter) — and scoring.
Run: python3 anchor_check_test.py
"""
from publicPageHomepage_select_anchor import (pick_anchor, question, tagged, live_gate, is_label, words_after, spaceless,
                          score_follow)

H = "Latest news from the newsroom"
def test_pick_anchor_line_above_the_changing_part():
    versions = [["Site name header line", H, "Storm hits the northern coast", "Old story about markets"],
                ["Site name header line", H, "Vote result: budget passes", "Storm hits the northern coast"],
                ["Site name header line", H, "Train strike called for Monday", "Vote result: budget passes"]]
    assert pick_anchor(versions, [(1, 2), (2, 2)]) == {"anchor": H, "present_share": 1.0, "votes": 2}

def test_pick_anchor_rejects_repeated_rare_noise_or_long_lines():
    v = [["Menu item shown twice here", "Menu item shown twice here", "A new story line appears here"]] * 3
    assert pick_anchor(v, [(1, 2), (2, 2)]) is None
    v2 = [["short", "A brand new story line one"], ["short", "Another brand new story two"]]
    assert pick_anchor(v2, [(0, 1), (1, 1)]) is None
    long_headline = "Two people stabbed in an early morning fight in the city centre, police say"
    v3 = [[long_headline, "A brand new story line one"], [long_headline, "Another brand new story two"]]
    assert pick_anchor(v3, [(0, 1), (1, 1)]) is None

def test_is_label():
    assert is_label("Popularne w serwisie Wiadomości:") and is_label("オリジナル記事をもっと見る")
    assert not is_label("Two people stabbed in an early morning fight in the city centre, police say")
    assert not is_label("乗車を検知してチェック忘れを通知！プロの仕事を支えるアルコール検知器が進化してる件")

def test_question_is_the_approved_wording():
    assert question("https://a.com/?x=1", "Latest news") == (
        'Fetch this document: https://a.com/?x=1. Quote exactly the first 12 words that follow the line '
        '"Latest news" in it. If you cannot read the document, say so plainly.')

def test_question_for_pages_without_spaces_asks_characters():
    # approved 2026-10-28: Japanese/Chinese pages have no word spaces, so ask for characters
    assert question("https://c.jp/?x=1", "オリジナル記事をもっと見る", spaceless_page=True) == (
        'Fetch this document: https://c.jp/?x=1. Quote exactly the first 30 characters that follow the line '
        '"オリジナル記事をもっと見る" in it. If you cannot read the document, say so plainly.')

def test_tagged_adds_one_item_before_any_fragment():
    assert tagged("https://a.com", "8f3a91c2") == "https://a.com/?x=8f3a91c2"
    assert tagged("https://a.com/p?lang=en", "ab") == "https://a.com/p?lang=en&x=ab"
    assert tagged("https://a.com/p#top", "ab") == "https://a.com/p?x=ab#top"

def test_words_after_flattens_lines_and_drops_punctuation():
    page = ["top", H, "", "Train strike: called for Monday.", "2026.10.28", "Vote result — budget passes"]
    assert words_after(page, H, 6) == ["train", "strike", "called", "for", "monday", "2026.10.28"]
    assert words_after(page, "not on page", 5) is None

def test_spaceless():
    assert spaceless("オリジナル記事をもっと見る乗車を検知して") and not spaceless("Train strike called")

def test_live_gate():
    page = ["top", H, "Storm hits the northern coast tonight and tomorrow morning along the bay area too"]
    ok, exp = live_gate(page, H)
    assert ok == "ok" and exp.split()[:3] == ["storm", "hits", "the"]
    assert live_gate(page + [H], H)[0] == "anchor repeated on the live page"
    assert live_gate(["top"], H)[0] == "anchor missing on the live page"

PAGE = ["Site name header line", H, "Train strike called for Monday morning",
        "Vote result: budget passes in parliament", "Storm hits the northern coast tonight",
        "Football: cup final ends in penalties", "Footer contact address imprint privacy policy page here"]
U = "https://a.com/"

def test_score_followed_in_order_despite_extra_bits():
    ok = 'The first 12 words are: "Train strike called for Monday morning Vote result: budget passes in parliament"'
    assert score_follow(ok, None, [PAGE], H, U) == "followed"
    # a service that inserts an image description or a date still matches a long run in order
    ok2 = "Train strike called for Monday morning [image] Vote result budget"
    assert score_follow(ok2, None, [PAGE], H, U) == "followed"

def test_score_other_outcomes():
    wrong = "Footer contact address imprint privacy policy page here"
    assert score_follow(wrong, None, [PAGE], H, U) == "wrong place"
    nf = 'I fetched the document, but it does not contain the line "Latest news from the newsroom".'
    assert score_follow(nf, None, [PAGE], H, U) == "anchor not found by the service"
    assert score_follow("I cannot read the document.", None, [PAGE], H, U) == "refused"
    assert score_follow("", "HTTP 503", [PAGE], H, U) == "failed"
    assert score_follow("Here is a summary of a news site.", None, [PAGE], H, U) == "inconclusive"

def test_score_spaceless_page_by_characters():
    jp = ["上部メニュー", "オリジナル記事をもっと見る",
          "スーパーGT第3戦／第8戦もてぎのフォーマットが確定。各日で計時予選と250kmレースを実施", "2026.10.28", "スポーツ"]
    a = "最初の12語：スーパーGT第3戦／第8戦もてぎのフォーマット"
    assert score_follow(a, None, [jp], "オリジナル記事をもっと見る", "https://c.jp/") == "followed"

def test_remote_snap_command_takes_contact_from_env_only():
    from publicPageHomepage_select_anchor import remote_snap_cmd, ask_id
    cmd = remote_snap_cmd("https://hvg.hu", contact="someone@example.org")
    assert cmd[:3] == ["ssh", "-o", "BatchMode=yes"] and "probe-vps" in cmd
    assert "publicPageHomepage_select_anchor.py snap1 https://hvg.hu" in cmd[-1] and "PROBE_CONTACT=someone@example.org" in cmd[-1]
    # a page's answers and its two copies share one id
    assert ask_id("https://hvg.hu", "2026-10-28T15:00:00Z") == "https://hvg.hu@2026-10-28T15:00:00Z"

def test_google_gets_two_tries_others_keep_theirs():
    # Google's free tier allows 500 requests a day and every failed try counts (2026-10-30)
    import publicPageHomepage_select_anchor, publicPage_run
    from publicPage_config import POST_KW
    seen = {}
    def fake(name):
        def f(*a, post_kw=None, **k):
            seen[name] = post_kw["retries"]; return "ok", {}
        return f
    real = (publicPage_run.ask_gemini, publicPage_run.ask_anthropic, publicPage_run.ask_openrouter)
    publicPage_run.ask_gemini, publicPage_run.ask_anthropic, publicPage_run.ask_openrouter = \
        fake("google"), fake("anthropic"), fake("openrouter")
    try:
        for v in ("google", "anthropic", "openai"): publicPageHomepage_select_anchor.ask_one(v, "https://a.com/", "q")
    finally:
        publicPage_run.ask_gemini, publicPage_run.ask_anthropic, publicPage_run.ask_openrouter = real
    assert seen == {"google": 2, "anthropic": POST_KW["retries"], "openrouter": POST_KW["retries"]}

def test_ask_one_keeps_the_full_error_message():
    # every failure keeps its own words, not just a type name or a bare code (2026-10-30)
    import io, urllib.error, publicPageHomepage_select_anchor, publicPage_run
    def boom(*a, **k): raise ValueError("Expecting value: line 1 column 1 (char 0)")
    def http(*a, **k):
        raise urllib.error.HTTPError("u", 400, "Bad Request", {}, io.BytesIO(b'{"error": {"message": "API key expired"}}'))
    real = publicPage_run.ask_gemini
    try:
        publicPage_run.ask_gemini = boom
        r = publicPageHomepage_select_anchor.ask_one("google", "https://a.com/", "q")
        assert r["error"] == "ValueError" and "Expecting value" in r["error_detail"]
        publicPage_run.ask_gemini = http
        r = publicPageHomepage_select_anchor.ask_one("google", "https://a.com/", "q")
        assert r["error"] == "HTTP 400" and "API key expired" in r["error_detail"]
    finally:
        publicPage_run.ask_gemini = real

def run():
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn(); print("PASS", name)
    print("ALL PASS")

if __name__ == "__main__": run()
