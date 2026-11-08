# Correction: when does an answer show a changed page?

**The question.** A site owner corrects a page at the same address. How long until each AI
service fetches the corrected page, and how long until its answers show it? The run keeps
"fetched the correction but still answered the old version" apart from ordinary reuse of an
old copy. HTML and plain-text pages run side by side, to see whether the file type matters.

## How it works

- **One address, two versions.** `/correction/html/<id>/2048` and `/correction/txt/<id>/2048`
  serve the 2 KB depth page: five codes at 0, 25, 50, 75 and 95% down the page. The codes are
  made from the page's current version. Version 2's codes did not exist before the switch, so a
  version-2 code in an answer proves the model had the new page; a version-1 code after the
  switch means the answer was out of date.
- **The version is server state.** `POST /control/correction` (needs the control password) sets
  which version an address serves. The request and the question are the same before and after.
- **`Last-Modified` moves to the switch time**, so a date-only "has this changed?" gets the right
  answer. No `Cache-Control`, like every page except the cache-header test.
- **Which version each fetch got** is read from the server log (`etag_sent`).
- **Schedule, per page:** ask once (the first ask names the 95% code) → 5 minutes after that
  answer comes back, switch to version 2 → ask again once at the page's check time (naming the
  50% code). One fresh page per vendor, format and check time, so no page is asked twice after
  the switch. Each check time is drawn at random inside its own step of a times-2 grid, so checks
  never fall into step with anything the vendor does on a timer.

## Run it

1. Plan a sitting. No API call; writes the state file.

   ```
   ./bin/probe-trial -correction-plan -correction-stateFile logs/correction-s1.state.json \
       -siteUrl https://<your domain> -vendors or:openai,anthropic,or:x-ai \
       -correction-askAgainTimes "or:openai=15m,30m,1h,2h,4h;*=1h,2h,4h,8h,16h,32h,64h" \
       [-correction-startAt 2026-12-01T10:00Z]
   ```

   Direct setup only: add `-correction-controlUrl https://<your domain>` (see
   [setup-direct.md](../setup-direct.md)). Behind a CDN, the default `http://localhost:8899` is
   right when the runner runs on the server machine.

2. Run the steps until every page is done. Either keep a loop running (in `tmux` or similar):

   ```
   ./bin/probe-trial -correction-loop -correction-stateFile logs/correction-s1.state.json
   ```

   or call `-correction-step` from cron: it does what is due, then exits.

3. Report, once the last check has passed:

   ```
   ./bin/probe-trial -correction-report -correction-stateFile logs/correction-s1.state.json \
       -serverLog logs/probe.jsonl
   ```

Repeats come from separate sittings on different days and times, each with its own state file.

| Switch | What it does |
|---|---|
| `-correction-askAgainTimes` | When to ask again after the switch, per vendor; `*` covers the rest. No default: every vendor must be covered |
| `-correction-formats` | `html,txt` (default) |
| `-correction-repeats` | Pages per vendor, format and check time in this sitting (default 1) |
| `-correction-firstAskGap` | Time between first asks, in shuffled order (default 90 s) |
| `-correction-switchDelay` | Wait after the first answer before switching (default 5 min) |
| `-correction-startAt` | When the first ask is due, with a time zone (default now) |
| `-correction-jitter` | Draw each check time at random inside its step (default on) |
| `-correction-pageSize` | Page size in bytes (default 2048) |

## Reading the result

The report prints, for each page, its timeline (first ask, switch, each fetch and which
version it got, the second ask) and one outcome: answered with the new version, answered with the
old version without fetching, or fetched the new version and still answered with the old one.

## Figure

Name each sitting's state file `correction-<RUN>.state.json` in one folder, then:

```
python3 scripts/correction_plot.py --logs logs --origin A=logs/probe.jsonl=My\ server \
    --out figures A:s1:new A:s2:new A:s3:new
```

Each run is given as `SERVER:RUN:PROMPT`; `PROMPT` is `new` for runs made with this code (the
address and the question only). `--origin` can be given once per server when you ran on two.
