#!/usr/bin/env python3
"""TEST-FIRST spec for publicPageHomepage_score_answerKinds.py — the post-run classifier for public-page answers.
Outcomes are fixed before the run; reason codes come from provider evidence and are derived after.
Order (first match wins): failed -> valid read (inconclusive / stale / fresh) -> refused -> off-target
-> unclassified. A valid read is checked BEFORE refusal wording, because real answers mix the two
(OpenAI on carview: "I can't provide the exact verbatim lines" + a correct excerpt).
Run: python3 -m pytest publicPageHomepage_score_answerKinds_test.py
"""
from publicPageHomepage_score_answerKinds import classify

A = "Popularne w serwisie Wiadomości:"
OLD = ["menu", A, "Stary nagłówek o pogodzie w Krakowie dzisiaj", "Wspólny nagłówek o sejmie i budżecie państwa",
       "Inny stary tekst o sporcie", "stopka"]
NEW = ["menu", A, "Nowy nagłówek o pociągach w Hiszpanii wczoraj", "Wspólny nagłówek o sejmie i budżecie państwa",
       "Inny nowy tekst o kulturze", "stopka"]
U = "https://wiadomosci.wp.pl/"

def C(answer, **kw):
    base = dict(answer=answer, error="", fetch_status=[], stop_reason=None, older=OLD, newer=NEW, anchor=A, url=U)
    base.update(kw); return classify(**base)

def test_fresh_and_stale_by_words_unique_to_one_copy():
    assert C("Nowy nagłówek o pociągach w Hiszpanii")[:2] == ("fresh", None)
    assert C("Stary nagłówek o pogodzie w Krakowie")[:2] == ("stale", None)

def test_inconclusive_when_only_shared_text_or_both():
    assert C("Wspólny nagłówek o sejmie i budżecie")[:2] == ("inconclusive", "shared_text_only")
    assert C("Stary nagłówek o pogodzie w Krakowie Nowy nagłówek o pociągach w Hiszpanii")[:2] == \
        ("inconclusive", "matches_both_versions")

def test_position_flag_near_or_shifted():
    long_new = ["menu", A] + [f"Krótka wiadomość numer {i} o czymś zupełnie innym" for i in range(4)] + \
               ["Nowy nagłówek o pociągach w Hiszpanii wczoraj"]
    out = C("Nowy nagłówek o pociągach w Hiszpanii", newer=long_new)
    assert out[0] == "fresh" and out[2] == {"position": "shifted"}
    assert C("Nowy nagłówek o pociągach w Hiszpanii")[2] == {"position": "near"}

def test_failed_from_system_evidence():
    assert C("", error="HTTP 503")[:2] == ("failed", "http_5xx")
    assert C("", error="TimeoutError: timed out")[:2] == ("failed", "timeout")
    assert C("")[:2] == ("failed", "empty_reply")
    assert C("", stop_reason="RECITATION")[:2] == ("refused", "quote_blocked_api")    # deliberate, not a crash

def test_valid_read_wins_over_refusal_wording():
    a = "I can't provide the exact verbatim lines (copyrighted text). Excerpt: Nowy nagłówek o pociągach w Hiszpanii"
    assert C(a)[0] == "fresh"

def test_refused_reasons():
    assert C("I cannot read the document.", fetch_status=[{"error_code": "url_not_allowed"}])[:2] == ("refused", "domain_not_allowed")
    assert C("I cannot read the document.", fetch_status=[{"url": U, "status": "URL_RETRIEVAL_STATUS_ERROR"}])[:2] == \
        ("refused", "fetch_status_error")
    assert C("I'm not able to reproduce more than 25 words verbatim from a single source.")[:2] == ("refused", "quote_limit_stated")
    assert C("I cannot read the document.")[:2] == ("refused", "cannot_read_stated")

def test_off_target_reasons():
    assert C('I fetched it, but it does not contain the line "Popularne w serwisie Wiadomości:".')[:2] == \
        ("off_target", "anchor_not_found")
    assert C("Do you mean the first 12 words counted as tokens, or as characters?")[:2] == ("off_target", "asked_back")
    assert C("menu Stary nagłówek")[0] in ("off_target", "unclassified")

def test_anchor_not_found_wins_over_cannot_read_wording():
    # OpenAI on news.mail.ru, 2026-10-28: it DID fetch, then could not find the label
    a = ('I cannot read the document in the way needed. I fetched the page but could not find the line '
         '"Popularne w serwisie Wiadomości:" in the page HTML, so I can\'t extract the 12 words that follow it.')
    assert C(a)[:2] == ("off_target", "anchor_not_found")
    # the provider's own fetch error still comes first
    assert C(a, fetch_status=[{"error_code": "url_not_allowed"}])[:2] == ("refused", "domain_not_allowed")

def test_unclassified_is_the_tripwire():
    assert C("Here is a nice summary of a Polish news site.")[:2] == ("unclassified", None)

def run():
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn(); print("PASS", name)
    print("ALL PASS")

if __name__ == "__main__": run()
