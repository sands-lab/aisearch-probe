# Read depth and token cost: how far into a page does each service read?

**The question.** How far into a long page does each AI service's fetcher read before it cuts
the page, and how does the number of input tokens billed grow with page size?

## How it works

A depth page, `/depth/html/<id>/<bytes>`, is real prose (a public-domain book) with five marker
codes at 0, 25, 50, 75 and 95% of its length. Each code is made from the page-code secret and the
page id, so it cannot be guessed. The runner asks the model to quote every code it can see. The
deepest code quoted is the deepest the model read. Read depth is judged from the codes in the
answer, not from the server log.

Every call also records the vendor's token use (input, output, and for Anthropic the number of
`pause_turn` rounds), so the same runs give the token cost.

## Run it

```
./bin/probe-trial -readDepth-run -readDepth-page /depth/html/ \
    -readDepth-sizes 4k,8k,16k,32k,64k,128k,256k,512k,1m,2m -readDepth-repeats 3 \
    -vendors or:openai,anthropic,or:x-ai -anthropicModel claude-sonnet-5 \
    -siteUrl https://<your domain> -serverLog logs/probe.jsonl
```

| Switch | What it does |
|---|---|
| `-readDepth-page` | Which page kind (default `/depth/html/`); see the document-type, dynamic-filtering and JavaScript tests for the others |
| `-readDepth-sizes` | Exact sizes (`k` = KiB, `m` = MiB). Smallest usable is about 2k |
| `-readDepth-maxSize` | Without `-readDepth-sizes`: the default 64 KiB to 4 MiB ladder, cut at this size |
| `-readDepth-repeats` | Fresh pages per vendor and size (default 1) |

The big sizes cost the most: a vendor that reads a 2 MiB page bills for it.

## Reading the result

The runner prints the deepest code each vendor quoted per size, and writes standard records to
`logs/results/truncprobe-<run>.jsonl` (with `size_bytes`, `deepest_depth` and the token counts).

## Figures

```
# input tokens against page size, one line per vendor (add --log for log-log axes)
python3 scripts/readDepth_plot_tokens.py figures/read-window logs/results/truncprobe-run*.jsonl

# which of the five codes each vendor returned, by size
python3 scripts/readDepth_plot_markers.py figures/read-depth-grid logs/results/truncprobe-run*.jsonl
```

`readDepth_plot_markers.py` downloads each page once more to learn its five codes, so the server
must still be running with the same secret.
