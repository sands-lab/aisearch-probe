#!/usr/bin/env bash
# One round of the Anthropic web_fetch dynamic-filtering test.
#   Breadth: the 8 document types at 8 KB.
#   Depth:   HTML at 32 and 64 KB.
# Each page is asked twice: filtering OFF (web_fetch_20250910) and filtering ON
# (web_fetch_20260209), on your own Anthropic key. Run three rounds at different
# times or dates, so a passing load spike cannot fake the result, then pool them
# with scripts/dynFilter_score.py.
#
# Usage: SITE_URL=https://<your server> scripts/dynFilter_run.sh <round-tag>   e.g. r1
# Settings (environment):
#   SITE_URL    your server's address (required)
#   SERVER_LOG  your server's request log (default logs/probe.jsonl)
#   BIN         the probe-trial binary (default ./bin/probe-trial)
#   MODEL       the Anthropic model (default claude-sonnet-5)
# probe-trial also needs PROBE_PAGE_CODE_SECRET and ANTHROPIC_API_KEY (or -keyFile).
# Every vendor reply is kept whole in logs/dynFilter-<round-tag>-replies.jsonl.
set -u
TAG="${1:?usage: dynFilter_run.sh <round-tag>}"
: "${SITE_URL:?set SITE_URL to the address of your server, e.g. https://probe.example.org}"
SERVER_LOG="${SERVER_LOG:-logs/probe.jsonl}"
BIN="${BIN:-./bin/probe-trial}"
MODEL="${MODEL:-claude-sonnet-5}"
REPLIES="logs/dynFilter-${TAG}-replies.jsonl"
OUT="logs/dynFilter-${TAG}.log"
mkdir -p logs

run() {  # <page kind> <sizes> <web_fetch version>
  "$BIN" -readDepth-run -readDepth-page "$1" -readDepth-sizes "$2" -readDepth-repeats 1 \
    -vendors anthropic -anthropicModel "$MODEL" -anthropicFetchTool "$3" \
    -replyLog "$REPLIES" -siteUrl "$SITE_URL" -serverLog "$SERVER_LOG" 2>&1 \
    | grep -viE "loaded|skipping" | tee -a "$OUT"
}

echo "=== dynamic-filtering round $TAG started $(date -u) ===" | tee -a "$OUT"
for f in txt html md json xml csv pdf docx; do
  echo "--- $f 8k, filtering off ---" | tee -a "$OUT"; run "/depth/$f/" 8k web_fetch_20250910
  echo "--- $f 8k, filtering on ---"  | tee -a "$OUT"; run "/depth/$f/" 8k web_fetch_20260209
done
echo "--- html 32k,64k, filtering off ---" | tee -a "$OUT"; run /depth/html/ 32k,64k web_fetch_20250910
echo "--- html 32k,64k, filtering on ---"  | tee -a "$OUT"; run /depth/html/ 32k,64k web_fetch_20260209
echo "=== dynamic-filtering round $TAG done $(date -u) ===" | tee -a "$OUT"
