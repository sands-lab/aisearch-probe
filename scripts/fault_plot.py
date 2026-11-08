#!/usr/bin/env python3
"""Fault token cost: a failed fetch costs the caller nearly as much input as a
successful one. One single-column ACM sigconf panel.

    python3 scripts/fault_plot.py --logs logs --out figures \
        --runs A=<run>,<run>,<run> [--runs B=<run>,...] \
        --baseline A=<fault-outFile of the flaky/0 run> [--baseline B=...]

--runs gives, per server (A, B), the fault run ids (the run id each `probe-trial -fault-run`
prints; its records are logs/results/faults-<run>.jsonl). --baseline gives, per server, the
-fault-outFile written by the success-baseline run (`-fault-kinds flaky/0`); its records pick
the successful calls. One server is enough; with two, numbers are pooled across them.
--check-paper compares the per-server medians with the table in the paper.

Reads only raw per-call records (logs/results/faults-<run>.jsonl, input_tokens) and the
baseline files. Every plotted number is a median over raw calls; nothing is smoothed.

Writes:
  <out>/fault-token-cost.pdf   the figure, exactly 3.33 x 2.1 in (no tight crop)
  <out>/fault-token-cost.png   a preview
  <out>/fault-token-cost.csv   every plotted number: pooled, Origin A and B medians, counts
and prints the checks: (with --check-paper) recomputed medians vs the paper's table, cells where the two
origins differ by more than 25% of the pooled median, the PDF page size, and the
questions behind the success and fault calls.

Vendor colours follow the paper's figure style (Anthropic = the "dynamic filtering off"
hue, since every call here ran without dynamic filtering). In this figure shape means
fault type, not vendor, and every marker is filled.
"""
import argparse
import csv
import json
import os
import re
import statistics as st
import sys

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402
from matplotlib.lines import Line2D  # noqa: E402

# --- runs -------------------------------------------------------------------
# Filled from --runs: server label (A, B) -> fault run ids.
FAULT_RUNS = {}
# Success baseline: the same fault runner and question, asked about /fault/flaky/0 (a
# page that works at once). Filled from --baseline: server label -> that run's
# -fault-outFile. Every run in the file is used; a call counts only when the model
# quoted at least one of the page's depth codes (the page was really retrieved).
BASELINE_FILE = {}

CATEGORY = {
    "404": "Error reply", "500": "Error reply", "503": "Error reply", "429": "Error reply",
    "429date": "Error reply", "403": "Error reply", "loop/a": "Redirect loop",
    "reset": "Dropped connection", "slow": "Slow page", "cut": "Cut-off page",
    "empty": "Empty page", "flaky/2": "Fails twice, then works",
}
CATS = ["Error reply", "Redirect loop", "Dropped connection", "Slow page", "Cut-off page",
        "Empty page", "Fails twice, then works"]
VENDORS = ["openai", "anthropic", "xai"]
LABEL = {"openai": "OpenAI\ngpt-5-mini", "anthropic": "Anthropic\nclaude-sonnet-5", "xai": "xAI\ngrok-4.3"}

# The paper's table (origin A, origin B), to check a recomputation to the token (--check-paper).
EXPECTED = {
    "Error reply": {"openai": (4962, 4953), "anthropic": (2937, 2928), "xai": (4122, 3294)},
    "Redirect loop": {"openai": (4977, 5394), "anthropic": (2946, 2937), "xai": (2532, 4075)},
    "Dropped connection": {"openai": (4898, 4971), "anthropic": (2940, 2931), "xai": (4321, 2553)},
    "Slow page": {"openai": (5262, 5300), "anthropic": (2932, 2931), "xai": (5533, 4142)},
    "Cut-off page": {"openai": (5216, 4978), "anthropic": (2940, 2931), "xai": (5618, 3935)},
    "Empty page": {"openai": (4970, 4953), "anthropic": (2940, 2957), "xai": (2572, 2572)},
    "Fails twice, then works": {"openai": (5735, 5684), "anthropic": (2949, 4663), "xai": (3388, 3385)},
}

