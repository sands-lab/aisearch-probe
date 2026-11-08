# Public page, XML file: does a service read real XML files?

**The question.** Does each AI service read real XML files that sites publish (sitemaps and
RSS/Atom feeds), by the type the server declares (`application/xml`, `text/xml`, RSS, Atom)? As
a contrast, llms.txt files served as `text/plain`. Read [publicPage.md](publicPage.md) first.

## How it works

1. `scripts/publicPageXmlFile_select.py` finds each site's files in the standard places only:
   `Sitemap:` lines in robots.txt (else `/sitemap.xml`), RSS/Atom links on the homepage, and
   `/llms.txt`. Each file is checked against its own host's robots.txt. It uses the site list
   from the [homepage test](publicPageHomepage.md).
2. `scripts/publicPageXmlFile_run.py` picks the test files (`select`), downloads an answer key
   for each on your download server before and after the run (`keys`), asks each service to quote
   the file's first 3 lines (`ask`), and scores each answer (`score`): read, header lines only,
   fetched as one line, refused, failed or inconclusive.

## Run it

```
export PROBE_CONTACT=<your contact>
python3 scripts/publicPageXmlFile_select.py work/sites.txt work/files
python3 scripts/publicPageXmlFile_run.py select work/files/files-all-<time>.jsonl work/files-test.jsonl
python3 scripts/publicPageXmlFile_run.py keys work/files-test.jsonl work/keys-start.jsonl    # on the download server
python3 scripts/publicPageXmlFile_run.py ask work/files-test.jsonl work/answers.jsonl --vendors openai,anthropic,xai,google --no-pause
python3 scripts/publicPageXmlFile_run.py keys work/files-test.jsonl work/keys-end.jsonl      # on the download server
```

`ask` runs one lane per vendor, one call every 3 minutes, and can be restarted; a file named
`STOP` next to the answers file ends it cleanly. Without `--no-pause` the Google lane pauses
from 07:00 to 09:30 UTC each day (a quiet window kept from the original run; not needed
otherwise). Copy `work/files-test.jsonl` to the download server for the `keys` steps and the key
files back.

## Reading the result and the figure

```
python3 scripts/publicPageXmlFile_run.py score work/answers.jsonl work/keys-start.jsonl work/keys-end.jsonl
python3 scripts/publicPageXmlFile_plot.py work/answers.jsonl work/keys-start.jsonl,work/keys-end.jsonl figures/xml-file
```

The figure shows, per declared file type and service, the share of answers that read the file,
read only header lines, refused, or other.
