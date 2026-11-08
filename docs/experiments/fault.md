# Fault recovery: what happens when a page fails?

**The question.** When a page fails (error code, slow reply, dropped connection, cut-off body,
redirect loop), does each AI service's fetcher retry, how often, does it obey `Retry-After`,
and what does the failure cost in tokens?

## How it works

The server has deliberately misbehaving pages under `/fault/<kind>`. The fault is in the
address path, never in a query string, because some fetchers strip the query. No fault page
sends `ETag`, `Last-Modified` or `Cache-Control`. Faults that end with content (for example a
slow page that does arrive) serve the 2 KB depth page, so the server needs the page-code secret.

| Fault | What the server does |
|---|---|
| `404`, `403` | Permanent error |
| `500` | Server error |
| `503`, `429` | Busy, with `Retry-After: 10`; `429date` gives the wait as a date |
| `slow` | Answers after about 25 s |
| `empty` | 200 with an empty body |
| `reset` | Drops the connection |
| `cut` | Promises more bytes than it sends |
| `loop/a` | Redirects back and forth between two addresses |
| `flaky/N` | Fails N times for each caller, then serves the page; `flaky/0` works at once |

For each vendor and fault the runner asks one question naming the page, then watches the server
log for late retries, and counts the requests.

## Run it

Two runs: the fault run itself, and a baseline run on a page that works, which the figure
compares the faults against.

1. **Ask about every failing page.** For each vendor, the runner asks about each fault page in
   turn, then waits 90 seconds (`-fault-retryWait`) while it counts the vendor's retries in the
   server log. With 12 faults and 3 vendors that takes roughly an hour. Before the first paid
   question it checks that it can read a fault page's codes with your secret, so a wrong secret
   costs nothing.

   ```
   ./bin/probe-trial -fault-run -siteUrl https://<your domain> -serverLog logs/probe.jsonl \
       -vendors or:openai,anthropic,or:x-ai -anthropicModel claude-sonnet-5 \
       -anthropicFetchTool web_fetch_20250910 -fault-outFile logs/faults-run1.jsonl
   ```

2. **Ask about a page that works, for the baseline.** The same command, asking only about
   `flaky/0` (a fault page that answers normally at once). This gives the token cost of an
   ordinary fetch, to compare the faults against.

   ```
   ./bin/probe-trial -fault-run -fault-kinds flaky/0 -siteUrl https://<your domain> \
       -serverLog logs/probe.jsonl -vendors or:openai,anthropic,or:x-ai \
       -anthropicModel claude-sonnet-5 -anthropicFetchTool web_fetch_20250910 \
       -fault-outFile logs/faults-baseline.jsonl
   ```

| Switch | What it does |
|---|---|
| `-fault-kinds` | Which faults to ask about (default: all) |
| `-fault-retryWait` | How long to watch the log after each question for late retries (default 90 s, at least 45 s) |
| `-fault-outFile` | Per-fault results |
| `-fault-runTag` | Tag put into every fault address, so a repeat run gets new addresses (default: time plus random) |

## Reading the result

Each run prints a table of requests per vendor and fault, and writes per-fault results to the
`-fault-outFile` file and standard records to `logs/results/faults-<run>.jsonl`.

## Figure

Tokens billed per failing page, against the success baseline:

```
python3 scripts/fault_plot.py --logs logs --out figures \
    --runs A=<run>,<run>,<run> --baseline A=logs/faults-baseline.jsonl
```

`<run>` is the run id each fault run prints; its records are `logs/results/faults-<run>.jsonl`.
`--baseline` is the `-fault-outFile` of the `-fault-kinds flaky/0` run. Add `--runs B=...` and
`--baseline B=...` for a second server.
