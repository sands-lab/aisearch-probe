# API keys

You need either each vendor's own key, or one OpenRouter key. Both work for most experiments.

## Where keys go

`probe-trial` reads a plain `KEY=value` file, by default `~/.config/probe-trial/env`
(`-keyFile` picks another). Variables already set in your shell win over the file. The file
is a good place for the two secrets as well:

```
OPENAI_API_KEY=...
ANTHROPIC_API_KEY=...
XAI_API_KEY=...
GEMINI_API_KEY=...
OPENROUTER_API_KEY=...
PROBE_PAGE_CODE_SECRET=...
PROBE_CONTROL_SECRET=...
```

Any route whose key is missing is skipped, with a message saying so.

## Routes

Each route has a name. `-vendors` picks routes by name.

| Route | Key | Model switch (default) |
|---|---|---|
| `openai` | `OPENAI_API_KEY` | `-openaiModel` (`gpt-5-mini`) |
| `anthropic` | `ANTHROPIC_API_KEY` | `-anthropicModel` (`claude-sonnet-5`) |
| `x-ai` | `XAI_API_KEY` | `-xaiModel` (`grok-4.3`) |
| `google` | `GEMINI_API_KEY` | `-googleModel` (`gemini-3.1-flash-lite`) |
| `google-vertex` | `GOOGLE_APPLICATION_CREDENTIALS` (path to a service-account file) | `-googleModel`; used only when there is no `GEMINI_API_KEY` |
| `or:openai`, `or:anthropic`, `or:x-ai` | `OPENROUTER_API_KEY` | `-openrouterModels` (`openai/gpt-5-mini,anthropic/claude-sonnet-5,x-ai/grok-4.3`) |

A route can also be named with its model, e.g. `or:openai-gpt-5-mini`; the sharing test uses that
to compare two models of one vendor.

Through OpenRouter the fetch still runs on the vendor's own side (the server log shows
`ChatGPT-User`, `Claude-User` and so on), so the cache being measured is the vendor's.
`-checkVendors` confirms this from the log for each route. Not every OpenRouter model has a
vendor-side fetch: for some, OpenRouter fetches with its own crawler instead, which turns the
result into one about OpenRouter. Keep to the default models or check new ones first.

## Which experiments need which keys

- Most experiments were run with `-vendors or:openai,anthropic,or:x-ai`: OpenAI and xAI through
  OpenRouter, Anthropic with its own key.
- **Anthropic needs its own key** wherever the fetch tool version matters
  (`-anthropicFetchTool`, used by the dynamic-filtering, JavaScript, fault, heal and cache-header
  runs). OpenRouter does not pass that setting through.
- The API sharing test compares callers, so it needs at least two routes to the same vendor (for
  example `openai` and `or:openai`).
- The public-page tests (Python) use OpenRouter for OpenAI and xAI, the Anthropic key for
  Anthropic, and `GEMINI_API_KEY` for Google.

## Checking keys

```
./bin/probe-trial -listModels          # which cheap models each key can call
./bin/probe-trial -checkVendors -siteUrl https://<your domain> -serverLog logs/probe.jsonl
```
