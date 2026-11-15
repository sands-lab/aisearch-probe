#!/usr/bin/env python3
"""Box-plot figure for the 3-day public-endpoint lifetime run.
Four panels (one per vendor), shared y = the 8 endpoints grouped by Cache-Control stance,
x = staleness (hours behind live), symlog. Each box = that vendor x endpoint's ~80 samples.
Anthropic dated by its own retrieved_at; the others by interpolation on the 5-min poll.
Google x USGS is a known read-out bug -> blanked. Saves PNG + PDF.
Style: 7pt text, fonttype 42, fixed vendor hues.
Usage: publicPageEndpoint_plot.py [--answers <publicPageEndpoint_run.py output>]
                                  [--truth <publicPageEndpoint_poll.py output>] [--out <path without extension>]"""
import argparse, json, datetime, re
from collections import defaultdict
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.patches import Patch

_ap = argparse.ArgumentParser(description="Draw staleness per vendor and public endpoint.")
_ap.add_argument("--answers", default="logs/results/anchor-probe-lifetime.jsonl", help="the vendor answers")
_ap.add_argument("--truth", default="logs/results/anchor-truth-lifetime.jsonl", help="the 5-min live values")
_ap.add_argument("--out", default="anchor-lifetime-staleness", help="output path without extension")
_args = _ap.parse_args()
PB, TR, OUT = _args.answers, _args.truth, _args.out

def t(s): return datetime.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()

_LEAD = re.compile(r"^\s*([\d][\d,]*)\b")   # a digit-run at the very start of the answer
def parse_value(ans):
    """The endpoint's numeric value. Accepts a bare number and 'value([source](url))' (OpenAI's
    citation suffix); returns None for prose/refusals, which never start with a digit."""
    m = _LEAD.match(str(ans))
    return int(m.group(1).replace(",", "")) if m else None

# ground-truth timelines
poll = defaultdict(list)
for l in open(TR):
    r = json.loads(l)
    if not r.get("ok"): continue
    try: poll[r["key"]].append((t(r["ts"]), int(str(r["value"]).replace(",", ""))))
    except: pass
for k in poll: poll[k].sort()

def age_h(key, V, pt):
    tl = poll.get(key)
    if not tl: return None
    if V <= tl[0][1]: return (pt - tl[0][0]) / 3600.0
    if V >= tl[-1][1]: return max(0.0, (pt - tl[-1][0])) / 3600.0
    for i in range(1, len(tl)):
        t0, v0 = tl[i-1]; t1, v1 = tl[i]
        if v0 <= V <= v1:
            vt = t0 if v1 == v0 else t0 + (V - v0) / (v1 - v0) * (t1 - t0)
            return (pt - vt) / 3600.0
    return (pt - tl[-1][0]) / 3600.0

data = defaultdict(list)   # (vendor, anchor) -> [staleness hours]
for l in open(PB):
    r = json.loads(l)
    if "vendor" not in r or not r.get("ok"): continue
    v, a = r["vendor"], r["anchor"]
    if v == "anthropic" and r.get("retrieved_at"):
        data[(v, a)].append((t(r["ts"]) - t(r["retrieved_at"])) / 3600.0); continue
    V = parse_value(r.get("answer", ""))
    if V is None: continue
    h = age_h(a, V, t(r["ts"]))
    if h is not None: data[(v, a)].append(h)

# endpoints grouped by Cache-Control stance (top band first)
BANDS = [
    ("declared “don't reuse”", [("hn_maxitem","Hacker News","no-cache"),
                                         ("nist_beacon","NIST beacon","no-store"),
                                         ("btc_tip","Bitcoin tip","max-age=1")]),
    ("private",                          [("stackoverflow","StackOverflow","private")]),
    ("reuse for a while",                [("crates_io","crates.io","max-age=15"),
                                         ("drand","drand","max-age=29"),
                                         ("usgs_quakes","USGS quakes","max-age=60")]),
    ("no caching header",                [("eth_height","Ethereum","—")]),
]
INK, INK2, BAND = "#1a1a1a", "#4d4d4d", "#ececec"
VEND = [("google","Google","#0B5FA5"), ("openai","OpenAI","#009E73"),
        ("xai","xAI","#CC79A7"), ("anthropic","Anthropic","#E0A000")]

