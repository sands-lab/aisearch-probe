#!/usr/bin/env python3
"""Build the frozen filler corpus for the truncation-depth probe (/depthpage/).

The depth page needs realistic prose as filler so a fetcher's readability
extractor keeps the page (a wall of one repeated character gets discarded as
junk — see doc/truncation-depth-probe-design.md). The filler is a SNAPSHOT of
this project's own findings notes, frozen so the page stays a pure function of
(id, bytes) and the run is reproducible.

Two rules make it safe (both enforced by tests over the frozen output):

  1. NO COLLISION. The marker prefix `TRUNC-` must never appear in the filler, or
     a marker could be faked by the filler. We drop every line that mentions the
     marker scheme (trunc/depthpage/sizepage/objsize/the shared RCP marker) or
     carries a code-shaped hex run.
  2. NOT SELF-DESCRIBING. Lines that narrate THIS probe are dropped by the same
     rule, so the page does not explain its own experiment to the model reading it.

Provenance: run against a committed tree; record the commit in the design doc.
The frozen output (proxy/depthcorpus.txt) is committed alongside this recipe.
"""

import os
import re
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(REPO, "proxy", "depthcorpus.txt")

# Findings prose to snapshot. Big, prose-rich docs about the caching results —
# NOT the docs that describe the size/truncation probes themselves.
SOURCE_GLOBS = [
    "DECISIONS.md",
    "RUNBOOK.md",
    "doc/study-plan.md",
    "doc/open-measurements.md",
    "doc/caching-policy-refs.md",
    "doc/cache-scope-framework.md",
    "doc/fault-injection.md",
    "doc/negcache-paired-handover.md",
]

# A line matching any of these is dropped: it mentions the marker scheme, this
# probe, the shared marker, or carries a code-shaped hex run.
DROP = re.compile(
    r"trunc|depthpage|sizepage|objsize|page-code-secret|PROBE_PAGE_CODE|RCP-2026-08|\b[0-9a-f]{12,}\b",
    re.IGNORECASE,
)


def main():
    parts = []
    used = []
    for rel in SOURCE_GLOBS:
        path = os.path.join(REPO, rel)
        if not os.path.exists(path):
            continue
        with open(path, encoding="utf-8") as f:
            kept = [ln for ln in f.read().splitlines() if not DROP.search(ln)]
        parts.append("\n".join(kept))
        used.append(rel)

    corpus = "\n\n".join(parts).strip() + "\n"

    # Hard guarantee before writing: the reserved prefix must be absent.
    if "TRUNC-" in corpus:
        print("ERROR: reserved prefix TRUNC- survived sanitisation", file=sys.stderr)
        sys.exit(1)

    with open(OUT, "w", encoding="utf-8") as f:
        f.write(corpus)

    print(f"wrote {OUT}")
    print(f"  sources: {', '.join(used)}")
    print(f"  bytes:   {len(corpus.encode('utf-8'))}")
    print(f"  TRUNC- occurrences: {corpus.count('TRUNC-')} (must be 0)")


if __name__ == "__main__":
    main()
