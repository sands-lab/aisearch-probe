# Server setup, behind a CDN

A CDN sits between the internet and `probe-proxy`. Cloudflare was used, with a Cloudflare tunnel
from the CDN to `probe-proxy` on `localhost:8899`. Many real sites look like this, so it is worth
measuring. But a CDN can quietly change what the experiment sees: it can answer from its own
cache (so the request never reaches the log), and it can change or drop the `ETag`. The CDN
therefore needs three settings.

## The three CDN settings the experiments need

1. **No caching on the test pages.** The CDN must forward every request for the test addresses
   to the server: `/depth/*`, `/cacheHeaders/*`, `/correction/*`, `/heal/*`, `/fault/*`. In
   Cloudflare, add a Cache Rule for the host (or for those paths) with **Bypass cache**. The
   cache-header test sends `Cache-Control: public, max-age=...` on some pages on purpose; without
   this rule the CDN would keep those pages and answer for the server.
2. **Pass the `ETag` through unchanged.** The server sends a strong `ETag` on every page. A CDN
   that compresses or rewrites a page turns it into a weak one (`W/"..."`) or drops it, and then
   crawlers cannot ask "has this changed?" the way they would at a normal site. In Cloudflare,
   turn on **Respect Strong ETags** (Caching, Configuration) and turn off everything that rewrites
   HTML (Auto Minify, Rocket Loader, Email Obfuscation, and similar).
3. **Keep the CDN's cache status visible.** Check that every test address is answered by the
   server, not the CDN: `CF-Cache-Status` should read `DYNAMIC` or `BYPASS`, never `HIT`.
   The server log records `CF-Cache-Status` and `CF-Ray` whenever the CDN passes them on.

Also turn off bot blocking for the host (Cloudflare's Bot Fight Mode and similar). It would block
or challenge the very crawlers being measured. It may still challenge the runner's own page
check; the runner treats any HTTP answer as "the site is up".

Check settings 1 to 3 from outside after any change:

```
curl -sI https://probe.example.org/depth/html/setup-check/2048 | grep -iE 'etag|cf-cache-status|cache-control'
```

The `ETag` must have no `W/` in front and the cache status must not be `HIT`. (Without the
page-code secret the page answers 503; the headers still show the CDN's behaviour.)

## Start the server

On the machine the tunnel points at:

```
export PROBE_PAGE_CODE_SECRET=<a long random string>
export PROBE_CONTROL_SECRET=<another long random string>
./bin/probe-proxy -serveDir site -port 8899 -logFile logs/probe.jsonl \
    -correction-serverStateFile logs/correction-server.json \
    -heal-serverStateFile logs/heal-server.json
```

The server switches are listed in [setup-direct.md](setup-direct.md).

## Where the runner runs

Run `probe-trial` on the same machine as the server and pass `-serverLog logs/probe.jsonl`.
The correction and heal runs then reach the page controls on `http://localhost:8899`, which is
their default (`-correction-controlUrl`, `-heal-controlUrl`). That keeps the controls off the
public address and out of reach of the CDN's bot checks.

## Google and tunnels

Google documents that its URL tool does not support tunneling services. A Cloudflare tunnel on
your own domain name is a real domain; a free throwaway tunnel address is not, and Google will
then download the page but tell the model it is blocked. `probe-trial` prints a highlighted
warning on every run that includes a Google route.
