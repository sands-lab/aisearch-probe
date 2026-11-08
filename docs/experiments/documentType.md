# Document type: which formats does each service read?

**The question.** Which file formats does each AI service read, and is the deciding factor the
`Content-Type` header the server sends, or the bytes themselves?

## How it works

The depth page exists in eight formats, all with the same five codes:
`/depth/html/`, `/depth/txt/`, `/depth/md/`, `/depth/json/`, `/depth/xml/`, `/depth/csv/`,
`/depth/pdf/`, `/depth/docx/`. Each is sent with its normal `Content-Type`. A vendor "reads" a
format when its answer quotes the page's codes.

To tell the header apart from the bytes, `/depth/declaredType/<label>/<id>/<bytes>` serves the
identical body under a chosen `Content-Type`: `plain` (`text/plain`), `html`, `xml`
(`application/xml`), `md`, `csv`. Same bytes, only the label differs.

## Run it

Once per format (here the PDF):

```
./bin/probe-trial -readDepth-run -readDepth-page /depth/pdf/ -readDepth-sizes 4k \
    -vendors or:openai,anthropic,or:x-ai -anthropicModel claude-sonnet-5 \
    -siteUrl https://<your domain> -serverLog logs/probe.jsonl
```

The header against the bytes, once per label:

```
./bin/probe-trial -readDepth-run -readDepth-page /depth/declaredType/plain/ -readDepth-sizes 2k \
    -vendors or:openai,anthropic,or:x-ai -anthropicModel claude-sonnet-5 \
    -siteUrl https://<your domain> -serverLog logs/probe.jsonl
```

## Reading the result

Read or not read per vendor and format, from the deepest code quoted (none = not read). The
records are the same as the [read-depth test](readDepth.md). No figure script; the tables were
made from the run output.
