# Cache headers: does a service obey the site's caching headers?

**The question.** A site tells caches how long they may reuse a page through response headers
(`Cache-Control`, `Expires`). For each common setting, how long does each AI service reuse its
stored copy, and is that within what the HTTP caching standard (RFC 9111) allows? An AI service
that hands one stored copy to many customers is a *shared* cache in RFC 9111's terms, so it is
graded against the rules for shared caches.

## How it works

The test pages are `/cacheHeaders/<setting>/<validators>/<id>/<bytes>`: the same 2 KB depth page
as everywhere else, sent with the caching headers the address names. It is the only page kind
that ever sends caching headers; every other page sends none.

| `<setting>` | Header sent | Longest reuse RFC 9111 allows a shared cache without checking |
|---|---|---|
| `nostore` | `Cache-Control: no-store` | none (must not store) |
| `private` | `Cache-Control: private` | none (a shared cache must not store) |
| `nocache` | `Cache-Control: no-cache` | none (may store, must check every time) |
| `maxage0` | `Cache-Control: max-age=0` | none |
| `combo` | `Cache-Control: no-cache, no-store, max-age=0, must-revalidate` | none |
| `expired` | `Expires:` a date in the past | none |
| `max60` | `Cache-Control: max-age=60` | 60 s |
| `max60reval` | `Cache-Control: max-age=60, must-revalidate` | 60 s |
| `smax60` | `Cache-Control: max-age=3600, s-maxage=60` | 60 s (`s-maxage` wins for a shared cache) |
| `max3600` | `Cache-Control: max-age=3600` | 1 h |
| `none` | nothing | a guess the standard allows: a tenth of the page's age |

`<validators>` is `tag` (an `ETag` and an old fixed `Last-Modified`, the normal page), `recent`
(`Last-Modified` = when the server started, so the tenth-of-the-age guess is minutes), or `notag`
(neither, so checking can only be a full download). The 19 pages run every "no reuse" setting
with and without validators, the timed settings with validators, and `none` with all three.

Each vendor gets one fresh page per setting. The runner asks about all of them once, then again
at each of `-cacheHeaders-askAgainTimes` (default 2.5, 5, 10, 20, 40 and 80 minutes) until the
vendor downloads the page again. Each ask names a different code on the page, so a stored answer
cannot pass for a stored page. The first gap at which a download reaches the server is that
copy's lifetime.

## Run it

```
./bin/probe-trial -cacheHeaders-run -siteUrl https://<your domain> -serverLog logs/probe.jsonl \
    -cacheHeaders-askAgainTimes 2m30s,5m,10m,20m,40m,80m \
    -vendors or:openai,anthropic,or:x-ai -anthropicModel claude-sonnet-5 \
    -anthropicFetchTool web_fetch_20250910
```

Only one cache-header run at a time (it takes its own lock).

| Switch | What it does |
|---|---|
| `-cacheHeaders-settings` | Run only these pages, e.g. `cc-nostore-tag,cc-combo-tag,cc-private-tag,cc-nocache-tag,cc-none-tag` (default all 19; labels are `cc-<setting>-<validators>`) |
| `-cacheHeaders-askAgainTimes` | When to ask again; each gap at least 150 s, rising |
| `-cacheHeaders-askAfterRefetch` | Keep asking at every gap even after the vendor downloads again (default: stop at the first download) |
| `-cacheHeaders-repeats` | Fresh pages per vendor and setting (default 8) |
| `-cacheHeaders-pageSize` | Page size (default 2048) |

## Reading the result

For each page and gap the runner prints "downloaded again" or "reused", graded against what
RFC 9111 allows for the headers sent, and writes `logs/results/ttlmap-<run>.jsonl`. No figure
script; tables are made from these reports.
