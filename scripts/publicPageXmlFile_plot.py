#!/usr/bin/env python3
"""Compact versions of the public XML file test figure, for the paper (spec from 2026-10-28):
  grid     rows = declared file type, columns = service, each spot a small 100% bar
  heatmap  two small heatmaps, Refused % and Read %, one shared colour scale
Four outcomes only, each a plain sum of the scored outcomes in publicPageXmlFile_run.score:
  Read = read; Partial = header lines only + fetched as one line; Refused = refused;
  Other = inconclusive + failed.
RSS and the single Atom file share one row ("RSS or Atom"). Same scoring and answer keys as
plot-public-filetest.py; nothing in the data changes. Paper style: 7 pt text, fonts embedded,
service order Anthropic, OpenAI, xAI, then Google; outcome colours are the paper's good (blue) /
bad (red) family and never a service hue.
Saves PDF + PNG.
Usage: publicPageXmlFile_plot.py <answers.jsonl> <keys1.jsonl[,keys2.jsonl]> <out-prefix>
"""
import sys, os, json, collections
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.colors import LinearSegmentedColormap
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import publicPageXmlFile_run as ft

INK, INK2 = "#1a1a1a", "#4d4d4d"
OUTCOMES = [   # label, scored outcomes summed, colour, number colour
    ("Read", ("read",), "#0B5FA5", "white"),
    ("Partial", ("boilerplate only", "fetched, one line"), "#9CC8EA", INK),
    ("Refused", ("refused",), "#B2182B", "white"),
    ("Other", ("inconclusive", "failed"), "#c9c9c9", INK),
]
TYPES = [("application/xml", ("application/xml",)), ("text/xml", ("text/xml",)),
         ("RSS or Atom", ("application/rss+xml", "application/atom+xml")),
         ("text/plain (llms.txt)", ("llms.txt as text/plain",))]
VENDORS = [("anthropic", "Anthropic", "claude-sonnet-5"), ("openai", "OpenAI", "gpt-5-mini"),
           ("xai", "xAI", "grok-4.3"), ("google", "Google", "gemini-3.1-flash-lite")]
MIN_LABEL = 9.0          # print a number only in segments above this share (%); the text cites 14-19% refusals

def style():
    plt.rcParams.update({"pdf.fonttype": 42, "ps.fonttype": 42, "font.family": "DejaVu Sans",
                         "font.size": 7, "xtick.labelsize": 6.5, "ytick.labelsize": 6.5,
                         "text.color": INK, "savefig.facecolor": "white", "figure.facecolor": "white"})

def shares():
    """-> {(vendor, type label): ({outcome label: share %}, file count)}"""
    ans_p, key_ps = sys.argv[1], sys.argv[2].split(",")
    keys = collections.defaultdict(list)
    for p in key_ps:
        for k in (json.loads(l) for l in open(p)):
            if k.get("key"): keys[k["url"]].append(k["key"])
    raw = collections.defaultdict(collections.Counter)
    for a in (json.loads(l) for l in open(ans_p)):
        raw[(a["vendor"], a["group"])][ft.score(a["answer"], a["error"], keys.get(a["url"], []), a["url"])] += 1
    out = {}
    for v, _, _ in VENDORS:
        for t, groups in TYPES:
            c = collections.Counter()
            for g in groups: c.update(raw[(v, g)])
            n = sum(c.values())
            out[(v, t)] = ({lab: 100.0 * sum(c[k] for k in ks) / n for lab, ks, _, _ in OUTCOMES}, n)
    return out

def type_label(t, s):
    return f"{t} ({s[(VENDORS[0][0], t)][1]})"     # same file count for every service (checked in main)

def vendor_heads(fig, ax, xs, y):
    """Service name, and the model on a smaller second line, centred over each column."""
    from matplotlib.transforms import offset_copy
    wrap = {}                    # one line each; a smaller font keeps them inside their column
    for x, (_, name, model) in zip(xs, VENDORS):
        m = wrap.get(model, model)   # the long names wrap, or they run into the next column
        # names on one line; each model hangs just below its name (room for two lines)
        top = offset_copy(ax.transAxes, fig=fig, y=5.2, units="points")
        ax.text(x, y, m, ha="center", va="top", fontsize=4.6, color=INK2, linespacing=0.95, transform=top)
        up = offset_copy(ax.transAxes, fig=fig, y=5.7, units="points")
        ax.text(x, y, name, ha="center", va="bottom", fontsize=7, fontweight="bold", transform=up)

def legend(fig, y):
    hs = [plt.Rectangle((0, 0), 1, 1, facecolor=c, edgecolor="none") for _, _, c, _ in OUTCOMES]
    fig.legend(hs, [o[0] for o in OUTCOMES], loc="lower center", ncol=4, frameon=False, fontsize=7,
               bbox_to_anchor=(0.5, y), handlelength=1.0, handletextpad=0.4, columnspacing=1.2, borderaxespad=0)

