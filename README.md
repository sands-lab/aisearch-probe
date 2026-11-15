# aisearch-probe

Test pages, a logging web server and a test runner for measuring how AI assistants fetch, read
and reuse web pages.

You run the server on a domain you control. The runner asks an AI service (OpenAI, Anthropic,
xAI, Google, directly or through OpenRouter) a question about a fresh page on that server. The
server log then shows which crawler downloaded the page, when, and with which headers. The
answer shows the result of both how the model read the page and the service's own processing
of it on the way (text extraction, cutting long pages short, filtering).

To make a code that cannot be faked, the server takes the page's id, the code's position on the
page and a secret key shared by the server and the runner, and runs them through a keyed hash
(HMAC-SHA256). The result is a short code such as `TRUNC-13de1433e2ee`. Without the key nobody
can produce it, so a code in an answer proves the page's text reached the model. Each page
carries five such codes, placed at 0, 25, 50, 75 and 95% of the way down, and the deepest one an
answer quotes shows how far into the page the model got.

## The paper's data

The data behind the paper's figures and tables is in a separate repo:
<https://anonymous.4open.science/r/aisearch-data-9618/>. Put its `logs/` folder in this repo's top
folder to redraw the figures. Its guides give the command for each one.

## What it measures

| Experiment | The question | Page |
|---|---|---|
| [Correction](docs/experiments/correction.md) | A page changes at the same address. When does the answer show the new version? | [docs/experiments/correction.md](docs/experiments/correction.md) |
| [Fault recovery](docs/experiments/fault.md) | A page misbehaves (error code, slow reply, cut-off body, redirect loop). Does the fetcher retry, and what does it cost? | [docs/experiments/fault.md](docs/experiments/fault.md) |
| [Heal time](docs/experiments/heal.md) | A page is out of service (answers with an error) for a short time, then comes back. Asked again later, does the service see that it is back? | [docs/experiments/heal.md](docs/experiments/heal.md) |
| [Account sharing, API](docs/experiments/sharingApi.md) | Does one caller's stored copy get handed to another caller? | [docs/experiments/sharingApi.md](docs/experiments/sharingApi.md) |
| [Account sharing, web app](docs/experiments/sharingWeb.md) | Does a chat app (ChatGPT, Claude, Gemini and others) hand a page one account downloaded to a different account? A person pastes the prompts | [docs/experiments/sharingWeb.md](docs/experiments/sharingWeb.md) |
| [Cache headers](docs/experiments/cacheHeaders.md) | Does a service obey `Cache-Control`, `Expires` and similar headers? | [docs/experiments/cacheHeaders.md](docs/experiments/cacheHeaders.md) |
| [Document type](docs/experiments/documentType.md) | Which file formats does each service read, and is the deciding factor the `Content-Type` header? | [docs/experiments/documentType.md](docs/experiments/documentType.md) |
| [Read depth and token cost](docs/experiments/readDepth.md) | How far into a long page does each service read, and what does it bill? | [docs/experiments/readDepth.md](docs/experiments/readDepth.md) |
| [Anthropic dynamic filtering](docs/experiments/dynFilter.md) | Does the filtering step of Anthropic's newer fetch tool raise the cost of a fetch? | [docs/experiments/dynFilter.md](docs/experiments/dynFilter.md) |
| [JavaScript content](docs/experiments/javascript.md) | Does a service see text that a page adds with JavaScript, or keeps in script blocks? | [docs/experiments/javascript.md](docs/experiments/javascript.md) |
| [Public page, endpoint](docs/experiments/publicPageEndpoint.md) | On real public pages whose value only goes up, how old is the copy a service answers from? | [docs/experiments/publicPageEndpoint.md](docs/experiments/publicPageEndpoint.md) |
| [Public page, homepage](docs/experiments/publicPageHomepage.md) | Does a service answer from an old copy of a real, often-changing homepage? | [docs/experiments/publicPageHomepage.md](docs/experiments/publicPageHomepage.md) |
| [Public page, XML file](docs/experiments/publicPageXmlFile.md) | Does a service read real sitemaps, feeds and llms.txt files, by declared type? | [docs/experiments/publicPageXmlFile.md](docs/experiments/publicPageXmlFile.md) |

## What is in the repo

```
proxy/          probe-proxy: the web server that makes the test pages and logs every request (Go)
trial/          probe-trial: the runner that asks the AI services and reads the log (Go)
internal/probe/ shared code: the log record, page codes, cache-header settings, token use
scripts/        analysis, figures, and the public-page tests (Python)
site/           the folder the server serves its static files from (robots.txt)
docs/           setup and one page per experiment
```

Start with [QUICKSTART.md](QUICKSTART.md).

## Licence and text

The filler text on the depth pages is Joseph Conrad's *Heart of Darkness* (1899), a public-domain
book taken from Project Gutenberg with Project Gutenberg's header and licence footer removed.
You can also bring your own text instead: `scripts/depthPage_build.py --input <your file>` builds
the filler from it.