plt.rcParams.update({"pdf.fonttype": 42, "ps.fonttype": 42, "font.size": 7,
    "axes.titlesize": 7, "axes.labelsize": 7, "xtick.labelsize": 6.5, "ytick.labelsize": 6.5,
    "axes.edgecolor": INK2, "xtick.color": INK2, "ytick.color": INK2, "text.color": INK,
    "axes.spines.top": False, "axes.spines.right": False, "savefig.facecolor": "white"})

# y positions: a gap between bands
ypos, ylabels, rows, band_spans = {}, [], [], []
y = 0.0
for bi, (bname, eps) in enumerate(BANDS):
    start = y
    for key, disp, cc in eps:
        ypos[key] = y
        ylabels.append((y, disp, cc)); rows.append(key); y += 1.0
    band_spans.append((bname, start - 0.5, y - 0.5))
    y += 0.8
ymax = y

fig, axes = plt.subplots(1, 4, sharey=True, figsize=(7.0, 2.8))
for ax, (vk, vname, vcol) in zip(axes, VEND):
    # shaded Cache-Control bands
    for bi, (bname, lo, hi) in enumerate(band_spans):
        if bi % 2 == 0: ax.axhspan(lo, hi, color=BAND, zorder=0)
    for key in rows:
        if vk == "google" and key == "usgs_quakes":
            ax.text(0.09, ypos[key], "read-out bug", fontsize=5.6, style="italic",
                    color=INK2, va="center"); continue
        vals = data.get((vk, key))
        if not vals:
            ax.text(0.09, ypos[key], "not run", fontsize=5.6, style="italic",
                    color="#9a9a9a", va="center"); continue
        vals = [max(v, 0.03) for v in vals]                      # floor so 0 shows on symlog
        bp = ax.boxplot(vals, positions=[ypos[key]], vert=False, widths=0.62,
                        patch_artist=True, showfliers=True,
                        flierprops=dict(marker=".", ms=1.6, mfc=vcol, mec=vcol, alpha=0.5),
                        medianprops=dict(color=INK, lw=1.1),
                        whiskerprops=dict(color=vcol, lw=0.9), capprops=dict(color=vcol, lw=0.9),
                        boxprops=dict(facecolor=vcol, edgecolor=vcol, alpha=0.45, lw=0.9))
    ax.set_xscale("symlog", linthresh=0.1)
    ax.set_xlim(0, 400); ax.set_xticks([0, 0.1, 1, 10, 100])
    ax.set_xticklabels(["0", "0.1", "1", "10", "100"])
    ax.set_title("(%s) %s" % ("abcd"[VEND.index((vk,vname,vcol))], vname), fontweight="bold")
    ax.set_xlabel("staleness (hours)")
    ax.grid(axis="x", color="#e3e3e3", lw=0.5, zorder=0)
    ax.set_axisbelow(True)

axes[0].set_ylim(-0.8, ymax - 0.6); axes[0].invert_yaxis()
axes[0].set_yticks([yy for yy, _, _ in ylabels])
axes[0].set_yticklabels(["%s  (%s)" % (d, cc) for _, d, cc in ylabels])
# Cache-Control grouping is shown by the shaded bands, the gaps, and the directive in each
# row label; the four group names go in the caption (kept off-plot to avoid overlap).

fig.tight_layout(w_pad=0.6)
for ext in ("png", "pdf"):
    fig.savefig("%s.%s" % (OUT, ext), bbox_inches="tight", pad_inches=0.02,
                dpi=200 if ext == "png" else None)
bb = fig.get_tightbbox(fig.canvas.get_renderer())
print("saved %s.pdf / .png  ~ %.2f x %.2f in" % (OUT, bb.width, bb.height))
