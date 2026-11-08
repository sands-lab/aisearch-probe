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

Two parts. The first finds which formats each vendor reads. The second checks whether the
`Content-Type` header alone decides it.

1. **Ask about each format.** Run this once per format, changing the page kind each time:
   `/depth/html/`, `/depth/txt/`, `/depth/md/`, `/depth/json/`, `/depth/xml/`, `/depth/csv/`,
   `/depth/pdf/`, `/depth/docx/`. Here, the PDF:

   ```
   ./bin/probe-trial -readDepth-run -readDepth-page /depth/pdf/ -readDepth-sizes 4k \
       -vendors or:openai,anthropic,or:x-ai -anthropicModel claude-sonnet-5 \
       -siteUrl https://<your domain> -serverLog logs/probe.jsonl
   ```

2. **Send the same text under different labels.** Run this once per label: `plain`, `html`,
   `xml`, `md`, `csv`. The body is identical every time; only the `Content-Type` header changes.
   If a vendor reads one label and not another, the header is what decides. Here, `text/plain`:

   ```
   ./bin/probe-trial -readDepth-run -readDepth-page /depth/declaredType/plain/ -readDepth-sizes 2k \
       -vendors or:openai,anthropic,or:x-ai -anthropicModel claude-sonnet-5 \
       -siteUrl https://<your domain> -serverLog logs/probe.jsonl
   ```

## Reading the result

Read or not read per vendor and format, from the deepest code quoted (none = not read). The
records are the same as the [read-depth test](readDepth.md). No figure script; the tables were
made from the run output.
