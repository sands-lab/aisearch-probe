#!/usr/bin/env python3
"""Plot the remembered-failure (heal) run: after a broken page is fixed, does the
AI go back and see the fix?

One figure, saved as PNG and PDF:
  heal-timeline   one mark per re-ask, placed at its ACTUAL time after the fix.
                  COLOUR + FILL = what happened at that re-ask (fetch yes/no x code
                  yes/no); SHAPE = which server served the page (circle = the first
                  --origin, triangle = the second, ...). Rows = vendor x
                  the failure the page showed before its fix; within a row, one lane
                  per sitting.

Input: the JSON written by `probe-trial -heal-report -heal-reportFile <file>`, one
file per sitting (any origin). The runner applies the rules (only requests between
sending the question and the answer count; "code" = the answer quoted one of the
fixed page's codes), so this script only draws — it never needs the page secret.

Usage: heal_plot.py --out <figure dir> [--name heal-timeline]
                    [--origin <host>=<legend label> ...] <results.json> ...
--origin names each server host found in the results and its legend label, in legend order.
Without it, every host in the results is shown under its own name.
"""
import argparse
import json
import os
from collections import Counter

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402
from matplotlib.lines import Line2D  # noqa: E402

# Reference palette, categorical slots 1-3, validated for light mode (slot 3 is below
# 3:1 contrast, so it is always legend-labelled and carries a fill). Fill + colour carry the outcome, shape
# the origin, so colour is never the only cue. Same surface/ink as correction_plot.py.
BLUE, ORANGE, AQUA = "#2a78d6", "#eb6834", "#1baf7a"
INK, INK2, MUTED, GRID, SURFACE = "#0b0b0b", "#52514e", "#8a8984", "#e6e5e0", "#fcfcfb"

# Fixed row order (Claude, ChatGPT, Grok, as every table in this study), then failure.
VENDORS = [("anthropic", "Anthropic"), ("or:openai", "OpenAI"), ("or:x-ai", "xAI")]
FAILS = [("503", "503 unavailable"), ("404", "404 not found"), ("403", "403 forbidden")]
ROWS = [(v, f, f"{vl}, {fl}") for v, vl in VENDORS for f, fl in FAILS]

# Outcome of one re-ask -> look. Hollow = the page's failure was still being reported
# with no request to us; filled = the fixed page's code was in the answer, or it came back.
OUTCOME = {
    "remembered": dict(face="none", edge=INK2, label="No new request; still said broken"),
    "went_back": dict(face=BLUE, edge=BLUE, label="New request; read the fix"),
    # A dark outline as well as the colour: shape is taken by the origin, so this is
    # the second cue that separates it from the filled blue.
    "fetched_old": dict(face=ORANGE, edge=INK, label="New request; still said broken"),
    "code_no_fetch": dict(face=AQUA, edge=AQUA, label="No new request; read the fix"),
}
# Filled in main(): server host -> (marker, legend label).
ORIGINS = {}
MARKERS = ["o", "^", "s", "D"]

# The default check grid (trial/healrun.go defaultHealChecks), as tick labels.
TICKS = [(5, "5 min"), (10, "10 min"), (20, "20 min"), (40, "40 min"), (80, "1 h 20"),
         (160, "2 h 40"), (320, "5 h 20"), (640, "10 h 40")]


def outcome(r):
    if r["call_failed"]:
        return "failed"
    if r["fetch"] and r["code"]:
        return "went_back"
    if r["fetch"]:
        return "fetched_old"
    if r["code"]:
        return "code_no_fetch"
    return "remembered"


# A page whose first answer came back before any request reached us never tested a
# remembered failure: the vendor answered before it had our failure page. Left out of
# the figure and listed on stdout.
NOT_A_TEST = "no request reached us during the first ask"


def load(paths):
    rows, left_out = [], []
    for p in paths:
        with open(p) as f:
            for r in json.load(f):
                if not r.get("asked"):
                    continue
                if any(n.startswith(NOT_A_TEST) for n in r.get("notes") or []):
                    left_out.append(r)
                else:
                    rows.append(r)
    for r in left_out:
        print(f"left out (first answer came before our failure page reached the vendor): "
              f"{r['origin']} {r['run']} {r['provider']} {r['fail']} check {r['check']}")
    return rows


def style_axes(ax):
    ax.set_facecolor(SURFACE)
    for side in ("top", "right", "left"):
        ax.spines[side].set_visible(False)
    ax.spines["bottom"].set_color(MUTED)
    ax.tick_params(colors=INK2, labelsize=9, length=0)


def save(fig, out, name, extra=()):
    # extra: artists outside the axes (the legends); the PDF crop misses them otherwise.
    for ext in ("png", "pdf"):
        fig.savefig(os.path.join(out, f"{name}.{ext}"), dpi=200, bbox_inches="tight",
                    bbox_extra_artists=list(extra), pad_inches=0.15, facecolor=SURFACE)
    plt.close(fig)


