#!/usr/bin/env python3
"""Plot the correction-visibility runs (doc/correction-visibility-method.md).

Two figures, each saved as PNG and PDF:
  correction-timeline   one panel per origin; one mark per follow-up query, placed at
                        its ACTUAL time after the page update; colour + shape = what
                        the answer showed; one thin lane per run, labelled with its prompt
  correction-fetched-but-old
                        one panel per (origin, prompt): of the follow-up queries during
                        which our server sent the updated page, how many answers still
                        showed the old one

Outcomes are computed here from the runner's state files and the ORIGIN'S OWN log, with
the same rules as `probe-trial -correction-report`: only requests that arrived between
sending the question and the answer coming back count, and the version a request got is
read from the fingerprint tag we sent. Origin A's log is logs/probe.jsonl; origin B's is
logs/probe-vps.jsonl (a copy of the VPS log; read after the run, so its lag cannot matter).

Usage: correction_plot.py --logs <dir with the logs and correction-*.state.json>
                          --out <figure dir> A:s1:old A:s2:old ... B:vps2:new
Each run is ORIGIN:RUN:PROMPT; ORIGIN is A or B, PROMPT is old (wrapper) or new
(address + question only).
"""
import argparse
import json
import os
from collections import defaultdict

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402
from matplotlib.lines import Line2D  # noqa: E402

# Reference palette (dataviz skill), first two categorical slots; validated
# all-pairs, light surface. Shape carries the same meaning, so colour is never alone.
BLUE, ORANGE = "#2a78d6", "#eb6834"
INK, INK2, MUTED, GRID, SURFACE = "#0b0b0b", "#52514e", "#8a8984", "#e6e5e0", "#fcfcfb"

ORIGIN_LOG = {"A": "probe.jsonl", "B": "probe-vps.jsonl"}
ORIGIN_NAME = {"A": "Origin A (behind Cloudflare)", "B": "Origin B (direct, no CDN)"}
PROMPT_NAME = {"old": "old prompt", "new": "new prompt"}

ROWS = [("or:openai", "html", "OpenAI (HTML)"), ("or:openai", "txt", "OpenAI (txt)"),
        ("anthropic", "html", "Anthropic (HTML)"), ("anthropic", "txt", "Anthropic (txt)"),
        ("or:x-ai", "html", "xAI (HTML)"), ("or:x-ai", "txt", "xAI (txt)")]

STYLE = {
    "old": dict(marker="o", s=40, facecolors="none", edgecolors=INK2, linewidths=1.2,
                label="Old answer; no new origin request"),
    "corrected": dict(marker="o", s=40, facecolors=BLUE, edgecolors=SURFACE, linewidths=1.0,
                      label="Updated answer; new origin request"),
    "new_but_old": dict(marker="D", s=50, facecolors=ORANGE, edgecolors=SURFACE, linewidths=1.0,
                        label="Old answer despite new origin request"),
    "failed": dict(marker="x", s=26, color=MUTED, linewidths=1.2, label="Query failed (excluded)"),
}


def parse_run(spec):
    origin, run, prompt = spec.split(":")
    if origin not in ORIGIN_LOG or prompt not in PROMPT_NAME:
        raise SystemExit(f"bad run {spec!r}: want A|B:<run>:old|new")
    return dict(origin=origin, run=run, prompt=prompt)


def load_fetches(log_path, paths):
    out = defaultdict(list)
    with open(log_path) as f:
        for line in f:
            if "/verhtmlpage/" not in line and "/vertxtpage/" not in line:
                continue
            try:
                r = json.loads(line)
            except ValueError:
                continue
            if r.get("page_key") in paths and not (r.get("bot") or "").startswith("probe-"):
                out[r["page_key"]].append(r)
    return out


