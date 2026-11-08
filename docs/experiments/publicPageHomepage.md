# Public page, homepage: does a service answer from an old copy of a real page?

**The question.** For real homepages that change often, does an AI service answer from an old
copy? Read [publicPage.md](publicPage.md) first: it covers the contact setting, robots.txt and the
download server.

## How it works

1. **Pick sites.** Start from a public list of popular sites (the Chrome UX Report, CrUX), visit
   each homepage once, record its caching headers and robots.txt verdicts, and drop adult sites and
   sites in excluded categories.
2. **Watch.** Download each kept homepage every 10 minutes for 24 hours, from your download
   server, and find the pages whose real content changes within 20 minutes.
3. **Pick an anchor** per page: a stable short label just above the changing part, and check that
   each service can find it and quote the words after it.
4. **Run.** Per page, at a start time inside an hour it usually changes: download copy A → ask
   each service on the plain address (warm-up) → 20 minutes later ask each service twice at the
   same moment, on the plain address and on the address plus a random tag (a new address, so the
   service must read the live page) → download copy B. An answer is stale only if the plain answer
   has text found only in copy A and the tagged answer from the same moment does not.

## Run it

```
export PROBE_CONTACT=<your contact>  PROBE_SNAP_HOST=<ssh host>  PROBE_SNAP_DIR=<folder with scripts/>

# 1. site list: first visit (writes work/scan/sites-seed<N>.txt and scan-<time>.jsonl;
#    uses ..._select_cacheHeaders.py), then the filters
python3 scripts/publicPageHomepage_select_sites.py <crux.csv.gz> work/scan
python3 scripts/publicPageHomepage_select_adultSites.py work/scan/sites-seed<N>.txt work/adult.jsonl
python3 scripts/publicPageHomepage_select_urlScanSearch.py work/scan/sites-seed<N>.txt work/search.jsonl
python3 scripts/publicPageHomepage_select_urlScanSubmit.py submit work/scan/sites-seed<N>.txt work/submitted.jsonl  # optional
python3 scripts/publicPageHomepage_select_urlScanSubmit.py read work/submitted.jsonl work/new-results.jsonl
#    then write work/sites.txt: one site per line, the sites that answered on the first visit,
#    let in our name in robots.txt, and passed both filters

# 2. watch (on the download server; writes work/watch/watch-<time>.jsonl and work/watch/snapshots/),
#    then find the changing pages
python3 scripts/publicPageHomepage_poll.py work/sites.txt work/watch --groups 2 --hours 24
python3 scripts/publicPageHomepage_select_changes.py work/watch/watch-<time>.jsonl work/watch/snapshots \
    work/candidates.jsonl work/search.jsonl,work/new-results.jsonl

# 3. anchors
python3 scripts/publicPageHomepage_select_anchor.py anchors work/candidates.jsonl work/watch/watch-<time>.jsonl \
    work/watch/snapshots work/anchors.jsonl

# 4. plan and run (all times UTC)
python3 scripts/publicPageHomepage_run.py plan work/anchors.jsonl work/candidates.jsonl \
    2026-12-01T09:00:00Z work/plan.jsonl
python3 scripts/publicPageHomepage_run.py run work/plan.jsonl work/answers.jsonl --vendors openai,anthropic,xai
```

The two URL Scanner steps need `CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID`. The run step
writes its downloads next to the answers (`work/answers.jsonl.copies.jsonl`); a restart skips
pages already finished. Each script's own notes (`--help` or the top of the file) give the
remaining options; the file names above are examples.

## Reading the result

```
python3 scripts/publicPageHomepage_run.py score work/answers.jsonl
python3 scripts/publicPageHomepage_score_table.py work/answers.jsonl <redo-answers.jsonl> work/table
python3 scripts/publicPageHomepage_score_answerKinds.py work/answers.jsonl work/answers.jsonl.copies.jsonl
python3 scripts/publicPageHomepage_score_anthropicCopies.py work/answers.jsonl work/anthropic
```

`..._score_table.py` builds the main results table (the "download only" rows).
`..._score_answerKinds.py` sorts answers into kinds. `..._score_anthropicCopies.py` reads
Anthropic's own fetch date and fetched text from its replies.