# Shared paper style (see module docstring).
STYLE = {
    "openai": dict(color="#009E73", marker="o", fill=True),
    "xai": dict(color="#CC79A7", marker="^", fill=True),
    "anthropic": dict(color="#E0A000", marker="s", fill=False),
}
INK, INK2, GRID = "#1a1a1a", "#4d4d4d", "#e3e3e3"
SIZE_IN = (3.33, 2.6)
# One marker shape per fault type, the same in every vendor group.
CAT_MARKER = {
    "Error reply": "o", "Redirect loop": "s", "Dropped connection": "^", "Slow page": "v",
    "Cut-off page": "D", "Empty page": "P", "Fails twice, then works": "X",
}
MS = 4.6
# Short legend names (the full names stay in the CSV).
LEGEND_NAME = {
    "Error reply": "Error reply", "Redirect loop": "Redirect loop", "Dropped connection": "Dropped",
    "Slow page": "Slow", "Cut-off page": "Cut off", "Empty page": "Empty",
    "Fails twice, then works": "Fail ×2 then ok",
}


def vendor_of(v):
    return "anthropic" if "anthropic" in v else ("openai" if "openai" in v else "xai")


def load_jsonl(path):
    with open(path) as f:
        return [json.loads(line) for line in f if line.strip()]