def plot_timeline(rows, out, name):
    # One lane per (origin, sitting), origin A's sittings first, each in run order.
    lanes = sorted({(r["origin"], r["run"]) for r in rows},
                   key=lambda k: (list(ORIGINS).index(k[0]) if k[0] in ORIGINS else 9, k[1]))
    step = 0.62 / max(len(lanes), 1)
    lane_y = {k: ((len(lanes) - 1) / 2 - i) * step for i, k in enumerate(lanes)}  # first lane on top
    y_of = {(v, f): len(ROWS) - 1 - i for i, (v, f, _) in enumerate(ROWS)}

    fig, ax = plt.subplots(figsize=(10, 7.2))
    fig.patch.set_facecolor(SURFACE)
    style_axes(ax)
    for kind in ("remembered", "failed", "went_back", "fetched_old", "code_no_fetch"):  # highlights last
        for origin, (marker, _) in ORIGINS.items():
            pts = [r for r in rows if outcome(r) == kind and r["origin"] == origin and (r["provider"], r["fail"]) in y_of]
            if not pts:
                continue
            xs = [r["waited_s"] / 60 for r in pts]
            ys = [y_of[(r["provider"], r["fail"])] + lane_y[(r["origin"], r["run"])] for r in pts]
            if kind == "failed":
                ax.scatter(xs, ys, marker="x", s=30, color=MUTED, linewidths=1.2, zorder=3)
            else:
                o = OUTCOME[kind]
                ax.scatter(xs, ys, marker=marker, s=50, facecolors=o["face"], edgecolors=o["edge"],
                           linewidths=1.3, zorder=3)
    ax.set_xscale("log")
    ax.set_xticks([t for t, _ in TICKS])
    ax.set_xticklabels([l for _, l in TICKS])
    ax.minorticks_off()
    ax.set_xlim(3, 1000)
    ax.set_yticks(range(len(ROWS)))
    ax.set_yticklabels([lab for _, _, lab in reversed(ROWS)], color=INK, fontsize=10)
    ax.set_ylim(-0.6, len(ROWS) - 0.4)
    ax.grid(axis="x", color=GRID, linewidth=0.8, zorder=0)
    for y in (2.5, 5.5):  # between vendors
        ax.axhline(y, color=INK2, linewidth=1.2, zorder=1)
    ax.set_xlabel("Time since the fix (log scale)", color=INK2, fontsize=10)

    # Two legend groups side by side, a thin rule between: what happened | which origin.
    present = Counter(outcome(r) for r in rows)
    what = [Line2D([], [], linestyle="none", marker="o", markersize=8, markeredgewidth=1.3,
                   markerfacecolor=OUTCOME[k]["face"], markeredgecolor=OUTCOME[k]["edge"],
                   label=OUTCOME[k]["label"]) for k in OUTCOME if present[k]]
    if present["failed"]:
        what.append(Line2D([], [], linestyle="none", marker="x", markersize=6, color=MUTED,
                           label="Question failed"))
    # Solid grey: a shape-only swatch, so it is never read as an outcome.
    where = [Line2D([], [], linestyle="none", marker=marker, markersize=8,
                    markerfacecolor=MUTED, markeredgecolor=MUTED, label=label)
             for origin, (marker, label) in ORIGINS.items() if any(r["origin"] == origin for r in rows)]
    top, split = -0.1, 0.72
    kw = dict(frameon=False, fontsize=9, labelcolor=INK2, borderaxespad=0)
    leg_what = ax.add_artist(ax.legend(handles=what, loc="upper right", bbox_to_anchor=(split - 0.03, top), ncol=2, **kw))
    leg_where = ax.legend(handles=where, loc="upper left", bbox_to_anchor=(split + 0.03, top), **kw)
    rows_shown = max((len(what) + 1) // 2, len(where))
    ax.plot([split, split], [top, top - 0.035 * rows_shown], transform=ax.transAxes,
            color=MUTED, linewidth=0.8, clip_on=False)
    save(fig, out, name, extra=(leg_what, leg_where))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--name", default="heal-timeline")
    ap.add_argument("--origin", action="append", default=[], metavar="HOST=LABEL",
                    help="a server host in the results and its legend label, in legend order")
    ap.add_argument("results", nargs="+")
    args = ap.parse_args()
    rows = load(args.results)
    for spec in args.origin:
        host, _, label = spec.partition("=")
        ORIGINS[host] = (MARKERS[len(ORIGINS) % len(MARKERS)], label or host)
    for host in sorted({r["origin"] for r in rows} - set(ORIGINS)):
        ORIGINS[host] = (MARKERS[len(ORIGINS) % len(MARKERS)], host)
    plot_timeline(rows, args.out, args.name)
    counts = Counter((r["origin"], r["provider"], r["fail"], outcome(r)) for r in rows)
    for k in sorted(counts):
        print(k, counts[k])


if __name__ == "__main__":
    main()
