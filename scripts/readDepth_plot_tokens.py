#!/usr/bin/env python3
"""3-vendor read-window plot: input tokens (page content fed to the model) vs page size.
One mean line per vendor, pooled across the HTML read-depth runs given (n up to 3 per size).

Usage: readDepth_plot_tokens.py <out path without extension> [--log] [result files ...]
       (default files: logs/results/truncprobe-run*.jsonl; --log draws a log y axis)

What the runs showed:
  - OpenAI  — SUMMARIZES: reads the whole page but caps input ~65k (cheap, complete).
  - Anthropic — INGESTS: input rises ~1:1 with size (linear, expensive, no cap to 2 MB).
  - xAI — TRUNCATES at a fixed ~500 KB window: flat input, and read depth falls past it
          (1 MB -> 50%, 2 MB -> 25%).
"""
import argparse, json, collections, sys, glob

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.ticker import FuncFormatter, LogLocator, NullFormatter

# vendor field in records -> (label, colour, marker)
VENDORS = {
    "or:openai":    ("OpenAI  gpt-5-mini",         "#10a37f", "o"),
    "or:anthropic": ("Anthropic  Claude sonnet-5", "#d97757", "s"),
    "or:x-ai":      ("xAI  Grok-4.3",              "#5b6ee1", "^"),
}

_ap = argparse.ArgumentParser(description="Draw input tokens against page size.")
_ap.add_argument("out", nargs="?", default="readwindow", help="output path without extension")
_ap.add_argument("--log", action="store_true", help="log y axis")
_ap.add_argument("files", nargs="*", help="result files (default logs/results/truncprobe-run*.jsonl)")
ARGS = _ap.parse_args()

def is_html_depth(url):
    """The HTML depth page: /depth/html/ now, /depthhtmlpage/ in older runs."""
    return "/depth/html/" in url or "/depthhtmlpage/" in url

def pooled_mean_by_size():
    """Pool every html scan -> {vendor: {size: mean_input}}."""
    acc = collections.defaultdict(lambda: collections.defaultdict(list))
    for path in ARGS.files or glob.glob("logs/results/truncprobe-run*.jsonl"):
        for line in open(path):
            r = json.loads(line); ex = r.get("extra", {})
            if ex.get("part") != "A" or not is_html_depth(r.get("url", "")):
                continue
            it, sz, v = r.get("input_tokens"), ex.get("size_bytes"), r.get("vendor", "")
            if it and sz and v in VENDORS:
                acc[v][sz].append(it)
    return {v: {sz: sum(xs)/len(xs) for sz, xs in d.items()} for v, d in acc.items()}

data = pooled_mean_by_size()

fig, ax = plt.subplots(figsize=(8.5, 5.2), dpi=200)
for v, (label, color, marker) in VENDORS.items():
    d = data.get(v, {})
    if not d:
        ax.plot([], [], color=color, marker=marker, label=label + "  (pending)", lw=2)
        continue
    xs = sorted(d)
    ys = [d[x] / 1000.0 for x in xs]  # in units of 1,000 tokens
    ax.plot(xs, ys, color=color, marker=marker, ms=7, lw=2.2, label=label)

# The only measured read CUT in range: xAI stops reading past ~500 KB (read depth falls).
# OpenAI and Anthropic read in full all the way to 2 MB, so they have no cut line.
ax.axvline(512 << 10, color="#5b6ee1", ls="--", lw=1.4, alpha=0.75)
ax.text(512 << 10, 0.97, "  xAI read cut ~500 KB (reads 50% at 1 MB, 25% at 2 MB)",
        rotation=90, color="#5b6ee1", fontsize=9, ha="right", va="top",
        transform=ax.get_xaxis_transform(),
        bbox=dict(facecolor="white", edgecolor="none", alpha=0.65, pad=1.2))

ax.set_xscale("log")
ticks = [4 << 10, 16 << 10, 64 << 10, 256 << 10, 1 << 20, 2 << 20]
def fmt(v, _):
    return f"{v // (1 << 20)} MB" if v >= 1 << 20 else f"{v // (1 << 10)} KB"
ax.set_xticks(ticks)
ax.xaxis.set_major_formatter(FuncFormatter(fmt))
ax.xaxis.set_minor_locator(LogLocator(base=10.0, subs=tuple(range(2, 10)), numticks=200))
ax.xaxis.set_minor_formatter(NullFormatter())
ax.set_xlabel("Page size")
ax.set_ylabel("Input tokens (k)")
ax.set_title("Input tokens vs. HTML page size, by service")
ax.grid(True, which="major", ls=":", alpha=0.5)
ax.grid(True, which="minor", ls=":", alpha=0.2)
ax.legend(frameon=False, loc="upper left")
ax.margins(y=0.12)
fig.tight_layout()

if ARGS.log:
    ax.set_yscale("log")

out = ARGS.out
for ext in ("png", "svg", "pdf"):
    fig.savefig(out + "." + ext, bbox_inches="tight")
print("wrote", out, "(.png/.svg/.pdf)")
for v, (label, _, _) in VENDORS.items():
    d = data.get(v, {})
    print(f"  {label}: {len(d)} sizes, {sorted(s//1024 for s in d)} KB")