def grid(s, out):
    style()
    fig = plt.figure(figsize=(3.2, 2.1))
    L, R, B, T = 0.22, 0.995, 0.02, 0.74          # plot area as figure fractions
    ax = fig.add_axes((L, B, R - L, T - B)); ax.set_axis_off()
    ax.set_xlim(0, len(VENDORS)); ax.set_ylim(len(TYPES), 0)
    gap, h = 0.035, 0.80
    for j, (v, _, _) in enumerate(VENDORS):
        for i, (t, _) in enumerate(TYPES):
            sh, _ = s[(v, t)]; left = j + gap / 2; width = 1 - gap
            for lab, _, col, ink in OUTCOMES:
                w = width * sh[lab] / 100
                if w <= 0: continue
                ax.add_patch(plt.Rectangle((left, i + (1 - h) / 2), w, h, facecolor=col, edgecolor="white", linewidth=0.5))
                if sh[lab] > MIN_LABEL:
                    ax.text(left + w / 2, i + 0.5, f"{sh[lab]:.0f}", ha="center", va="center", fontsize=6 if sh[lab] >= 25 else 5.5, color=ink)
                left += w
    for i, (t, _) in enumerate(TYPES):
        ax.text(-0.05, i + 0.5, t.replace("text/plain (llms.txt)", "text/plain\n(llms.txt)") + f"\n({s[(VENDORS[0][0], t)][1]})", ha="right", linespacing=0.95, va="center", fontsize=6.5, color=INK2)
    vendor_heads(fig, ax, [(j + 0.5) / len(VENDORS) for j in range(len(VENDORS))], 1.02)
    legend(fig, 0.885)
    save(fig, out + "-grid")

def heatmap(s, out):
    style()
    fig = plt.figure(figsize=(3.0, 2.55))
    cmap = LinearSegmentedColormap.from_list("one", ["#f4f4f4", "#3a3a3a"])   # one scale, both maps
    nv, nt = len(VENDORS), len(TYPES)
    # stacked, so the service names print once across the full width (side by side they collided)
    boxes = [(0.30, 0.47, 0.69, 0.36), (0.30, 0.02, 0.69, 0.36)]
    for k, (box, lab) in enumerate(zip(boxes, ("Refused", "Read"))):
        ax = fig.add_axes(box)
        m = [[s[(v, t)][0][lab] for v, _, _ in VENDORS] for t, _ in TYPES]
        ax.imshow(m, cmap=cmap, vmin=0, vmax=100, aspect="auto")
        for i in range(nt):
            for j in range(nv):
                val = m[i][j]
                ax.text(j, i, f"{val:.0f}", ha="center", va="center", fontsize=6.5,
                        color="white" if val > 55 else INK)
        ax.set_xticks([]); ax.set_yticks(range(nt))
        ax.set_yticklabels([type_label(t, s) for t, _ in TYPES])
        ax.tick_params(length=0, pad=2, colors=INK2)
        for sp in ax.spines.values(): sp.set_visible(False)
        ax.set_xticks([x - 0.5 for x in range(1, nv)], minor=True); ax.set_yticks([y - 0.5 for y in range(1, nt)], minor=True)
        ax.grid(which="minor", color="white", linewidth=1.0); ax.tick_params(which="minor", length=0)
        ax.text(-0.02, 1.03, f"{lab} (%)", ha="right", va="bottom", fontsize=7, fontweight="bold", transform=ax.transAxes)
        if k == 0: vendor_heads(fig, ax, [(j + 0.5) / nv for j in range(nv)], 1.03)
    save(fig, out + "-heatmap")

def save(fig, name):
    for ext in ("pdf", "png"):
        fig.savefig(f"{name}.{ext}", bbox_inches="tight", pad_inches=0.02, dpi=300 if ext == "png" else None)
    w, h = fig.get_size_inches()
    print(f"saved {name}.{{pdf,png}}  (set at {w:.2f} x {h:.2f} in before the tight crop)")

def main():
    s = shares()
    for t, _ in TYPES:     # the file count must be the same for every service, or the row label lies
        assert len({s[(v, t)][1] for v, _, _ in VENDORS}) == 1, t
    total = sum(s[(VENDORS[0][0], t)][1] for t, _ in TYPES)
    print("files per row:", {t: s[(VENDORS[0][0], t)][1] for t, _ in TYPES}, "total", total)
    for v, name, _ in VENDORS:
        print(name, {t: {k: round(x, 1) for k, x in s[(v, t)][0].items()} for t, _ in TYPES})
    grid(s, sys.argv[3]); heatmap(s, sys.argv[3])

if __name__ == "__main__": main()