def med(xs):
    return int(st.median(xs)) if xs else None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--logs", default="logs", help="folder holding results/faults-<run>.jsonl")
    ap.add_argument("--out", default="figures")
    ap.add_argument("--runs", action="append", required=True, metavar="A=RUN,RUN,...",
                    help="per server (A or B): the fault run ids")
    ap.add_argument("--baseline", action="append", default=[], metavar="A=FILE",
                    help="per server (A or B): the -fault-outFile of the success-baseline run")
    ap.add_argument("--check-paper", action="store_true", help="compare medians with the paper's table")
    a = ap.parse_args()
    res = os.path.join(a.logs, "results")
    for spec in a.runs:
        o, _, ids = spec.partition("=")
        if o not in ("A", "B") or not ids:
            sys.exit(f"bad --runs {spec!r}: want A=<run>,<run> or B=<run>,...")
        FAULT_RUNS[o] = [x for x in ids.split(",") if x]
    for spec in a.baseline:
        o, _, path = spec.partition("=")
        if o not in ("A", "B") or not path:
            sys.exit(f"bad --baseline {spec!r}: want A=<file> or B=<file>")
        BASELINE_FILE[o] = path

    calls = {}          # (category, vendor, origin) -> [input tokens]
    questions = {}      # source -> set of questions
    for origin, runs in FAULT_RUNS.items():
        for rid in runs:
            for r in load_jsonl(os.path.join(res, f"faults-{rid}.jsonl")):
                c = CATEGORY[r["extra"]["fault"]]
                calls.setdefault((c, vendor_of(r["vendor"]), origin), []).append(r["input_tokens"])
                questions.setdefault(f"fault runs, origin {origin}, {rid}", set()).add(r["question"])

    success = {}
    for origin, fname in BASELINE_FILE.items():
        if fname is None:
            continue
        path = fname
        if not os.path.exists(path):
            sys.exit(f"STOP: the success baseline file {path} is missing; nothing is drawn without it.")
        ok = {}
        for r in load_jsonl(path):
            if r.get("deepest_depth", -1) >= 0:
                ok.setdefault(r["run_id"], set()).add(r["provider"])
        for rid in sorted(ok):
            for r in load_jsonl(os.path.join(res, f"faults-{rid}.jsonl")):
                if r["vendor"] in ok[rid] or ("anthropic" in r["vendor"] and "anthropic" in ok[rid]):
                    success.setdefault((vendor_of(r["vendor"]), origin), []).append(r["input_tokens"])
                    questions.setdefault(f"success baseline, origin {origin}, {rid}", set()).add(r["question"])

    missing = [o for o in ("A", "B") if not any(k[1] == o for k in success)]
    if not success:
        sys.exit("STOP: no successful baseline calls found; the success line cannot be drawn.")

    # --- checks: recomputed per-origin medians vs the agreed table ----------------
    if a.check_paper:
        print("CHECK 1 — recomputed origin medians vs the paper's table (to the token)")
        diffs = 0
        for c in CATS:
            for v in VENDORS:
                got = (med(calls.get((c, v, "A"), [])), med(calls.get((c, v, "B"), [])))
                if got != EXPECTED[c][v]:
                    diffs += 1
                    print(f"  DIFFERS  {c} / {v}: table {EXPECTED[c][v]}, logs {got}")
        print(f"  {diffs} cell(s) differ" if diffs else "  all 42 origin medians match the table exactly")

    # --- the numbers ------------------------------------------------------------
    rows = []
    for v in VENDORS:
        sa, sb = success.get((v, "A"), []), success.get((v, "B"), [])
        rows.append(dict(vendor=v, case="Success (page retrieved)", pooled=med(sa + sb),
                         origin_a=med(sa), origin_b=med(sb), n_pooled=len(sa + sb), n_a=len(sa), n_b=len(sb)))
        for c in CATS:
            xa, xb = calls.get((c, v, "A"), []), calls.get((c, v, "B"), [])
            rows.append(dict(vendor=v, case=c, pooled=med(xa + xb), origin_a=med(xa), origin_b=med(xb),
                             n_pooled=len(xa + xb), n_a=len(xa), n_b=len(xb)))

    os.makedirs(a.out, exist_ok=True)
    csv_path = os.path.join(a.out, "fault-token-cost.csv")
    with open(csv_path, "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(rows[0]))
        w.writeheader()
        w.writerows(rows)

    print("\nSUCCESS baseline, per origin (median input tokens, calls)")
    for r in rows:
        if r["case"].startswith("Success"):
            print(f"  {r['vendor']:9} A {r['origin_a']} ({r['n_a']})   B {r['origin_b']} ({r['n_b']})   pooled {r['pooled']} ({r['n_pooled']})")
    if missing:
        print(f"  NOTE: no success baseline for origin(s) {missing}; the success line uses the other origin only.")

    print("\nCHECK 2 — cells where origin A and B differ by more than 25% of the pooled median")
    flagged = 0
    for r in rows:
        if r["origin_a"] is None or r["origin_b"] is None:
            continue
        gap = abs(r["origin_a"] - r["origin_b"])
        if gap > 0.25 * r["pooled"]:
            flagged += 1
            print(f"  {r['vendor']:9} {r['case']:24} A {r['origin_a']}  B {r['origin_b']}  pooled {r['pooled']}")
    print(f"  {flagged} cell(s)" if flagged else "  none")

    print("\nCALL COUNTS — cells with fewer than 5 calls behind the pooled median")
    for r in rows:
        if r["n_pooled"] < 5:
            print(f"  {r['vendor']:9} {r['case']:24} n={r['n_pooled']}")

    # --- figure -------------------------------------------------------------------
    plt.rcParams.update({
        "pdf.fonttype": 42, "ps.fonttype": 42, "font.family": "DejaVu Sans",
        "font.size": 7, "axes.labelsize": 7.5, "xtick.labelsize": 7, "ytick.labelsize": 7,
        "legend.fontsize": 7, "axes.linewidth": 0.6, "axes.edgecolor": INK2,
        "axes.labelcolor": INK, "xtick.color": INK2, "ytick.color": INK2, "text.color": INK,
        "axes.spines.top": False, "axes.spines.right": False,
        "xtick.major.size": 0, "ytick.major.size": 2.5, "ytick.major.width": 0.6,
        "axes.labelpad": 2, "xtick.major.pad": 2, "ytick.major.pad": 1.5,
    })
    fig = plt.figure(figsize=SIZE_IN)
    ax = fig.add_axes((0.165, 0.135, 0.825, 0.665))
    # Fixed slot per fault type, the same in every group: no random jitter.
    step = 0.1
    offsets = [step * (i - (len(CATS) - 1) / 2) for i in range(len(CATS))]
    for i, v in enumerate(VENDORS):
        s = STYLE[v]
        succ = next(r for r in rows if r["vendor"] == v and r["case"].startswith("Success"))
        ax.hlines(succ["pooled"], i - 0.38, i + 0.38, colors=s["color"], lw=1.6, zorder=2)
        for off, c in zip(offsets, CATS):
            y = next(r["pooled"] for r in rows if r["vendor"] == v and r["case"] == c)
            # Shape = fault type; colour = vendor; every marker filled.
            ax.plot(i + off, y, ls="none", marker=CAT_MARKER[c], ms=MS, zorder=3,
                    markerfacecolor=s["color"], markeredgecolor=s["color"], markeredgewidth=0.9)
    ax.set_xlim(-0.5, len(VENDORS) - 0.5)
    ax.set_xticks(range(len(VENDORS)))
    ax.set_xticklabels([LABEL[v] for v in VENDORS])
    ax.set_ylim(0, 6600)
    ax.set_yticks([0, 2000, 4000, 6000])
    ax.set_yticklabels(["0", "2,000", "4,000", "6,000"])
    ax.set_ylabel("Input tokens (median per call)")
    ax.yaxis.grid(True, color=GRID, lw=0.5)
    ax.set_axisbelow(True)
    # Legend: the seven fault shapes only (the success line is explained in the
    # caption), a grid of 4 columns x 2 rows above the plot, read row by row.
    # Matplotlib fills legend columns top-to-bottom, so the handles are interleaved.
    def shape(c):
        return Line2D([], [], ls="none", marker=CAT_MARKER[c], ms=MS, markerfacecolor=INK2,
                      markeredgecolor=INK2, label=LEGEND_NAME[c])
    entries = [shape(c) for c in CATS]
    ncol = 4
    rows_in_legend = [entries[k:k + ncol] for k in range(0, len(entries), ncol)]
    ordered = [rows_in_legend[r][col] for col in range(ncol) for r in range(len(rows_in_legend))
               if col < len(rows_in_legend[r])]
    leg = fig.legend(handles=ordered, loc="upper center", bbox_to_anchor=(0.5, 1.0), ncol=ncol,
                     frameon=False, fontsize=6.5, handlelength=1.0, handletextpad=0.35,
                     columnspacing=1.2, labelspacing=0.4, borderaxespad=0.1, borderpad=0.1,
                     markerscale=0.9)
    # The legend must fit inside the page: a clipped legend is a broken figure.
    fig.canvas.draw()
    bb = leg.get_window_extent().transformed(fig.transFigure.inverted())
    print(f"legend spans {bb.x0:.3f}-{bb.x1:.3f} of the page width")
    if bb.x0 < 0 or bb.x1 > 1:
        sys.exit("STOP: the legend is wider than the page at this size; not saving.")
    # Put the top of the plot just under the legend, so no band is wasted between them.
    p = ax.get_position()
    ax.set_position((p.x0, p.y0, p.width, bb.y0 - 0.045 - p.y0))

    pdf_path = os.path.join(a.out, "fault-token-cost.pdf")
    fig.savefig(pdf_path)                                  # exact size: no tight crop
    fig.savefig(os.path.join(a.out, "fault-token-cost.png"), dpi=300)

    data = open(pdf_path, "rb").read()
    m = re.search(rb"/MediaBox\s*\[\s*([-\d.]+)\s+([-\d.]+)\s+([-\d.]+)\s+([-\d.]+)\s*\]", data)
    x0, y0, x1, y1 = (float(g) for g in m.groups())
    print(f"\nCHECK 3 — PDF page size {(x1 - x0) / 72:.3f} x {(y1 - y0) / 72:.3f} in "
          f"(target {SIZE_IN[0]} x {SIZE_IN[1]}); Type 3 fonts present: {b'/Type3' in data}")

    print("\nQUESTIONS asked")
    for src in sorted(questions):
        for q in sorted(questions[src]):
            print(f"  {src}:\n    {q}")
    print(f"\nwrote {pdf_path}, {csv_path}")


if __name__ == "__main__":
    main()
