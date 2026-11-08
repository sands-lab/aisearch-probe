# Heal time: does a service see that a page is back in service?

**The question.** A page is out of service (it answers with an error) and the service, asked
about it, says it is down. Five minutes later the page comes back. When the same page is asked
about again later, does the service read it again and see that it is back, or keep answering
from the old error, and for how long?

## How it works

- **The page.** `/heal/<id>/2048` is created broken with a chosen failure (503, 404 or 403)
  through `POST /control/heal` (needs the control password). Once fixed it serves the 2 KB depth
  page with its five codes. The server keeps each page's state in its
  `-heal-serverStateFile`.
- **Schedule, per page:** create it broken → ask once (naming the 95% code) → 5 minutes after the
  answer comes back, fix it → ask again once at the page's check time (naming the 50% code). One
  fresh page per vendor, failure and check time. Check times follow a times-2 grid from 5 minutes
  to about 11 hours after the fix (`-heal-askAgainTimes`), each drawn at random inside its step.
- **What is recorded per page:** did the second ask cause a fetch of the fixed page, and did the
  answer quote the fixed page's code. Nothing is skipped: a failed call or an odd first ask is
  noted, and the page still runs to the end.
- The Anthropic fetch tool is pinned in the plan (default `web_fetch_20250910`, no dynamic
  filtering), so a long sitting cannot change tools half way.

## Run it

1. Plan a sitting (no API call):

   ```
   ./bin/probe-trial -heal-plan -heal-stateFile logs/heal-s1.state.json \
       -heal-startAt 2026-12-01T10:00Z -siteUrl https://<your domain> \
       -vendors or:openai,anthropic,or:x-ai -anthropicModel claude-sonnet-5
   ```

   Direct setup only: add `-heal-controlUrl https://<your domain>`. Behind a CDN, the default
   `http://localhost:8899` is right when the runner runs on the server machine.

2. Run it. The loop sleeps until each step is due, checks the site only then, and exits when
   every page has been asked again (run it in `tmux` or similar):

   ```
   ./bin/probe-trial -heal-loop -heal-stateFile logs/heal-s1.state.json -siteUrl https://<your domain>
   ```

   `-heal-step` instead does what is due now and exits, for cron.

3. Report:

   ```
   ./bin/probe-trial -heal-report -heal-stateFile logs/heal-s1.state.json \
       -heal-reportFile logs/heal-s1.results.json -serverLog logs/probe.jsonl
   ```

| Switch | What it does |
|---|---|
| `-heal-askAgainTimes` | Check times after the fix (default `5m,10m,20m,40m,1h20m,2h40m,5h20m,10h40m`) |
| `-heal-failCodes` | Failures to test (default `503,404,403`) |
| `-heal-repeats` | Pages per vendor, check time and failure in this sitting (default 1) |
| `-heal-fixDelay` | Wait after the first answer before fixing the page (default 5 min) |
| `-heal-firstAskGap` | Time between first asks, in shuffled order (default 90 s) |
| `-heal-startAt` | When the first ask is due, with a time zone (default now) |
| `-heal-jitter` | Draw each check time inside its step (default on) |
| `-heal-pageSize` | Size of the fixed page (default 2048) |
| `-heal-anthropicFetchTool` | Anthropic fetch tool pinned in the plan (default `web_fetch_20250910`) |
| `-heal-reportFile` | With `-heal-report`, also write every page's result as JSON (input for the figure) |

## Reading the result

The report lists every page as fetch yes/no × code yes/no for the second ask, with notes.
A page is left out of the figure when no request reached the server between sending the first
question and getting its answer: then the first answer was not about the failure page, so the
page cannot test a remembered failure. The figure script prints every page it leaves out.

## Figure

```
python3 scripts/heal_plot.py --out figures logs/heal-s1.results.json logs/heal-s2.results.json ...
```

`--origin <host>=<label>` sets the legend name of each server; without it the host names are
used.

How often OpenAI downloaded the fixed page but still answered with the failure, and what those
replies looked like (sitting `NAME` means `logs/heal-NAME.state.json` and
`logs/heal-NAME.results.json`):

```
python3 scripts/heal_score_openaiStale.py --logs logs --server-log <your host>=logs/probe.jsonl \
    --group "My server=s1,s2,s3" --replies logs/raw-replies.jsonl
```
