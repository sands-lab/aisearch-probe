# JavaScript content: does a service see text that a script puts on the page?

**The question.** Many real pages put their text on the page with JavaScript. Does a
question-time AI fetcher run the page's script, and does the model get the text the script
writes? Does it get text kept inside script blocks that is never displayed?

## How it works

The 4 KB depth page in five versions. The 0% code is plain HTML in all of them, which proves the
page reached the model; the deeper four codes are placed differently:

| Page | Where the deeper codes are |
|---|---|
| `/depth/html/` | plain HTML (the baseline) |
| `/depth/jsInline/` | scrambled inside an inline script that unscrambles them and writes them in |
| `/depth/jsExternal/` | the same, in a separate script file `/depth/jsExternalScript/<id>/<bytes>.js` |
| `/depth/jsData/` | plain JSON in a data block that a script copies into the page |
| `/depth/jsonLd/` | plain text in a schema.org JSON-LD block, never displayed |

The scrambled versions cannot be read without running the script. So a model that quotes only
the top code got the page but not the script's text. Whether a fetcher downloaded the outside
script file shows in the server log as requests for `/depth/jsExternalScript/`.

## Run it

Once per version:

```
./bin/probe-trial -readDepth-run -readDepth-page /depth/jsInline/ -readDepth-sizes 4k \
    -vendors anthropic,or:openai,or:x-ai -anthropicModel claude-sonnet-5 \
    -anthropicFetchTool web_fetch_20250910 \
    -siteUrl https://<your domain> -serverLog logs/probe.jsonl
```

The basic Anthropic fetch tool (`web_fetch_20250910`) is pinned on purpose: the newer one runs
code on the fetched page, which could unscramble the script itself. To check whether that
matters, repeat with `-vendors anthropic -anthropicFetchTool web_fetch_20260209`.

## Reading the result

The runner prints the deepest code each vendor quoted: top only means the script's text never
reached the model. Records: `logs/results/truncprobe-<run>.jsonl`. The proxy tests run the served
script under Node.js (skipped if Node is missing) to prove the script does write the codes in.
No figure script; the table was made from the run output.
