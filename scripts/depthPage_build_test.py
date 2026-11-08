"""Tests for depthPage_build.py: the filler can never spell a marker, Project
Gutenberg's header and footer are cut, and the shipped text builds the shipped
corpus exactly."""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import depthPage_build as b  # noqa: E402

PROSE = "The river was calm and the boat lay still on the water near the bank.\n" * 30


def test_cuts_gutenberg_header_and_footer():
    raw = ("The Project Gutenberg eBook of Something\nLicence words\n"
           "*** START OF THE PROJECT GUTENBERG EBOOK SOMETHING ***\n"
           + PROSE +
           "*** END OF THE PROJECT GUTENBERG EBOOK SOMETHING ***\nMore licence words\n")
    corpus, _ = b.clean(raw)
    assert "Gutenberg" not in corpus and "Licence" not in corpus
    assert "The river was calm" in corpus


def test_plain_text_is_kept_whole():
    corpus, dropped = b.clean(PROSE)
    assert dropped == 0 and corpus.strip() == PROSE.strip()


def test_drops_lines_that_could_spell_a_marker():
    raw = PROSE + "a TRUNC-abc line\nthe reference marker for the section\ncode 0123456789abcdef here\n" + PROSE
    corpus, dropped = b.clean(raw)
    assert dropped == 3
    assert "trunc" not in corpus.lower() and "reference marker" not in corpus
    assert not b.check(corpus)


def test_windows_line_ends_and_curly_punctuation_become_plain():
    corpus, _ = b.clean(PROSE.replace("\n", "\r\n") + "“He said—no’s”\r\n")
    assert "\r" not in corpus
    assert "\"He said--no's\"" in corpus


def test_refuses_short_text():
    corpus, _ = b.clean("too short\n")
    assert b.check(corpus)


def test_shipped_source_builds_the_shipped_corpus(tmp_path):
    out = tmp_path / "corpus.txt"
    assert b.main(["--output", str(out)]) == 0
    with open(b.DEFAULT_OUTPUT, encoding="utf-8") as f:
        assert out.read_text(encoding="utf-8") == f.read()
