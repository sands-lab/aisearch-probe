#!/usr/bin/env python3
"""Pool the three time-spread rounds of the dynamic-filtering study and report mean +/- spread
per cell. Rounds: 2026-10-15 (round 1 breadth 2/4/8/16k + depth 32/64/128k), 2026-10-16 (r2),
2026-10-17 (r3). Direct only; cell = A (web_fetch_20250910, filtering off) or B
(web_fetch_20260209, filtering on). n=3 cells: all types at 8KB, and HTML 32/64KB."""
import json, glob, re, statistics as st

def cell(v, w):
    if v != "anthropic":
        return None
    return "A" if w == "web_fetch_20250910" else "B" if w == "web_fetch_20260209" else None

pool = {}
for f in sorted(glob.glob("logs/results/truncprobe-run*.jsonl")):
    for line in open(f):
        r = json.loads(line); ex = r.get("extra", {})
        if ex.get("part") != "A":
            continue
        m = re.search(r"/depth([a-z]+)page/", r.get("url", ""))
        c = cell(r.get("vendor", ""), ex.get("web_fetch_version", ""))
        it = r.get("input_tokens")
        if m and c and it:
            pool.setdefault((m.group(1), ex.get("size_bytes"), c), []).append(it)

def fmt_stat(xs):
    if len(xs) == 1:
        return f"{xs[0]:>8,} (n=1)"
    return f"{st.mean(xs):>8,.0f} ±{st.pstdev(xs):>7,.0f} (n={len(xs)}, {min(xs):,}-{max(xs):,})"

FMT = ["txt", "html", "md", "json", "xml", "csv", "pdf", "docx"]
print("=== 8 KB breadth anchor (n=3) — filtering OFF vs ON ===")
for f in FMT:
    A = pool.get((f, 8192, "A"), []); B = pool.get((f, 8192, "B"), [])
    if A and B:
        spread = max(B)/min(B) if len(B) > 1 else 1
        print(f"{f:5}  OFF {fmt_stat(A):<30}  ON {fmt_stat(B):<42}  on-swing {spread:.1f}x")
print()
print("=== HTML depth ladder — filtering ON (n varies by size) ===")
for s in [2048, 4096, 8192, 16384, 32768, 65536, 131072]:
    B = pool.get(("html", s, "B"), []); A = pool.get(("html", s, "A"), [])
    if B:
        sw = max(B)/min(B) if len(B) > 1 else 1
        print(f"{s//1024:>4}KB  OFF {fmt_stat(A):<22}  ON {fmt_stat(B):<42}  on-swing {sw:.1f}x")