def outcomes(log_dir, runs):
    rows = []
    for origin in ("A", "B"):
        mine = [r for r in runs if r["origin"] == origin]
        if not mine:
            continue
        states = {r["run"]: json.load(open(os.path.join(log_dir, f"correction-{r['run']}.state.json"))) for r in mine}
        paths = {p["path"] for st in states.values() for p in st["pages"]}
        fetches = load_fetches(os.path.join(log_dir, ORIGIN_LOG[origin]), paths)
        for run in mine:
            for p in states[run["run"]]["pages"]:
                if p["status"] == "void":
                    continue
                for a in p["reasks"]:
                    if a.get("status") != "done":
                        continue
                    wait_h = (a.get("waited_s") or (a["sent_unix"] - p["flip_unix"])) / 3600
                    during = [r for r in fetches[p["path"]] if a["sent_unix"] <= r["ts_unix"] <= a["done_unix"]]
                    got_b = any(r["status_code"] == 200 and r["etag_sent"] == p["etag_b"] for r in during)
                    v = a.get("version")
                    if a.get("err") or v in (None, "neither", "both"):
                        kind = "failed"
                    elif v == "B":
                        kind = "corrected"
                    elif got_b:
                        kind = "new_but_old"
                    else:
                        kind = "old"
                    rows.append(dict(origin=origin, run=run["run"], prompt=run["prompt"], vendor=p["provider"],
                                     fmt=p["format"], wait_h=wait_h, kind=kind, got_b=got_b))
    return rows


def style_axes(ax):
    ax.set_facecolor(SURFACE)
    for side in ("top", "right", "left"):
        ax.spines[side].set_visible(False)
    ax.spines["bottom"].set_color(MUTED)
    ax.tick_params(colors=INK2, labelsize=9, length=0)


def save(fig, out, name):
    for ext in ("png", "pdf"):
        fig.savefig(os.path.join(out, f"{name}.{ext}"), dpi=200, bbox_inches="tight", facecolor=SURFACE)
    plt.close(fig)


def legend_handles(kinds):
    return [Line2D([], [], linestyle="none", markersize=8 if k != "failed" else 6, marker=STYLE[k]["marker"],
                   markerfacecolor=STYLE[k].get("facecolors", "none") if k != "failed" else MUTED,
                   markeredgecolor=STYLE[k].get("edgecolors", STYLE[k].get("color")),
                   label=STYLE[k]["label"]) for k in kinds]


def plot_timeline(rows, runs, out):
    origins = [o for o in ("A", "B") if any(r["origin"] == o for r in runs)]
    fig, axes = plt.subplots(1, len(origins), figsize=(7.2 * len(origins), 5.8), sharey=True, squeeze=False)
    fig.patch.set_facecolor(SURFACE)
    y_of = {(v, f): len(ROWS) - 1 - i for i, (v, f, _) in enumerate(ROWS)}
    ticks = [(10 / 60, "10 min"), (0.5, "30 min"), (1, "1 h"), (3, "3 h"), (10, "10 h"), (24, "1 day"), (72, "3 days")]
    for ax, origin in zip(axes[0], origins):
        style_axes(ax)
        mine = [r for r in runs if r["origin"] == origin]
        lane = {r["run"]: ((len(mine) - 1) / 2 - i) * 0.22 for i, r in enumerate(mine)}  # first run on top
        pts_all = [r for r in rows if r["origin"] == origin]
        for kind in ("old", "failed", "corrected", "new_but_old"):  # highlights drawn last
            pts = [r for r in pts_all if r["kind"] == kind]
            if pts:
                ax.scatter([r["wait_h"] for r in pts], [y_of[(r["vendor"], r["fmt"])] + lane[r["run"]] for r in pts],
                           zorder=3, **{k: v for k, v in STYLE[kind].items() if k != "label"})
        ax.set_xscale("log")
        ax.set_xticks([t for t, _ in ticks])
        ax.set_xticklabels([l for _, l in ticks])
        ax.set_xlim(8 / 60, 110)
        ax.set_ylim(-0.6, len(ROWS) - 0.4)
        ax.grid(axis="x", color=GRID, linewidth=0.8, zorder=0)
        for y in (1.5, 3.5):
            ax.axhline(y, color=INK2, linewidth=1.2, zorder=1)
        ax.set_title(ORIGIN_NAME[origin], loc="left", color=INK, fontsize=10, fontweight="bold", pad=6)
        # Lane key, right of the top row: which run, which prompt.
        top = len(ROWS) - 1
        for i, r in enumerate(mine):
            ax.text(1.01, (top + lane[r["run"]] + 0.6) / len(ROWS), f"run {i + 1} · {PROMPT_NAME[r['prompt']]}",
                    transform=ax.transAxes, va="center", ha="left", color=INK2, fontsize=7.5)
        ax.set_xlabel("Time from page update to follow-up query (log scale)", color=INK2, fontsize=9.5)
    axes[0][0].set_yticks(range(len(ROWS)))
    axes[0][0].set_yticklabels([lab for _, _, lab in reversed(ROWS)], color=INK, fontsize=10)
    fig.legend(handles=legend_handles(("old", "corrected", "new_but_old", "failed")), loc="lower center",
               bbox_to_anchor=(0.5, -0.06), ncol=4, frameon=False, fontsize=9, labelcolor=INK2)
    fig.subplots_adjust(wspace=0.32)
    save(fig, out, "correction-timeline")


