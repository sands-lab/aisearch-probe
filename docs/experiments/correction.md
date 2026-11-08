# Correction: when does an answer show a changed page?

**The question.** A site owner corrects a page at the same address. How long until each AI
service fetches the corrected page, and how long until its answers show it?

## How it works

- **One address, two versions.** `/correction/html/<id>/2048` and `/correction/txt/<id>/2048`
  serve the 2 KB depth page: five codes at 0, 25, 50, 75 and 95% down the page. Each version
  has its own set of codes, so the code in an answer shows which version the model had.
- **Switching versions.** The runner switches the version through the server's control address,
  `POST /control/correction`.
- **`Last-Modified`** is set to the switch time.
- **Which version each fetch got** is read from the server log (`etag_sent`).
- **Schedule, per page:** ask once (the first ask names the 95% code) → 5 minutes after that
  answer comes back, switch to version 2 → ask again once at the page's check time (naming the
  50% code). Each page is used once: one new page for every vendor, file type and wait.
- **When to ask again.** You choose how long after the switch the second question is asked,
  with `-correction-askAgainTimes` (for example 15 min, 30 min, 1 h, 2 h). The runner adds
  random jitter to each of these times (`-correction-jitter`, on by default).

## Run it

A run has three commands. The first only writes a schedule, the second carries it out over hours
or days, and the third reads the results.

1. **Make the schedule.** This picks one new page address for every vendor, file type and wait,
   and works out when each question and each version switch is due. It writes all of that to a
   state file. It sends no questions and costs nothing.

   ```
   ./bin/probe-trial -correction-plan -correction-stateFile logs/correction-s1.state.json \
       -siteUrl https://<your domain> -vendors or:openai,anthropic,or:x-ai \
       -correction-askAgainTimes "or:openai=15m,30m,1h,2h,4h;*=1h,2h,4h,8h,16h,32h,64h" \
       [-correction-startAt 2026-12-01T10:00Z]
   ```

   The runner switches pages through the server's control address, `-correction-controlUrl`
   (default `http://localhost:8899`). Behind a CDN, run the runner on the server machine and
   keep the default. Direct setup: add `-correction-controlUrl https://<your domain>`.

2. **Carry out the schedule.** This sets each page up on the server, asks the first questions,
   switches each page to version 2 and asks the second questions, each at its due time, reading
   and updating the state file as it goes. With the longest wait of 64 hours, that takes about three days. Keep it running in
   `tmux` or similar; it stops by itself when every page is done:

   ```
   ./bin/probe-trial -correction-loop -correction-stateFile logs/correction-s1.state.json
   ```

   If you prefer cron, call `-correction-step` instead: each call does whatever is due right
   now, then exits.

3. **Read the results**, after the last second question:

   ```
   ./bin/probe-trial -correction-report -correction-stateFile logs/correction-s1.state.json \
       -serverLog logs/probe.jsonl
   ```

To repeat the experiment, make a new schedule with a new state file (for example
`correction-s2`) on another day or at another time.

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

Run this once, after "Read the results", listing every run you made. A run's name is the part of
its state file name between `correction-` and `.state.json` (`s1` for
`logs/correction-s1.state.json`):

```
python3 scripts/correction_plot.py --state-dir logs --server-log logs/probe.jsonl --out figures s1 s2 s3
```

The figures are saved in `figures/`.
