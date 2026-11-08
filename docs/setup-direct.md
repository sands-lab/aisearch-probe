# Server setup, direct (no CDN)

The server answers the internet itself. Nothing sits between the AI services' crawlers and
`probe-proxy` except a plain HTTPS front end, so every header the server sends (including the
strong `ETag`) reaches the crawler unchanged. This is the simplest setup.

## 1. A machine and a domain

- A small virtual server with a public IP address.
- A domain name (or sub-domain) whose DNS `A`/`AAAA` record points at it, e.g.
  `probe.example.org`.

## 2. HTTPS in front of probe-proxy

`probe-proxy` speaks plain HTTP on one port. Put any HTTPS server in front of it that passes
requests through untouched and adds `X-Forwarded-For` (the server logs the crawler's IP from
it). With [Caddy](https://caddyserver.com/), which gets a certificate by itself, the whole
`Caddyfile` is:

```
probe.example.org {
	reverse_proxy localhost:8899
}
```

Caddy compresses nothing unless you add an `encode` line, so leave it out. Do not turn on
response compression, response caching or any rewriting of HTML in the front
end. The pages must arrive byte for byte as the server made them.

## 3. Start the server

```
export PROBE_PAGE_CODE_SECRET=<a long random string>
export PROBE_CONTROL_SECRET=<another long random string>
./bin/probe-proxy -serveDir site -port 8899 -logFile logs/probe.jsonl \
    -correction-serverStateFile logs/correction-server.json \
    -heal-serverStateFile logs/heal-server.json
```

Run it under `systemd`, `tmux` or similar so it outlives your login. The two state files keep
the changeable and healable pages' state across a restart.

| Server switch | What it does |
|---|---|
| `-serveDir` | Serve the test pages, plus static files from this folder (use `site`) |
| `-port` | Port to listen on (default 8899) |
| `-logFile` | The request log, one JSON line per GET |
| `-logOnlyPath` | Log only addresses that start with this |
| `-quiet` | No live line per request on the console |
| `-pageCodeSecret` | Secret for the page codes (default `$PROBE_PAGE_CODE_SECRET`); without it the test pages answer 503 |
| `-controlSecret` | Password for `POST /control/correction` and `POST /control/heal` (default `$PROBE_CONTROL_SECRET`); without it the controls are off |
| `-correction-serverStateFile`, `-heal-serverStateFile` | Where the correction and heal pages' state is kept |
| `-cacheControl` | `Cache-Control` on the static files in `-serveDir` only. Leave empty: the test pages never use it |
| `-robotsCacheControl` | `Cache-Control` on robots.txt. Default `no-store`, as the original servers sent it from 15 Sep 2026. `""` serves it the normal way: no `Cache-Control`, an `ETag` |
| `-proxyTo`, `-skipSelfCheck` | Sit in front of an existing site instead of serving test pages. Not used by any experiment here |

## 4. Check it

From any machine:

```
curl -sI https://probe.example.org/robots.txt
./bin/probe-trial -checkPage -siteUrl https://probe.example.org
```

## Where the runner runs

The runner reads the server log to see who downloaded what, so it needs that file.

- **Simplest:** run `probe-trial` on the server machine and pass `-serverLog logs/probe.jsonl`.
- **Runner on another machine:** copy the log over as it grows (for example `rsync --append`
  every few seconds) and pass the copy as `-serverLog`. The heal and correction runs read the log
  only when they report, after the run, so a short delay does not matter. The API sharing test
  reads the log during the run; on a lagging copy, grade each round again afterwards with
  `scripts/sharingApi_score.py` (see [sharingApi.md](experiments/sharingApi.md)).

The correction and heal runs change pages through the server's control address. On this setup
the control is reached through the public address, so pass `-correction-controlUrl
https://probe.example.org` or `-heal-controlUrl https://probe.example.org`.
