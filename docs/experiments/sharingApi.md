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

One command runs one comparison between two callers, from start to end. It takes about 15
minutes (the 5- and 10-minute waits plus the questions).

1. **Pick the two callers to compare.** A caller is a route (`openai`, `or:openai`,
   `anthropic`, `or:anthropic`, `x-ai`, `or:x-ai`), or a route plus model
   (`or:openai-gpt-5-mini`). Each comparison asks a different sharing question:

   | To test whether a copy is shared between… | Switches |
   |---|---|
   | the same caller asked twice (the reference: does the caller keep a copy at all?) | `-sharingApi-run-sameCaller -sharingApi-firstCaller or:openai -sharingApi-secondCaller or:openai` |
   | two paying customers (the vendor's own key and OpenRouter) | `-sharingApi-firstCaller openai -sharingApi-secondCaller or:openai` |
   | two models of one vendor | `-sharingApi-firstCaller or:openai-gpt-5-mini -sharingApi-secondCaller or:openai-gpt-5.5 -openrouterModels openai/gpt-5-mini,openai/gpt-5.5` |
   | two end users of one OpenRouter account | `-sharingApi-firstCaller or:openai -sharingApi-secondCaller or:openai -sharingApi-firstUser user-a -sharingApi-secondUser user-b` |

2. **Run the comparison.** This makes the two fresh pages, asks the four questions at their
   times, and prints the verdict. Name the output file `<round>-<comparison>` so the grading
   script in step 3 can find it:

   ```
   ./bin/probe-trial -sharingApi-run -siteUrl https://<your domain> -serverLog logs/probe.jsonl \
       -sharingApi-firstCaller openai -sharingApi-secondCaller or:openai \
       -anthropicModel claude-sonnet-5 -sharingApi-outFile logs/sharingApi/round1-account.jsonl
   ```

   Each comparison takes its own lock, so you can run several at once in separate terminals.

3. **Grade again, only if the runner read a copy of the log.** When the runner is on another
   machine than the server, its copy of the log can lag, and a download may arrive after the
   runner looked. After the round, this script checks every step again against the full log:

   ```
   python3 scripts/sharingApi_score.py round1 https://<your domain> logs/probe.jsonl
   ```

   It first sends one request of its own and waits until it appears in the log, so the log is
   known to be complete up to that moment. It writes `logs/sharingApi/round1-graded.json` and
   prints a table.

| Switch | What it does |
|---|---|
| `-sharingApi-secondAskGap` | Wait before the second caller asks (default 5 min) |
| `-sharingApi-controlAskGap` | Wait before the control question (default 0; 2 min helps Google's free tier) |
| `-sharingApi-lastAskGap` | Wait before the late question (default 10 min) |
| `-sharingApi-outFile` | Where the steps are written |

## Reading the result

Each try gets one verdict: shared, not shared, void (the control step did not download, so
silence proves nothing), or can't tell. The steps go to `-sharingApi-outFile` and standard
records to `logs/results/sharing-<run>.jsonl`.

No figure script; tables are made from these files.
