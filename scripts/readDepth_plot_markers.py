#!/usr/bin/env python3
"""Marker-presence grid: which of the 5 page positions each fetcher returned, vs page
size, one panel per vendor. Unlike a "deepest marker" number, this shows the FULL
distribution — every position's fate and whether the loss is contiguous or has gaps.

Per-position presence is reconstructed properly: each stored answer is matched against
that page's OWN five ordered markers (re-fetched by id), not assumed contiguous. Cell
shade = fraction of trials (0..1) that returned that position's code.

Data: clean /depthhtmlpage/ Part-A scans, pooled 5-7 Oct (n up to 3 per size).
Usage: python3 scripts/readDepth_plot_markers.py doc/figures/read-depth-grid
"""
import json, glob, collections, subprocess, re, sys
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np

PERCENTS = [0, 25, 50, 75, 95]
VENDORS = [("Claude  sonnet-5", "or:anthropic"),
           ("ChatGPT  gpt-5-mini", "or:openai"),
           ("Grok-4.3", "or:x-ai")]
SIZES = [4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152]

def size_label(s):
    return f"{s // 1024}k" if s < 1 << 20 else f"{s // (1 << 20)}M"

# Collect clean html Part-A records.
recs = []
for f in glob.glob("logs/results/truncprobe-run2026100[567]-*.jsonl"):
    for line in open(f):
        r = json.loads(line); ex = r.get("extra", {})
        if ex.get("part") != "A" or "/depthhtmlpage/" not in r.get("url", ""):
            continue
        recs.append((r.get("vendor", ""), ex.get("size_bytes"),
                     r.get("url", ""), r.get("answer_text") or ""))

# Each page's own five ordered markers (0,25,50,75,95% by position), re-fetched by id.
_cache = {}
def page_markers(url):
    if url not in _cache:
        out = subprocess.run(["curl", "-sS", url], capture_output=True, text=True).stdout
        seen = []
        for c in re.findall(r"TRUNC-[0-9a-f]{6,}", out):
            if c not in seen:
                seen.append(c)
        _cache[url] = seen[:5]
    return _cache[url]

# (vendor, size, position-index) -> list of 0/1 (returned?) across trials.
agg = collections.defaultdict(list)
for vendor, size, url, ans in recs:
    mk = page_markers(url)
    if len(mk) < 5:
        continue
    for i, code in enumerate(mk):
        agg[(vendor, size, i)].append(1 if code in ans else 0)

# Binary outcome (every cell is all-trials or no-trials): filled = returned, open = not.
# A partial cell would be flagged with "k/n" text so it is never hidden.
RETURNED, ABSENT = "#1b7837", "#f2f2f2"
from matplotlib.patches import Rectangle, Patch
fig, axes = plt.subplots(1, 3, figsize=(13, 3.8), dpi=200, sharey=True)
for ax, (label, v) in zip(axes, VENDORS):
    ax.set_xlim(-0.5, len(SIZES) - 0.5); ax.set_ylim(4.5, -0.5)
    for ri in range(5):
        for ci, s in enumerate(SIZES):
            vals = agg.get((v, s, ri))
            if not vals:
                continue
            frac = sum(vals) / len(vals)
            filled = frac >= 0.999
            ax.add_patch(Rectangle((ci - 0.45, ri - 0.45), 0.9, 0.9,
                                   facecolor=RETURNED if filled else ABSENT,
                                   edgecolor="#cccccc", lw=0.6))
            if not filled:
                ax.text(ci, ri, "✗" if frac == 0 else f"{sum(vals)}/{len(vals)}",
                        ha="center", va="center", fontsize=8,
                        color="#b00" if frac == 0 else "#333")
    ax.set_title(label, fontsize=11)
    ax.set_xticks(range(len(SIZES)))
    ax.set_xticklabels([size_label(s) for s in SIZES], rotation=45, ha="right", fontsize=8)
    ax.set_yticks(range(5)); ax.set_yticklabels([f"{p}%" for p in PERCENTS])
    ax.set_xlabel("page size")
    ax.set_aspect("auto"); ax.tick_params(length=0)
    for sp in ax.spines.values():
        sp.set_visible(False)
axes[0].set_ylabel("marker position (0% = page start, 95% = near end)")
fig.suptitle("Which page positions each AI fetcher returned  (n=3 per cell)", fontsize=12, y=1.04)
fig.subplots_adjust(bottom=0.26)
fig.legend(handles=[Patch(facecolor=RETURNED, edgecolor="#cccccc", label="returned (all trials)"),
                    Patch(facecolor=ABSENT, edgecolor="#cccccc", label="not returned")],
           loc="lower center", ncol=2, frameon=False, bbox_to_anchor=(0.5, -0.02))

out = sys.argv[1] if len(sys.argv) > 1 else "read-depth-grid"
for ext in ("png", "svg", "pdf"):
    fig.savefig(out + "." + ext, bbox_inches="tight")
print("wrote", out, "(.png/.svg/.pdf)")
# text summary
for label, v in VENDORS:
    rowtxt = []
    for s in SIZES:
        got = sum(1 for ri in range(5) if agg.get((v, s, ri)) and sum(agg[(v, s, ri)]) == len(agg[(v, s, ri)]))
        rowtxt.append(f"{size_label(s)}:{got}/5")
    print(f"  {label}: " + "  ".join(rowtxt))
