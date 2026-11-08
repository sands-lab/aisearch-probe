# Account sharing, through the API

**The question.** Does a vendor hand one caller's stored copy of a page to another caller? The
callers can be the same account asked twice (the reference), two models at one vendor, two
end-user names on one OpenRouter account, or two unrelated paying customers (the vendor's own key
and OpenRouter).

## How it works

Each try makes two brand-new 2 KB depth pages, `/depth/html/<id>/2048`: the shared page and a
control page nobody has touched. Both callers get the identical address, on purpose.

1. The first caller asks about the shared page (naming the 95% code), which makes the vendor
   download it.
2. After `-sharingApi-secondAskGap` (5 min) the second caller asks about the same page, naming a
   different code (50%), so a stored *answer* cannot explain a correct reply.
3. The second caller asks about the control page. It must cause a download, which proves the
   second caller was able to fetch at that moment.
4. After `-sharingApi-lastAskGap` (10 min) a late ask (75%) rules out both requests landing on
   one warm machine.

Every answer is checked against that page's own code, recomputed from the page-code secret. So
"quoted the code and no download reached the server" can only mean the caller was given a copy.

## Run it

```
./bin/probe-trial -sharingApi-run -siteUrl https://<your domain> -serverLog logs/probe.jsonl \
    -sharingApi-firstCaller openai -sharingApi-secondCaller or:openai \
    -anthropicModel claude-sonnet-5 -sharingApi-outFile logs/sharingApi/round1-account.jsonl
```

Caller names are routes (`openai`, `or:openai`, `anthropic`, `or:anthropic`, `x-ai`, `or:x-ai`)
or route plus model (`or:openai-gpt-5-mini`; other models through `-openrouterModels`).

| Comparison | Switches |
|---|---|
| Same caller twice (the reference) | `-sharingApi-run-sameCaller -sharingApi-firstCaller or:openai -sharingApi-secondCaller or:openai` |
| Two paying customers | `-sharingApi-firstCaller openai -sharingApi-secondCaller or:openai` |
| Two models, one vendor | `-sharingApi-firstCaller or:openai-gpt-5-mini -sharingApi-secondCaller or:openai-gpt-5.5 -openrouterModels openai/gpt-5-mini,openai/gpt-5.5` |
| Two end users, one account | `-sharingApi-firstCaller or:openai -sharingApi-secondCaller or:openai -sharingApi-firstUser user-a -sharingApi-secondUser user-b` |

| Switch | What it does |
|---|---|
| `-sharingApi-secondAskGap` | Wait before the second caller asks (default 5 min) |
| `-sharingApi-controlAskGap` | Wait before the control step (default 0; 2 min helps Google's free tier) |
| `-sharingApi-lastAskGap` | Wait before the late ask (default 10 min) |
| `-sharingApi-outFile` | Where the steps are written |

Each comparison takes its own lock, so different comparisons can run side by side.

## Reading the result

Each try gets one verdict: shared, not shared, void (the control step did not download, so
silence proves nothing), or can't tell. The steps go to `-sharingApi-outFile` and standard
records to `logs/results/sharing-<run>.jsonl`.

When the runner reads a copy of the server log that lags behind the real one (runner on another
machine), grade the round again from the full log afterwards. Name each comparison's step file
`logs/sharingApi/<round>-<comparison>.jsonl` (as in the command above), then:

```
python3 scripts/sharingApi_score.py round1 https://<your domain> logs/probe.jsonl
```

It first sends one request of its own to the server and waits until that request shows up in
the log copy (the fence), so the copy is known to be complete up to now. It writes
`logs/sharingApi/<round>-graded.json` and prints a table. No figure script; tables are made from
these files.
