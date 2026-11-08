#!/usr/bin/env python3
"""TEST-FIRST spec for publicPageHomepage_score_table.py (robustness re-judging of the public-page staleness run).
Run: python3 -m pytest publicPageHomepage_score_table_test.py
"""
from publicPageHomepage_score_table import reference, mcnemar_p, outcome

def agree_table(pairs):
    s = set(map(frozenset, pairs))
    return lambda a, b: frozenset((a, b)) in s

def test_reference_is_the_largest_agreeing_group():
    srcs = ["d_x", "d_y", "d_z", "E"]
    ag = agree_table([("d_x", "d_y"), ("d_x", "E"), ("d_y", "E")])        # x, y, E all agree; z alone
    assert reference(srcs, ag) == ("ok", {"d_x", "d_y", "E"})

def test_reference_needs_at_least_two_and_no_tie():
    srcs = ["d_x", "d_y", "d_z", "E"]
    assert reference(srcs, agree_table([]))[0] == "no reference"
    # two equally large groups that disagree with each other
    assert reference(srcs, agree_table([("d_x", "d_y"), ("d_z", "E")]))[0] == "tie"
    # two pairs sharing a member but not a 3-group: still two equally large groups -> tie
    assert reference(srcs, agree_table([("d_x", "d_y"), ("d_y", "E")]))[0] == "tie"
    assert reference(["d_x", "E"], agree_table([("d_x", "E")])) == ("ok", {"d_x", "E"})

def test_outcome_four_classes():
    assert outcome(c_hits=False, d_hits=True) == "stale"
    assert outcome(c_hits=True, d_hits=False) == "chance mismatch"
    assert outcome(c_hits=True, d_hits=True) == "both match"
    assert outcome(c_hits=False, d_hits=False) == "neither matches"

def test_mcnemar_exact_two_sided():
    assert mcnemar_p(0, 0) == 1.0
    assert abs(mcnemar_p(20, 3) - 0.000488) < 1e-6     # 2 * P(X <= 3), X ~ Bin(23, 1/2) = 2 * 2048 / 2^23
    assert mcnemar_p(9, 9) == 1.0
    assert mcnemar_p(5, 0) == 0.0625

Q = ('Fetch this document: https://www.rmf24.pl/?x=15ed320c. Quote exactly the first 12 words that follow the line '
     '"Najważniejsze Fakty" in it. If you cannot read the document, say so plainly.')

def test_prompt_words_are_removed_before_answers_are_compared():
    # 2 Nov: two answers "agreed" only because both repeated the question's own wording
    from publicPageHomepage_score_table import strip_prompt, ans_ans
    a = 'The first 12 words that follow the line "Najważniejsze Fakty" are: "Sikorski o decyzji Nawrockiego"'
    b = 'The first 12 words that follow the line "Najważniejsze Fakty" in the document are: "Nietypowy komunikat z Korei"'
    assert "najważniejsze" not in strip_prompt(a, Q, False) and "follow" not in strip_prompt(a, Q, False)
    assert "sikorski" in strip_prompt(a, Q, False)
    assert not ans_ans(a, Q, b, Q, False)
    c = 'Nietypowy komunikat z Korei Północnej ws. arsenału nuklearnego'
    assert ans_ans(b + " Północnej ws. arsenału nuklearnego", Q, c, Q, False)

def test_no_quote_answers_are_recognised():
    from publicPageHomepage_score_table import no_quote
    page = ["Najważniejsze Fakty", "Nietypowy komunikat z Korei Północnej ws. arsenału nuklearnego"]
    assert no_quote('The line "Najważniejsze Fakty" is a heading in the navigation menu.', Q, [page], False)
    assert not no_quote('"Nietypowy komunikat z Korei Północnej"', Q, [page], False)
    # a description shares single words with the page, not a 5-word run: still no quote (review, 2 Nov)
    assert no_quote('The heading "Najważniejsze Fakty" is followed by a komunikat link about Korei.', Q, [page], False)
    jq = 'Fetch this document: https://c.jp/?x=1. Quote exactly the first 30 characters that follow the line "オリジナル記事をもっと見る" in it.'
    jpage = ["オリジナル記事をもっと見る", "スーパーGT第3戦／第8戦もてぎのフォーマットが確定"]
    assert no_quote('The line "オリジナル記事をもっと見る" is followed only by images.', jq, [jpage], True)
    assert not no_quote('「スーパーGT第3戦／第8戦もてぎの」', jq, [jpage], True)
    assert no_quote('「スーパーです」', jq, [jpage], True)            # 4 shared characters is not a quote

def run():
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn(); print("PASS", name)
    print("ALL PASS")

if __name__ == "__main__": run()
