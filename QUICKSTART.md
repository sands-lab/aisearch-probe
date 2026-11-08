# Quick start

## Who this is for

Anyone who wants to repeat the measurements on their own server, or run new ones: researchers,
site owners, people who write crawler or caching policy.

## What you need

- **A domain name you control**, pointing at a machine that is reachable from the internet. The
  AI services must be able to download pages from it. Google's fetcher will not read pages on
  tunnel addresses (free Cloudflare or ngrok tunnels), localhost or private addresses, so use a
  real domain name if you want Google results.
- **Go 1.22 or newer** to build the two programs.
- **Python 3.10 or newer** with `matplotlib` and `numpy` for the figures, and `pytest` for the
  tests. The public-page tests also use only Python.
- **API keys**: either each vendor's own key, or one OpenRouter key. See [docs/keys.md](docs/keys.md).
- **Two secrets** you make up yourself:
  - the page-code secret (`PROBE_PAGE_CODE_SECRET`). The server uses it to print an
    unforgeable code on every page; the runner uses the same secret to check answers.
  - the control password (`PROBE_CONTROL_SECRET`). The runner uses it to change or fix pages on
    the server during the correction and heal tests.

## Build and test

```
go build -o bin/probe-proxy ./proxy
go build -o bin/probe-trial ./trial
go test ./...
python3 -m pytest scripts -q
```

## Set up the server

Pick one of the two ways the experiments were run:

- [Direct, no CDN](docs/setup-direct.md): the server answers the internet itself. Simplest.
- [Behind a CDN](docs/setup-cdn.md): a CDN (Cloudflare was used) sits in front of the server. The
  CDN must be set so it does not cache the test pages and does not change their `ETag`.

Either way the server is started the same way:

```
export PROBE_PAGE_CODE_SECRET=<a long random string>
export PROBE_CONTROL_SECRET=<another long random string>
./bin/probe-proxy -serveDir site -port 8899 -logFile logs/probe.jsonl
```

Each request becomes one JSON line in `logs/probe.jsonl`: time, address, crawler name, IP, the
validation headers the crawler sent (`If-None-Match`, `If-Modified-Since`) and the headers the
server sent back.

## A first run

Put your keys in a file (see [docs/keys.md](docs/keys.md)), then:

```
# Is the server reachable, and does a page carry the code the secret predicts? No API call.
./bin/probe-trial -checkPage -siteUrl https://<your domain>

# Can each vendor route fetch a page? One cheap API call per route.
./bin/probe-trial -checkVendors -siteUrl https://<your domain> -serverLog logs/probe.jsonl
```

`-checkVendors` asks each route about its own fresh 2 KB depth page. A route passes when the
answer quotes one of that page's codes. With `-serverLog` it also reads the log to show which
crawler downloaded the page, which catches a route that fetches through someone else's crawler.
Passes are remembered for 6 hours (`-checkRecordAge`); `-checkAgain` re-tests every route.

Then pick an experiment from the [README](README.md). Every experiment page gives the exact
commands. Add `-dryRun` to any run first: it prints the plan and makes no paid call.

## Where results go

- `logs/probe.jsonl`: the server log (or wherever `-logFile` points).
- `logs/results/<experiment>-<run>.jsonl`: one standard record per question asked, with the
  answer, the token use and the page address. The record file names use short internal
  experiment ids: `truncprobe` (read depth and the tests that use its runner), `ttlmap` (cache
  headers), `faults`, `sharing`.
- `logs/raw-replies.jsonl`: every whole vendor reply (`-replyLog`; empty turns it off).
- State files for the multi-step runs (correction, heal) where you point them.

## Shared runner switches

| Switch | What it does |
|---|---|
| `-siteUrl` | Address of your server, e.g. `https://probe.example.org` |
| `-serverLog` | The server's request log, read to see who downloaded what |
| `-vendors` | Which vendor routes to use, e.g. `or:openai,anthropic,or:x-ai` |
| `-keyFile` | File holding the API keys (default `~/.config/probe-trial/env`) |
| `-replyLog` | Keeps every whole vendor reply |
| `-dryRun` | Print the plan, make no calls |
| `-forceFetch` | Make the model use its fetch tool, not memory (on by default) |
| `-anthropicModel`, `-openaiModel`, `-xaiModel`, `-googleModel`, `-openrouterModels` | Models per route |
| `-anthropicFetchTool` | Anthropic fetch tool version: `web_fetch_20250910` (no dynamic filtering) or `web_fetch_20260209` (with it) |
| `-pageCodeSecret`, `-controlSecret` | The two secrets, if not set in the environment |
| `-checkVendors`, `-checkPage`, `-checkRecordFile`, `-checkRecordAge`, `-checkAgain` | The checks above |
| `-listModels`, `-listModelsAll` | List the models each key can call |
| `-resolve host:ip` | Use this IP for your own host name, like curl's `--resolve` |

`./bin/probe-trial -help` and `./bin/probe-proxy -help` list every switch.
