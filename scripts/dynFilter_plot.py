#!/usr/bin/env python3
"""Dynamic-filtering figure, two panels: (a) input tokens per document type at 8 KB, filtering off
vs on, (b) input tokens against HTML page size.
Reads the read-depth result records of the dynamic-filtering rounds (dynFilter_run.sh).
Usage: dynFilter_plot.py [--out <path without extension>] [result files ...]
       (default files: logs/results/truncprobe-run*.jsonl)
"""
import argparse, json, glob, re, statistics as st

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.ticker import FuncFormatter, FixedLocator
from matplotlib.patches import Patch

OFF_C, ON_C = "#5b6ee1", "#d97757"
FMT = ["txt", "html", "md", "json", "xml", "csv", "pdf", "docx"]

# Depth page addresses: the current /depth/<fmt>/ and the older /depth<fmt>page/.
PAGE_RE = re.compile(r"/depth(?:/([A-Za-z]+)/|([a-z]+)page/)")

def page_fmt(url):
    m = PAGE_RE.search(url or "")
    return (m.group(1) or m.group(2)) if m else None

_ap = argparse.ArgumentParser(description="Draw the dynamic-filtering figure.")
_ap.add_argument("--out", default="dynamic-filtering", help="output path without extension")
_ap.add_argument("files", nargs="*", help="result files (default logs/results/truncprobe-run*.jsonl)")
ARGS = _ap.parse_args()


def cell(v, w):
    if v != "anthropic":
        return None
    return "A" if w == "web_fetch_20250910" else "B" if w == "web_fetch_20260209" else None


# ---- load all pooled records once ----
by8k = {}       # (cell, fmt) -> [tokens]  at 8 KB
byhtml = {}     # (cell, size) -> [tokens] for html
for f in sorted(ARGS.files or glob.glob("logs/results/truncprobe-run*.jsonl")):
    for line in open(f):
        r = json.loads(line); ex = r.get("extra", {})
        if ex.get("part") != "A":
            continue
        c = cell(r.get("vendor", ""), ex.get("web_fetch_version", ""))
        it = r.get("input_tokens")
        if not (c and it):
            continue
        fmt = page_fmt(r.get("url", ""))
        if not fmt:
            continue
        if ex.get("size_bytes") == 8192:
            by8k.setdefault((c, fmt), []).append(it)
        if fmt == "html":
            byhtml.setdefault((c, ex.get("size_bytes")), []).append(it)

fig, (axA, axB) = plt.subplots(1, 2, figsize=(17, 6.5), dpi=200)

# ===== Panel (a): 8 KB variance bars =====
types = sorted([f for f in FMT if ("B", f) in by8k and ("A", f) in by8k],
               key=lambda f: st.mean(by8k[("B", f)]), reverse=True)
x = range(len(types)); w = 0.38
for i, f in enumerate(types):
    for c, color, off in (("A", OFF_C, -w/2), ("B", ON_C, +w/2)):
        v = by8k[(c, f)]; m = st.mean(v)
        axA.bar(i + off, m, w, color=color, zorder=2)
        axA.errorbar(i + off, m, yerr=[[m - min(v)], [max(v) - m]], color="#222", capsize=4, elinewidth=1.3, zorder=3)
    v = by8k[("B", f)]; ratio = max(v) / min(v)
    axA.text(i + w/2, max(v), f"{ratio:.0f}×" if ratio >= 2 else f"{ratio:.1f}×",
             ha="center", va="bottom", fontsize=8.5, color=ON_C, fontweight="bold")
axA.legend(handles=[Patch(color=OFF_C, label="Dynamic filtering OFF"),
                    Patch(color=ON_C, label="Dynamic filtering ON")], loc="upper right", fontsize=9)
axA.set_xticks(list(x)); axA.set_xticklabels(types)
axA.yaxis.set_major_formatter(FuncFormatter(lambda v, _: f"{int(v/1000)}k" if v >= 1000 else str(int(v))))
axA.set_xlabel("Object / file type  (8 KB page)")
axA.set_ylabel("Input Tokens")
axA.set_title("(a)  Cost by file type (8 KB)")
axA.grid(True, axis="y", ls=":", alpha=0.4); axA.margins(x=0.02)

# ===== Panel (b): HTML depth curve =====
SIZES = [2048, 4096, 8192, 16384, 32768, 65536, 131072]
axB.axvspan(33 * 1024, 148 * 1024, color="#cfe3cf", alpha=0.5, zorder=0)
axB.axvline(33 * 1024, color="#2e6b2e", ls="--", lw=1.2, zorder=1)
axB.axvline(148 * 1024, color="#2e6b2e", ls="--", lw=1.2, zorder=1)
axB.text(70 * 1024, 0.10, "real HTML sizes\n33–148 KB\n(median → p90)", transform=axB.get_xaxis_transform(),
         ha="center", va="center", fontsize=12, fontweight="bold", color="#1f5a1f")
for c, color, label in (("A", OFF_C, "Dynamic filtering OFF"), ("B", ON_C, "Dynamic filtering ON")):
    xs, ys, lo, hi = [], [], [], []
    for s in SIZES:
        v = byhtml.get((c, s))
        if not v:
            continue
        m = st.mean(v); xs.append(s); ys.append(m); lo.append(m - min(v)); hi.append(max(v) - m)
    axB.errorbar(xs, ys, yerr=[lo, hi], color=color, lw=3, marker="o", ms=7, capsize=5, elinewidth=1.5, label=label)
axB.annotate("~2.0M in one fetch", xy=(131072, 2029321), xytext=(11000, 2029321 * 0.8),
             fontsize=8.5, color="#333", va="center", arrowprops=dict(arrowstyle="->", color="#333", lw=1))
axB.set_xscale("log"); axB.set_yscale("log"); axB.set_xticks(SIZES)
axB.xaxis.set_major_formatter(FuncFormatter(lambda v, _: f"{int(v/1024)}KB"))
axB.xaxis.set_minor_locator(FixedLocator([]))
axB.yaxis.set_major_formatter(FuncFormatter(lambda v, _: f"{v/1e6:.0f}M" if v >= 1e6 else (f"{int(v/1000)}k" if v >= 1000 else str(int(v)))))
axB.set_xlabel("HTML page size")
axB.set_ylabel("Input Tokens")
axB.set_title("(b)  Cost vs HTML page size")
axB.grid(True, which="major", ls=":", alpha=0.4); axB.legend(loc="upper left", fontsize=9)

fig.tight_layout()

out = ARGS.out
for ext in ("png", "pdf", "svg"):
    fig.savefig(f"{out}.{ext}", bbox_inches="tight")
print("wrote", out + ".{png,pdf,svg}")
