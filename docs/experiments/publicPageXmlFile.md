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

Set `PROBE_CONTACT=<your contact>` first. File names below are examples.

1. **Find the files.** Looks for each site's sitemaps, feeds and llms.txt in the standard
   places, using the site list from the [homepage test](publicPageHomepage.md):

   ```
   python3 scripts/publicPageXmlFile_select.py work/sites.txt work/files
   ```

2. **Pick the test files.** Keeps the files that answered, are allowed by robots.txt and are big
   enough to test:

   ```
   python3 scripts/publicPageXmlFile_run.py select work/files/files-all-<time>.jsonl work/files-test.jsonl
   ```

3. **Take the answer key.** On your download server, download each test file and keep its first
   lines; the answers are checked against these. Copy `work/files-test.jsonl` there first and the
   key file back afterwards.

   ```
   python3 scripts/publicPageXmlFile_run.py keys work/files-test.jsonl work/keys-start.jsonl
   ```

4. **Ask the services.** Asks each service to quote each file's first 3 lines, one call every 3
   minutes per service. It can be restarted, and a file named `STOP` next to the answers file ends
   it cleanly. Use `--no-pause`: without it, Google's questions pause from 07:00 to 09:30 UTC each
   day, a quiet window kept from the original run.

   ```
   python3 scripts/publicPageXmlFile_run.py ask work/files-test.jsonl work/answers.jsonl \
       --vendors openai,anthropic,xai,google --no-pause
   ```

5. **Take the answer key again.** Files can change during the run, so a second key, taken right
   after, lets an answer match either version:

   ```
   python3 scripts/publicPageXmlFile_run.py keys work/files-test.jsonl work/keys-end.jsonl
   ```

## Reading the result and the figure

```
python3 scripts/publicPageXmlFile_run.py score work/answers.jsonl work/keys-start.jsonl work/keys-end.jsonl
python3 scripts/publicPageXmlFile_plot.py work/answers.jsonl work/keys-start.jsonl,work/keys-end.jsonl figures/xml-file
```

The figure shows, per declared file type and service, the share of answers that read the file,
read only header lines, refused, or other.
