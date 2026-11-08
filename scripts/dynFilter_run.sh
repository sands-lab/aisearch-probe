#!/usr/bin/env bash
# One lean n=3 round of the web_fetch dynamic-filtering study, meant for cron.
#   Breadth anchor: the 8 object types at 8 KB (variance for the heatmap).
#   Depth:          HTML at 32 and 64 KB (variance for the size-scaling curve;
#                   128 KB is kept as the single dramatic n=1 point, not repeated).
# DIRECT ONLY — no OpenRouter; the no-filtering baseline is already confirmed, so each
# round is just filtering OFF (web_fetch_20250910) vs ON (web_fetch_20260209) on the
# Anthropic direct key. Run from the worktree root (cron cd's here first). The three
# rounds are fired at different times/dates so a load transient cannot fake the result.
# Usage: dfbomb-round.sh <round-tag>    e.g. r2
set -u
TAG="${1:-rX}"
BIN=./bin/probe-trial
URL=https://probe.attentionhub.org
PLOG=logs/probe.jsonl
RAW="logs/dfbomb-round-${TAG}-raw.jsonl"
OUT="logs/dfbomb-round-${TAG}.log"
: > "$RAW"

run() {  # <route> <sizes> <web_fetch_version>
  "$BIN" -truncprobe -truncprobe-parta -truncprobe-route "$1" -truncprobe-sizes "$2" -truncprobe-reps 1 \
    -only anthropic -anthropic-model claude-sonnet-5 \
    -anthropic-fetch-version "$3" -anthropic-dump-raw "$RAW" \
    -url "$URL" -probe-log "$PLOG" 2>&1 | grep -viE "loaded|skipping" | tee -a "$OUT"
}

echo "=== dfbomb round $TAG started $(date -u) ===" | tee -a "$OUT"
for f in txt html md json xml csv pdf docx; do
  echo "--- $f 8k A(no filter) ---" | tee -a "$OUT"; run "/depth${f}page/" 8k web_fetch_20250910
  echo "--- $f 8k B(filter) ---"    | tee -a "$OUT"; run "/depth${f}page/" 8k web_fetch_20260209
done
echo "--- html 32k,64k A(no filter) ---" | tee -a "$OUT"; run /depthhtmlpage/ 32k,64k web_fetch_20250910
echo "--- html 32k,64k B(filter) ---"    | tee -a "$OUT"; run /depthhtmlpage/ 32k,64k web_fetch_20260209
echo "=== dfbomb round $TAG done $(date -u) ===" | tee -a "$OUT"
