package probe

import (
	"strings"
	"testing"
)

func trial(sent float64, provider string, opts ...func(*Trial)) Trial {
	t := Trial{
		TrialID: provider, Offset: "0s", Provider: provider,
		URL: "https://example.test/method.html", SentUnix: sent,
		SentAt: "2026-08-30T00:00:00Z",
	}
	for _, o := range opts {
		o(&t)
	}
	return t
}

func TestCorrelateMatchesFetchInsideWindow(t *testing.T) {
	trials := []Trial{trial(1000, "perplexity")}
	recs := []Record{rec(1003, page("/method.html"))}

	cors := Correlate(trials, recs, 90)
	if len(cors) != 1 {
		t.Fatalf("got %d correlations, want 1", len(cors))
	}
	if !cors[0].Fetched || len(cors[0].Hits) != 1 {
		t.Errorf("a fetch 3s after the query should correlate: %+v", cors[0])
	}
}

func TestCorrelateIgnoresFetchOutsideWindow(t *testing.T) {
	trials := []Trial{trial(1000, "perplexity")}
	recs := []Record{
		rec(900, page("/method.html")),  // before the query
		rec(1200, page("/method.html")), // long after
	}
	cors := Correlate(trials, recs, 90)
	if cors[0].Fetched {
		t.Errorf("background traffic outside the window must not be attributed to the query: %+v", cors[0].Hits)
	}
}

func TestCorrelateIgnoresOtherPages(t *testing.T) {
	trials := []Trial{trial(1000, "perplexity")}
	recs := []Record{rec(1005, page("/metrics.html"))}
	cors := Correlate(trials, recs, 90)
	if cors[0].Fetched {
		t.Error("a fetch of a different page must not correlate")
	}
}

