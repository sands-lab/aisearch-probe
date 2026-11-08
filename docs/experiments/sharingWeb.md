# Account sharing, web app

**The question.** The same question as the [API sharing test](sharingApi.md), for consumer chat
apps: does a page one account's chat downloaded get served to a different account's chat? A
person does the browsing; the helper tells them what to paste and reads the server log.

## How it works

Two logins per app, in separate browser profiles. For each app the helper makes the same 2 KB
depth pages and uses the same prompt text as the API run. Four parts, each going through all apps
before the next:

1. Account A asks about the shared page.
2. Account A asks about the same page again, in a new chat (does the app keep a copy at all?).
3. Account B asks about the shared page, 5 minutes after A's first ask: the sharing measurement.
4. Account B asks about a fresh control page (proof that B could download right then).

The person pastes each prompt into the named app and login, then pastes the reply into
`$EDITOR`. At the end the helper waits 30 s, reads the server log again and splits each step's
downloads into before and after the answer.

## Run it

```
./bin/probe-trial -sharingWeb-run -siteUrl https://<your domain> -serverLog logs/probe.jsonl \
    -sharingWeb-apps claude,chatgpt,gemini,grok
```

| Switch | What it does |
|---|---|
| `-sharingWeb-apps` | Apps in this sitting (default `claude,chatgpt,gemini,grok,deepseek,mistral,qwen,metaai`) |
| `-sharingWeb-firstAccount`, `-sharingWeb-secondAccount` | Labels for the two logins (default `account-a`, `account-b`) |
| `-sharingWeb-secondAskGap` | Wait from A's ask to B's ask, per app (default 5 min) |

No API keys are needed. The runner must read an up-to-date server log, because it splits
downloads into before and after each pasted answer; run it on the server machine.

## Reading the result

Per app the helper prints a same-account verdict (did the app keep a copy?) and a two-account
verdict (was it shared?). Replies go to `logs/sharingWeb/raw/<run>.jsonl`, steps to
`logs/sharingWeb/web-<run>.jsonl`, standard records to `logs/results/sharing-<run>.jsonl`.
