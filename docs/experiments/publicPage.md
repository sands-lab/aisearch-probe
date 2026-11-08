# Public pages: shared notes for the three public-page tests

The three public-page tests ask the AI services about real pages on other people's sites, not
about pages on your server:

- [Endpoint](publicPageEndpoint.md): public addresses whose value only goes up.
- [Homepage](publicPageHomepage.md): real, often-changing homepages.
- [XML file](publicPageXmlFile.md): real sitemaps, feeds and llms.txt files.

They are Python only. Shared parts:

- `scripts/publicPage_run.py` sends one question to one vendor and logs the whole reply: OpenAI
  and xAI through OpenRouter, Anthropic on its own key with the basic fetch tool
  (`web_fetch_20250910`), Google on `GEMINI_API_KEY`.
- `scripts/publicPage_config.py` holds the model list, the time limits and retries, the
  Anthropic tool settings and the refusal pattern.
- Keys come from the environment: `OPENROUTER_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`.

## Being a polite visitor

Every script that downloads pages itself sends an honest name tag with your contact:
`PROBE_CONTACT=<your contact>` must be set, and the scripts refuse to run without it. They obey
robots.txt for the name `research-cache-probe`, back off on 429 and 503, and keep request rates
low. The homepage and XML tests visit a few thousand sites, so read each script's notes before
running it.

## Your download server

Some sites block cloud or university address ranges. The homepage and XML tests therefore take
their own copies of the pages (the answer keys) on a server the sites let in, over ssh:

- `PROBE_SNAP_HOST`: the ssh host of that server, as you would type it after `ssh`.
- `PROBE_SNAP_DIR`: the folder on it holding a copy of `scripts/`.

The AI questions themselves can run anywhere.
