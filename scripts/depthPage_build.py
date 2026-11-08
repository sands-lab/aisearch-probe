#!/usr/bin/env python3
"""Build the filler text of the depth pages (proxy/depthcorpus.txt).

A depth page carries unforgeable marker codes at known depths, padded with real
prose. Real prose matters: a fetcher's text extractor keeps prose, but may throw
away a wall of repeated characters, and then the markers never reach the model.
The Go server embeds the built file, so the page is a pure function of its address.

Usage:
  scripts/depthPage_build.py                       # from the text shipped with the release
  scripts/depthPage_build.py --input my-text.txt   # from your own text file
  scripts/depthPage_build.py --input pg219.txt     # a raw Project Gutenberg file works too
  options: --output FILE (default proxy/depthcorpus.txt)

The shipped text (scripts/depthPage_source.txt) is Joseph Conrad's "Heart of
Darkness" (1899), a public-domain book, taken from Project Gutenberg with Project
Gutenberg's own header and licence footer cut off.

What the builder does to any input:
  1. If the text has Project Gutenberg's "*** START OF" and "*** END OF" lines,
     it keeps only what lies between them.
  2. It turns Windows line ends into plain ones and curly quotes and long dashes
     into plain ASCII ones.
  3. It drops every line that could spell or describe a marker: anything with
     "trunc" in it (the marker prefix is TRUNC-), the words the page prints next to
     a marker ("reference marker"), or a run of 12 or more hex characters (a code).
  4. It refuses to write a result that still contains the marker prefix, or that is
     too short to fill a page (under 1 KB).

After a rebuild, rebuild the server (go build ./proxy) so it embeds the new text.
Pages built from a different text are different pages: their sizes and codes stay
the same, but their token counts can change.
"""

import argparse
import os
import re
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_INPUT = os.path.join(REPO, "scripts", "depthPage_source.txt")
DEFAULT_OUTPUT = os.path.join(REPO, "proxy", "depthcorpus.txt")

MARKER_PREFIX = "TRUNC-"
MIN_BYTES = 1024

# A line matching this is dropped: it could spell or describe a marker.
DROP = re.compile(r"trunc|reference marker|\b[0-9a-f]{12,}\b", re.IGNORECASE)

PUNCT = {
    "‘": "'", "’": "'", "“": '"', "”": '"',
    "—": "--", "–": "-", "…": "...", " ": " ", "﻿": "",
}

START = re.compile(r"^\*\*\* ?START OF (THE|THIS) PROJECT GUTENBERG", re.IGNORECASE)
END = re.compile(r"^\*\*\* ?END OF (THE|THIS) PROJECT GUTENBERG", re.IGNORECASE)


def cut_gutenberg(text):
    """Keep only the book between Project Gutenberg's START and END lines, if any."""
    lines = text.split("\n")
    start = next((i for i, l in enumerate(lines) if START.match(l)), None)
    end = next((i for i, l in enumerate(lines) if END.match(l)), None)
    if start is None and end is None:
        return text
    lo = start + 1 if start is not None else 0
    hi = end if end is not None and end > lo else len(lines)
    return "\n".join(lines[lo:hi])


def normalise(text):
    text = text.replace("\r\n", "\n").replace("\r", "\n")
    for a, b in PUNCT.items():
        text = text.replace(a, b)
    return text


def clean(text):
    """The whole pipeline: returns (corpus, number of lines dropped)."""
    text = normalise(cut_gutenberg(normalise(text)))
    kept, dropped = [], 0
    for line in text.split("\n"):
        if DROP.search(line):
            dropped += 1
            continue
        kept.append(line.rstrip())
    corpus = "\n".join(kept).strip() + "\n"
    # Squeeze runs of blank lines: they are padding, not prose.
    corpus = re.sub(r"\n{3,}", "\n\n", corpus)
    return corpus, dropped


def check(corpus):
    """Reasons the corpus must not be written; empty when it is fine."""
    problems = []
    if MARKER_PREFIX.lower() in corpus.lower():
        problems.append(f"the marker prefix {MARKER_PREFIX} survived the cleaning")
    if len(corpus.encode("utf-8")) < MIN_BYTES:
        problems.append(f"the text is under {MIN_BYTES} bytes after cleaning")
    return problems


def main(argv=None):
    ap = argparse.ArgumentParser(description="Build proxy/depthcorpus.txt, the depth pages' filler text.")
    ap.add_argument("--input", default=DEFAULT_INPUT,
                    help="your own plain-text file (UTF-8); default: the shipped public-domain book")
    ap.add_argument("--output", default=DEFAULT_OUTPUT, help="where to write (default: proxy/depthcorpus.txt)")
    args = ap.parse_args(argv)

    with open(args.input, encoding="utf-8") as f:
        corpus, dropped = clean(f.read())
    problems = check(corpus)
    if problems:
        for p in problems:
            print("ERROR:", p, file=sys.stderr)
        return 1
    with open(args.output, "w", encoding="utf-8") as f:
        f.write(corpus)
    print(f"wrote {args.output}")
    print(f"  from:          {args.input}")
    print(f"  bytes:         {len(corpus.encode('utf-8'))}")
    print(f"  lines dropped: {dropped}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