def plot_fetched_but_old(rows, runs, out):
    groups = []
    for origin in ("A", "B"):
        for prompt in ("old", "new"):
            names = [r["run"] for r in runs if r["origin"] == origin and r["prompt"] == prompt]
            if names:
                groups.append((origin, prompt, len(names)))
    fig, axes = plt.subplots(1, len(groups), figsize=(4.4 * len(groups), 3.8), sharey=True, squeeze=False)
    fig.patch.set_facecolor(SURFACE)
    xmax = 0
    for origin, prompt, _ in groups:
        for v, f, _ in ROWS:
            xmax = max(xmax, sum(1 for r in rows if r["origin"] == origin and r["prompt"] == prompt and r["vendor"] == v
                                 and r["fmt"] == f and r["got_b"] and r["kind"] != "failed"))
    for ax, (origin, prompt, n) in zip(axes[0], groups):
        style_axes(ax)
        for i, (v, f, _) in enumerate(ROWS):
            y = len(ROWS) - 1 - i
            got = [r for r in rows if r["origin"] == origin and r["prompt"] == prompt and r["vendor"] == v
                   and r["fmt"] == f and r["got_b"] and r["kind"] != "failed"]
            good = sum(r["kind"] == "corrected" for r in got)
            bad = sum(r["kind"] == "new_but_old" for r in got)
            if not got:
                ax.text(0.3, y, "no new origin request", va="center", color=MUTED, fontsize=8,
                        bbox=dict(facecolor=SURFACE, edgecolor="none", pad=1))
                continue
            ax.barh(y, good, height=0.5, color=BLUE, edgecolor=SURFACE, linewidth=2, zorder=3)
            if bad:
                ax.barh(y, bad, left=good, height=0.5, color=ORANGE, edgecolor=SURFACE, linewidth=2, zorder=3)
            ax.text(good + bad + 0.5, y, f"{bad} of {good + bad}", va="center", color=INK2, fontsize=8.5,
                    bbox=dict(facecolor=SURFACE, edgecolor="none", pad=1))
        ax.set_xlim(0, xmax * 1.3 + 2)
        ax.grid(axis="x", color=GRID, linewidth=0.8, zorder=0)
        runs_word = "run" if n == 1 else "runs"
        ax.set_title(f"{ORIGIN_NAME[origin].split(' (')[0]} · {PROMPT_NAME[prompt]} ({n} {runs_word})",
                     loc="left", color=INK, fontsize=10, fontweight="bold", pad=6)
        ax.set_xlabel("Queries with a new origin request", color=INK2, fontsize=9)
    axes[0][0].set_yticks(range(len(ROWS)))
    axes[0][0].set_yticklabels([lab for _, _, lab in reversed(ROWS)], color=INK, fontsize=10)
    handles = [Line2D([], [], linestyle="none", marker="s", markersize=9, markerfacecolor=BLUE, markeredgecolor=BLUE,
                      label="Updated answer"),
               Line2D([], [], linestyle="none", marker="s", markersize=9, markerfacecolor=ORANGE,
                      markeredgecolor=ORANGE, label="Old answer despite new origin request (count shown)")]
    fig.legend(handles=handles, loc="lower center", bbox_to_anchor=(0.5, -0.1), ncol=2, frameon=False,
               fontsize=9, labelcolor=INK2)
    fig.subplots_adjust(wspace=0.12)
    save(fig, out, "correction-fetched-but-old")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--logs", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("runs", nargs="+", help="ORIGIN:RUN:PROMPT, e.g. A:s1:old B:vps2:new")
    args = ap.parse_args()
    runs = [parse_run(s) for s in args.runs]
    rows = outcomes(args.logs, runs)
    plot_timeline(rows, runs, args.out)
    plot_fetched_but_old(rows, runs, args.out)
    counts = defaultdict(int)
    for r in rows:
        counts[(r["origin"], r["prompt"], r["vendor"], r["fmt"], r["kind"])] += 1
    for k in sorted(counts):
        print(k, counts[k])


if __name__ == "__main__":
    main()
