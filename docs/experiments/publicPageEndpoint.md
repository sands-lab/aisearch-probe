# Public page, endpoint: how old is the copy a service answers from?

**The question.** On real public addresses whose value only ever goes up (a block height, an item
counter, a random-beacon round), how far behind the live value is each service's answer, and does
the site's `Cache-Control` make a difference? No server of your own is needed.

## How it works

- `scripts/publicPageEndpoint_config.py` lists the endpoints and how to read each one's value.
  Nine are used: `hn_maxitem`, `nist_beacon`, `drand`, `osm_minute`, `stackoverflow`,
  `crates_io`, `eth_height`, `btc_tip`, `usgs_quakes`. They cover the common `Cache-Control`
  stances (`no-cache`, `no-store`, `max-age`, none).
- `scripts/publicPageEndpoint_run.py` asks each vendor to fetch an endpoint and return only its
  value, on the same schedule for every vendor (every 45 minutes, with a random offset, around the
  clock). OpenAI, xAI and Google get all nine; Anthropic gets four, one per `Cache-Control`
  stance, and stops at a spending cap.
- `scripts/publicPageEndpoint_poll.py` reads the live values every 5 minutes (no vendor calls),
  which dates the OpenAI, xAI and Google answers. Anthropic's reply carries the time it fetched the
  page (`retrieved_at`), so Anthropic dates itself.
- Staleness = how far behind the live value at the moment of asking the answered value was.

## Run it

Start both, side by side (in `tmux` or similar), for about three days:

```
python3 scripts/publicPageEndpoint_poll.py logs/results/endpoint-truth.jsonl 300 259200
python3 scripts/publicPageEndpoint_run.py logs/results/endpoint-answers.jsonl \
    --rounds 96 --interval 2700 --jitter 300 --anthropic-cap 30
```

Keys: `OPENROUTER_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`. `--dry` makes no call.
`--vendors` and `--anchors` narrow the run.

## Figure

```
python3 scripts/publicPageEndpoint_plot.py --answers logs/results/endpoint-answers.jsonl \
    --truth logs/results/endpoint-truth.jsonl --out figures/endpoint-staleness
```

One panel per vendor: staleness per endpoint, grouped by `Cache-Control` stance.
