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

Four stages, each feeding the next. File names below are examples. Set these first:

```
export PROBE_CONTACT=<your contact>  PROBE_SNAP_HOST=<ssh host>  PROBE_SNAP_DIR=<folder with scripts/>
```

1. **Build the site list.** Start from a list of popular sites (a Chrome UX Report, CrUX, file),
   visit each homepage once, and drop adult sites and sites in excluded categories. The first
   visit writes `work/scan/sites-seed<N>.txt` and `work/scan/scan-<time>.jsonl`.

   ```
   python3 scripts/publicPageHomepage_select_sites.py <crux.csv.gz> work/scan
   python3 scripts/publicPageHomepage_select_adultSites.py work/scan/sites-seed<N>.txt work/adult.jsonl
   python3 scripts/publicPageHomepage_select_urlScanSearch.py work/scan/sites-seed<N>.txt work/search.jsonl
   ```

   Optional, for sites with no recent Cloudflare URL Scanner result (needs
   `CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID`):

   ```
   python3 scripts/publicPageHomepage_select_urlScanSubmit.py submit work/scan/sites-seed<N>.txt work/submitted.jsonl
   python3 scripts/publicPageHomepage_select_urlScanSubmit.py read work/submitted.jsonl work/new-results.jsonl
   ```

   Then write `work/sites.txt`, one site per line: the sites that answered on the first visit,
   allow our name in robots.txt, and passed both filters.

2. **Find the pages that change often.** On your download server, download each homepage every
   10 minutes for 24 hours. Then keep the pages whose real content changes within 20 minutes,
   because the test waits 20 minutes between questions.

   ```
   python3 scripts/publicPageHomepage_poll.py work/sites.txt work/watch --groups 2 --hours 24
   python3 scripts/publicPageHomepage_select_changes.py work/watch/watch-<time>.jsonl work/watch/snapshots \
       work/candidates.jsonl work/search.jsonl,work/new-results.jsonl
   ```

3. **Pick an anchor for each page.** An anchor is a short, stable label just above the changing
   part (for example a section heading). The test asks each service to quote the words after it,
   so this step also checks that every service can find it.

   ```
   python3 scripts/publicPageHomepage_select_anchor.py anchors work/candidates.jsonl \
       work/watch/watch-<time>.jsonl work/watch/snapshots work/anchors.jsonl
   ```

4. **Run the test.** First make the schedule (each page starts in an hour when it usually
   changes; all times UTC), then carry it out. The run downloads its own copies of each page
   before and after the questions; a restart skips pages already finished.

   ```
   python3 scripts/publicPageHomepage_run.py plan work/anchors.jsonl work/candidates.jsonl \
       2026-12-01T09:00:00Z work/plan.jsonl
   python3 scripts/publicPageHomepage_run.py run work/plan.jsonl work/answers.jsonl --vendors openai,anthropic,xai
   ```

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
