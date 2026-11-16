#!/usr/bin/env python3
"""Marker-presence grid: which of the 5 page positions each fetcher returned, vs page
size, one panel per vendor. Unlike a "deepest marker" number, this shows the FULL
distribution — every position's fate and whether the loss is contiguous or has gaps.

Per-position presence is reconstructed properly: each stored answer is matched against
that page's OWN five codes (0, 25, 50, 75, 95%), not assumed contiguous. Cell
shade = fraction of trials (0..1) that returned that position's code.

Data: the HTML read-depth scans given (n up to 3 per size). Each page's five codes come from
a page-code table (--codes, e.g. the released data's logs/page-codes.csv) or, for your own
runs, are computed from your page-code secret ($PROBE_PAGE_CODE_SECRET). No network access.
Usage: python3 scripts/readDepth_plot_markers.py [--codes page-codes.csv] <out path without extension>
       [result files ...]     (default files: logs/results/truncprobe-run*.jsonl)
"""
import argparse, json, glob, collections, csv, hashlib, hmac, os, re, sys
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

_ap = argparse.ArgumentParser(description="Draw which depth codes each vendor returned.")
_ap.add_argument("out", nargs="?", default="read-depth-grid", help="output path without extension")
_ap.add_argument("files", nargs="*", help="result files (default logs/results/truncprobe-run*.jsonl)")
_ap.add_argument("--codes", help="page-code table (origin,page_id,version,depth,code); "
                 "default: compute the codes from $PROBE_PAGE_CODE_SECRET")
ARGS = _ap.parse_args()

# Collect the HTML scans (/depth/html/ now, /depthhtmlpage/ in older runs).
recs = []
for f in ARGS.files or glob.glob("logs/results/truncprobe-run*.jsonl"):
    for line in open(f):
        r = json.loads(line); ex = r.get("extra", {})
        u = r.get("url", "")
        if ex.get("part") != "A" or ("/depth/html/" not in u and "/depthhtmlpage/" not in u):
            continue
        recs.append((r.get("vendor", ""), ex.get("size_bytes"),
                     r.get("url", ""), r.get("answer_text") or ""))

# Each page's own five codes, in position order (0, 25, 50, 75, 95%).
PAGE_ID = re.compile(r"//([^/]+)/(?:depth/html|depthhtmlpage)/([^/?#]+)/")
TABLE = collections.defaultdict(dict)   # (origin label, page id) -> {depth: code}
if ARGS.codes:
    for row in csv.DictReader(open(ARGS.codes)):
        if row["version"] == "1":
            TABLE[(row["origin"], row["page_id"])][int(row["depth"])] = row["code"]
SECRET = os.environ.get("PROBE_PAGE_CODE_SECRET", "")
if not ARGS.codes and not SECRET:
    sys.exit("give --codes <page-codes.csv>, or set PROBE_PAGE_CODE_SECRET for your own runs")

def page_markers(url):
    m = PAGE_ID.search(url)
    if not m:
        return []
    host, pid = m.groups()
    if ARGS.codes:
        codes = TABLE.get((host.split(".")[0], pid)) or TABLE.get(("", pid)) or {}
        return [codes[p] for p in PERCENTS if p in codes]
    return ["TRUNC-" + hmac.new(SECRET.encode(), f"{pid}|d{p}|v1".encode(), hashlib.sha256).hexdigest()[:12]
            for p in PERCENTS]

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

out = ARGS.out
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