func TestPagePathOf(t *testing.T) {
	tests := map[string]string{
		"https://example.test/method.html":     "/method.html",
		"https://example.test/method.html?a=1": "/method.html",
		"https://example.test":                 "/",
		"http://example.test/docs/a/b.html":    "/docs/a/b.html",
	}
	for in, want := range tests {
		if got := pagePathOf(in); got != want {
			t.Errorf("pagePathOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// The headline case: the assistant answered using the page's content but the
// server never saw a request. That is content reuse, and the report must say so.
func TestReportFlagsAnswerWithoutFetch(t *testing.T) {
	trials := []Trial{trial(1000, "perplexity", func(tr *Trial) { tr.FoundMarker = true })}
	var sb strings.Builder
	WriteCorrelationReport(&sb, Correlate(trials, nil, 90))
	out := sb.String()

	if !strings.Contains(out, "server saw       : NOTHING") {
		t.Errorf("report should say the server saw nothing\n%s", out)
	}
	if !strings.Contains(out, "without contacting the server") {
		t.Errorf("report should call out content reuse\n%s", out)
	}
	if !strings.Contains(out, "Answered from the page WITHOUT fetching: 1") {
		t.Errorf("summary should count it\n%s", out)
	}
}

// No fetch and no marker means it simply didn't have the content — a different
// finding, and it must not be reported as caching.
func TestReportDistinguishesNoFetchWithoutMarker(t *testing.T) {
	trials := []Trial{trial(1000, "perplexity")} // FoundMarker false
	var sb strings.Builder
	WriteCorrelationReport(&sb, Correlate(trials, nil, 90))
	out := sb.String()

	if strings.Contains(out, "without contacting the server") {
		t.Errorf("no marker means no evidence of caching; must not claim reuse\n%s", out)
	}
	if !strings.Contains(out, "Answered from the page WITHOUT fetching: 0") {
		t.Errorf("must not be counted as content reuse\n%s", out)
	}
}

func TestReportShowsWhetherTheFetchRevalidated(t *testing.T) {
	trials := []Trial{trial(1000, "perplexity")}
	recs := []Record{rec(1002, page("/method.html"), withAsk())}
	var sb strings.Builder
	WriteCorrelationReport(&sb, Correlate(trials, recs, 90))
	if out := sb.String(); !strings.Contains(out, `asked "has this changed?" first`) {
		t.Errorf("report should show whether the caused fetch revalidated\n%s", out)
	}
}

func TestParseTrialsSkipsGarbage(t *testing.T) {
	in := strings.Join([]string{
		`{"trial_id":"a","provider":"openai","sent_unix":1000,"url":"https://x/y.html"}`,
		`not json`,
		`{"trial_id":"b","provider":"openai","sent_unix":1100,"url":"https://x/y.html"}`,
	}, "\n")
	got, err := ParseTrials(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ParseTrials: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d trials, want 2", len(got))
	}
}

// The marker is only reliable when the question happens to elicit it. A model
// that correctly answers a NEW question without fetching has demonstrably
// retained the content — so a missing marker must prompt review, never a
// conclusion that it never had the page.
func TestNoFetchNoMarkerShowsTheAnswerForReview(t *testing.T) {
	tr := trial(1000, "openai", func(x *Trial) {
		x.FoundMarker = false
		x.Question = "How many categories does the page use to classify a request?"
		x.Answer = "Three categories: First sight; Return, unstamped; Judgeable."
	})
	var sb strings.Builder
	WriteCorrelationReport(&sb, Correlate([]Trial{tr}, nil, 90))
	out := sb.String()

	if strings.Contains(out, "never had the content") {
		t.Errorf("must not conclude it never had the content — the answer may be correct\n%s", out)
	}
	if !strings.Contains(out, "First sight") {
		t.Errorf("the answer must be shown so a human can judge it\n%s", out)
	}
	if !strings.Contains(out, "READ THE ANSWERS") {
		t.Errorf("summary must flag these for review\n%s", out)
	}
}

// Absent headers are evidence too: a client that never sends a conditional
// header or a Cache-Control is telling you it does not intend to revalidate.
func TestHeaderReportShowsWhatWasAndWasNotSent(t *testing.T) {
	r := rec(100)
	r.Bot = "openai-query"
	r.ReqHeaders = map[string]string{
		"User-Agent":      "ChatGPT-User/1.0",
		"Accept-Encoding": "gzip",
	}
	var sb strings.Builder
	WriteHeaderReport(&sb, []Record{r})
	out := sb.String()

	if !strings.Contains(out, "Accept-Encoding") {
		t.Errorf("sent headers must be listed\n%s", out)
	}
	if !strings.Contains(out, "NOT SENT") || !strings.Contains(out, "If-None-Match") {
		t.Errorf("headers the client never sent must be called out\n%s", out)
	}
}

func TestHeaderReportExplainsWhenNoneRecorded(t *testing.T) {
	var sb strings.Builder
	WriteHeaderReport(&sb, []Record{rec(100)}) // no ReqHeaders
	if !strings.Contains(sb.String(), "nginx Mode A") {
		t.Errorf("should explain why headers may be absent\n%s", sb.String())
	}
}

// The measured failure this guard exists for: an aggregator fetching on a
// vendor's behalf. Nothing in the API response reveals it; only the log does.
func TestAttributionCatchesAggregatorFetch(t *testing.T) {
	r := rec(1000, page("/limitations.html"))
	r.Bot = "openrouter-fetcher"
	r.UserAgent = "OpenRouter-WebFetch/1.0"

	at := AttributeFetch("or:openai", []Record{r}, "/limitations.html", 990, 120)
	if at.OK {
		t.Error("must NOT report success — the vendor's fetcher never arrived")
	}
	if !at.Aggregat {
		t.Error("must flag that an intermediary fetched")
	}
	v := at.Verdict()
	if !strings.Contains(v, "INTERMEDIARY") || !strings.Contains(v, "aggregator") {
		t.Errorf("verdict must be unmistakable, got: %s", v)
	}
}

func TestAttributionAcceptsTheVendorsOwnFetcher(t *testing.T) {
	r := rec(1000, page("/limitations.html"))
	r.Bot = "anthropic-query"
	at := AttributeFetch("or:anthropic", []Record{r}, "/limitations.html", 990, 120)
	if !at.OK {
		t.Errorf("Claude-User is the vendor's own fetcher; should pass. verdict: %s", at.Verdict())
	}
}

func TestAttributionDistinguishesNothingArriving(t *testing.T) {
	at := AttributeFetch("or:openai", nil, "/limitations.html", 990, 120)
	if at.OK || at.Aggregat {
		t.Error("no records: neither success nor aggregator")
	}
	if !at.Nothing {
		t.Error("must report that nothing arrived")
	}
	if strings.Contains(at.Verdict(), "INTERMEDIARY") {
		t.Error("absence of a fetch must not be reported as an aggregator fetch")
	}
}

// Our own probe requests must never be mistaken for a crawler.
func TestAttributionIgnoresSelfChecks(t *testing.T) {
	r := rec(1000, page("/limitations.html"))
	r.Bot = "probe-selfcheck"
	at := AttributeFetch("or:openai", []Record{r}, "/limitations.html", 990, 120)
	if !at.Nothing {
		t.Error("a self-check request must not count as a crawler fetch")
	}
}

// A bot whose operator publishes nothing must be reported as unverifiable, not
// as verified. Uses a bot with no published list so the test needs no network.
func TestIdentityReportsUnverifiableWhenNoListExists(t *testing.T) {
	r := rec(100)
	r.Bot = "openrouter-fetcher"
	r.ClientIP = "2a06:98c0:3600::103"

	res := VerifyIdentities([]Record{r})
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1", len(res))
	}
	if !res[0].NoList {
		t.Error("an operator with no published ranges must be marked unverifiable")
	}
	if res[0].Verified != 0 {
		t.Error("nothing can be counted as verified without a list to check against")
	}

	var sb strings.Builder
	WriteIdentityReport(&sb, res)
	out := sb.String()
	if !strings.Contains(out, "publishes no address list") {
		t.Errorf("report must say why it could not verify\n%s", out)
	}
	if strings.Contains(out, "VERIFIED") {
		t.Errorf("must not print VERIFIED for an unverifiable operator\n%s", out)
	}
}

// Our own probe requests carry a client IP too and must never be scored.
func TestIdentitySkipsSelfChecks(t *testing.T) {
	r := rec(100)
	r.Bot = "probe-selfcheck"
	r.ClientIP = "127.0.0.1"
	if got := VerifyIdentities([]Record{r}); len(got) != 0 {
		t.Errorf("self-checks must be excluded, got %+v", got)
	}
}

// An aggregator fetching is a hard failure. A fetcher that simply does not
// declare itself is a limitation to record, not a reason to stop — measured for
// xAI, which uses browser user-agents from a distributed IP pool.
func TestAttributionGradesAggregatorAboveUndeclared(t *testing.T) {
	agg := rec(1000, page("/p.html"))
	agg.Bot = "openrouter-fetcher"
	a := AttributeFetch("or:openai", []Record{agg}, "/p.html", 990, 120)
	if !a.Aggregat || a.Undeclared {
		t.Errorf("aggregator must be flagged as such, not merely undeclared: %+v", a)
	}

	und := rec(1000, page("/p.html"))
	und.Bot = "unknown"
	und.UserAgent = "Mozilla/5.0 ... Chrome/143.0.0.0 Safari/537.36"
	u := AttributeFetch("or:x-ai", []Record{und}, "/p.html", 990, 120)
	if u.Aggregat {
		t.Error("an undeclared browser-like fetcher is not an aggregator")
	}
	if !u.Undeclared {
		t.Error("must be flagged as undeclared")
	}
	if v := u.Verdict(); strings.Contains(v, "INTERMEDIARY") {
		t.Errorf("undeclared must not read as an aggregator: %s", v)
	}
	if v := u.Verdict(); !strings.Contains(v, "undeclared") {
		t.Errorf("verdict should name the actual situation: %s", v)
	}
}

// Two providers must never satisfy each other's fetch. Two routes to one vendor
// share that vendor's cache, so without a per-provider URL the second route
// legitimately does not fetch — and attributing by page plus a 120s window
// credited it with the first route's request.
func TestAttributionIsolatesProvidersByUrlToken(t *testing.T) {
	// A fetch made on behalf of the DIRECT openai route.
	first := rec(1000, page("/p.html"))
	first.Bot = "openai-query"
	first.URL = "https://x.test/p.html?probe=openai"

	// The aggregator route asked 3 seconds later and did not fetch.
	at := AttributeFetch("or:openai", []Record{first}, "/p.html", 1003, 120, "probe=or-openai")
	if at.OK {
		t.Error("or:openai must NOT be credited with the direct route's fetch")
	}
	if !at.Nothing {
		t.Errorf("should report nothing arrived for this provider, got %+v", at)
	}

	// The direct route, matched by its own token, is credited.
	own := AttributeFetch("openai", []Record{first}, "/p.html", 995, 120, "probe=openai")
	if !own.OK {
		t.Errorf("the route that actually fetched must be credited: %s", own.Verdict())
	}
}

// Without a token, behaviour must be unchanged — the experiment itself uses one
// shared URL per arm deliberately.
func TestAttributionStillWorksWithoutAToken(t *testing.T) {
	r := rec(1000, page("/p.html"))
	r.Bot = "anthropic-query"
	if at := AttributeFetch("or:anthropic", []Record{r}, "/p.html", 995, 120); !at.OK {
		t.Errorf("page+window matching must still work when no token is given: %s", at.Verdict())
	}
}

// The token attributes a fetch to the provider that was given it, even when the
// fetcher forges its user-agent — measured with xAI, whose fetches arrive as
// "Chrome on macOS" from a pool of residential addresses.
func TestTokenAttributesAForgedFetcher(t *testing.T) {
	trials := []Trial{
		{Provider: "or:x-ai", URL: "https://x.test/method.html",
			URLAsked: "https://x.test/method.html?probe=or-x-ai",
			Token:    "probe=or-x-ai", SentUnix: 1000},
		{Provider: "or:openai", URL: "https://x.test/method.html",
			URLAsked: "https://x.test/method.html?probe=or-openai",
			Token:    "probe=or-openai", SentUnix: 1000},
	}
	recs := []Record{{
		TSUnix: 1002, PageKey: "/method.html", StatusCode: 200,
		URL:       "https://x.test/method.html?probe=or-x-ai",
		Bot:       "unknown",
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/143.0.0.0",
	}}
	cors := Correlate(trials, recs, 120)

	if !cors[0].Fetched || len(cors[0].Hits) != 1 {
		t.Errorf("xAI's own fetch was not attributed to it: %+v", cors[0])
	}
	// The other provider must not be credited with someone else's fetch, and
	// must not have it silently vanish either.
	if cors[1].Fetched {
		t.Error("openai was credited with xAI's fetch")
	}
	if len(cors[1].OtherHits) != 1 {
		t.Errorf("the unclaimed request should be kept as an OtherHit, got %d", len(cors[1].OtherHits))
	}
}

// THE DANGEROUS CASE. If a provider strips the query when fetching, its request
// arrives without a token. That is a hole in our instrument, and it must never
// be reported as "nothing arrived, and the answer had the marker" — which is
// exactly the shape of the study's headline finding.
func TestStrippedTokenIsNotReportedAsCaching(t *testing.T) {
	trials := []Trial{{
		Provider: "or:x-ai", URL: "https://x.test/method.html",
		URLAsked: "https://x.test/method.html?probe=or-x-ai",
		Token:    "probe=or-x-ai", SentUnix: 1000,
		FoundMarker: true,
		Answer:      "This page is about the study. Reference code RCP-2026-08.",
	}}
	recs := []Record{{
		TSUnix: 1001, PageKey: "/method.html", StatusCode: 200,
		URL: "https://x.test/method.html", // query normalised away
		Bot: "unknown", UserAgent: "Mozilla/5.0 Chrome/143.0.0.0",
	}}
	cors := Correlate(trials, recs, 120)
	if cors[0].Fetched {
		t.Fatal("an untokenised request must not count as this provider's fetch")
	}
	if len(cors[0].OtherHits) != 1 {
		t.Fatalf("it must be kept for review, got %d OtherHits", len(cors[0].OtherHits))
	}

	var b strings.Builder
	WriteCorrelationReport(&b, cors)
	out := b.String()
	if strings.Contains(out, "server saw       : NOTHING") {
		t.Error("reported as NOTHING arrived — that is the false caching finding")
	}
	if !strings.Contains(out, "UNATTRIBUTABLE") {
		t.Errorf("the report must name it unattributable; got:\n%s", out)
	}
	if strings.Contains(out, "from its own cache") {
		t.Error("the report claimed cache reuse on an unattributable fetch")
	}
}

// A round where our own page was unreachable must be excluded, not scored. It is
// the most dangerous confound in a long run: the trace it leaves — no request at
// the server, marker in the answer — is identical to the study's headline finding.
func TestDeadPageRoundIsNotScoredAsCaching(t *testing.T) {
	tr := Trial{
		Provider: "openai", URL: "https://x.test/method.html",
		Token: "probe=openai", SentUnix: 1000,
		FoundMarker: true,
		Answer:      "The reference code is RCP-2026-08.",
	}
	tr.PageLive.CheckAt = "2026-08-31T18:00:00Z"
	tr.PageLive.OK = false
	tr.PageLive.Err = "dial tcp: connection refused"

	var b strings.Builder
	WriteCorrelationReport(&b, Correlate([]Trial{tr}, nil, 120))
	out := b.String()

	if !strings.Contains(out, "NOT SCORED") {
		t.Errorf("a dead-page round must be marked NOT SCORED; got:\n%s", out)
	}
	if strings.Contains(out, "from its own cache") {
		t.Error("a dead-page round was reported as cache reuse — the exact false finding")
	}
	if strings.Contains(out, "Answered from the page WITHOUT fetching: 1") {
		t.Error("a dead-page round was counted in the headline number")
	}
}

// Trial logs written before the liveness check existed have a zero value here.
// Treating that as a dead page would discard every earlier result.
func TestOlderTrialsWithoutLivenessAreStillScored(t *testing.T) {
	tr := Trial{
		Provider: "openai", URL: "https://x.test/method.html",
		SentUnix: 1000, FoundMarker: true,
		Answer: "The reference code is RCP-2026-08.",
	} // PageLive left zero, as an old log decodes

	var b strings.Builder
	WriteCorrelationReport(&b, Correlate([]Trial{tr}, nil, 120))
	out := b.String()
	if strings.Contains(out, "NOT SCORED") {
		t.Errorf("an old trial log was discarded as unreachable; got:\n%s", out)
	}
	if !strings.Contains(out, "from its own cache") {
		t.Errorf("the finding was lost for old logs; got:\n%s", out)
	}
}
