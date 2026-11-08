# Anthropic dynamic filtering: does the filtering step raise the cost of a fetch?

**The question.** Anthropic's newer web fetch tool (`web_fetch_20260209`) adds "dynamic
filtering": Claude writes and runs code to filter a fetched page before it enters the context.
Anthropic presents it as a way to cut tokens. Does it raise the cost of fetching one ordinary
document instead, and does that change with file type and size?

## How it works

The same depth pages are fetched with the old tool (`web_fetch_20250910`, no filtering) and the
new one (`web_fetch_20260209`, filtering on), on your own Anthropic key, and the billed input
tokens are compared. The runner records the tool version in each result record. One round asks
about the 8 document types at 8 KB, and HTML at 32 and 64 KB, once with each tool. Three rounds
at different times or dates show how much the cost moves from run to run.

## Run it

1. **Run one round.** A round asks about every test page twice on your own Anthropic key, once
   with the old fetch tool (filtering off) and once with the new one (filtering on). The script
   does this for the 8 document types at 8 KB, and for HTML at 32 and 64 KB:

   ```
   SITE_URL=https://<your domain> scripts/dynFilter_run.sh r1
   ```

   It needs `SITE_URL`. `SERVER_LOG` (default `logs/probe.jsonl`), `BIN` (default
   `./bin/probe-trial`) and `MODEL` (default `claude-sonnet-5`) are optional. Every whole reply is
   kept in `logs/dynFilter-r1-replies.jsonl`; those replies show the code Claude ran to filter
   the page, which is where the extra tokens go.

2. **Run two more rounds at other times.** The cost of one fetch moves from run to run, so the
   result pools three rounds. Run them hours or days apart:

   ```
   SITE_URL=https://<your domain> scripts/dynFilter_run.sh r2
   SITE_URL=https://<your domain> scripts/dynFilter_run.sh r3
   ```

## Reading the result and the figure

```
# mean and spread of input tokens per type and size, filtering off against on
python3 scripts/dynFilter_score.py logs/results/truncprobe-run*.jsonl

# two panels: tokens per type at 8 KB, and tokens against HTML page size
python3 scripts/dynFilter_plot.py --out figures/dynFilter logs/results/truncprobe-run*.jsonl
```

Both read only Anthropic records that carry a tool version, so other read-depth runs in the same
folder do not get mixed in.
