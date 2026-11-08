package probe

import (
	"fmt"
	"sort"
	"strings"
)

// ExpectedFetcher maps a driver provider name to the bot names that would prove
// the VENDOR'S own fetcher retrieved the page.
//
// This exists because of a measured failure: on 2026-08-31, requests routed via
// OpenRouter with engine:native arrived from `OpenRouter-WebFetch/1.0` for
// OpenAI and xAI — OpenRouter fetching on the vendor's behalf. Results labelled
// with a vendor's name while an aggregator did the fetching are results about
// the aggregator. Nothing in the API response reveals this; only the server log
// does.
var ExpectedFetcher = map[string][]string{
	"openai":       {"openai-query", "openai-search"},
	"or:openai":    {"openai-query", "openai-search"},
	"anthropic":    {"anthropic-query", "anthropic-search"},
	"or:anthropic": {"anthropic-query", "anthropic-search"},
	// Google's url_context fetcher may present as any of its crawler identities.
	"google":        {"google-urlcontext", "google-agent", "google-index", "google-other", "google-vertex"},
	"google-vertex": {"google-urlcontext", "google-agent", "google-index", "google-other", "google-vertex"},
	"or:google":     {"google-urlcontext", "google-agent", "google-index", "google-other", "google-vertex"},
	// Perplexity's Agent API runs fetch_url on Perplexity's own infrastructure
	// even though `model` names a third-party model, so expect a Perplexity
	// crawler identity.
	"perplexity":    {"perplexity-query", "perplexity-index"},
	"or:perplexity": {"perplexity-query", "perplexity-index"},
	"or:x-ai":       {"xai-fetch"},
}

// Attribution is what actually fetched a page in a time window.
type Attribution struct {
	Provider string
	Expected []string
	Seen     []string // distinct bot names observed
	OK       bool     // an expected vendor fetcher was among them
	Aggregat bool     // an intermediary fetched instead — hard failure
	Nothing  bool     // nothing arrived at all
	// Undeclared: something fetched the right page in the window but named no
	// known crawler. Measured for xAI, which uses browser user-agents from a
	// distributed IP pool. Not a failure — a limitation to record.
	Undeclared bool
}

// AttributeFetch decides whether the vendor's own fetcher is what arrived.
//
// urlToken, when non-empty, must appear in the request's full URL. This is what
// keeps two providers apart: attributing by page and time window alone let one
// provider's fetch answer for another, because sequential calls sit seconds
// apart inside the same window — and two routes to the same vendor (a direct
// account and an aggregator) share that vendor's cache, so the second one
// legitimately does not fetch. Give each provider a unique URL and the question
// becomes exact rather than probabilistic.
func AttributeFetch(provider string, records []Record, page string, since, window float64, urlToken ...string) Attribution {
	token := ""
	if len(urlToken) > 0 {
		token = urlToken[0]
	}
	a := Attribution{Provider: provider, Expected: ExpectedFetcher[provider]}
	seen := map[string]bool{}
	for _, r := range records {
		if r.TSUnix < since || r.TSUnix > since+window {
			continue
		}
		if token != "" && !strings.Contains(r.URL, token) {
			continue
		}
		if token == "" && page != "" && r.PageKey != page {
			continue
		}
		if r.Bot == "probe-selfcheck" {
			continue
		}
		seen[r.Bot] = true
	}
	for b := range seen {
		a.Seen = append(a.Seen, b)
	}
	sort.Strings(a.Seen)

	if len(a.Seen) == 0 {
		a.Nothing = true
		return a
	}
	for _, b := range a.Seen {
		if b == "openrouter-fetcher" {
			a.Aggregat = true
		}
		for _, want := range a.Expected {
			if b == want {
				a.OK = true
			}
		}
	}
	if !a.OK && !a.Aggregat {
		a.Undeclared = true
	}
	return a
}

// Verdict is a one-line human summary, written so the failure mode cannot be
// mistaken for success.
func (a Attribution) Verdict() string {
	switch {
	case a.OK:
		return fmt.Sprintf("the vendor's own fetcher arrived (%s) — results are about %s",
			strings.Join(a.Seen, ", "), a.Provider)
	case a.Aggregat:
		return fmt.Sprintf("AN INTERMEDIARY FETCHED (%s), not the vendor. "+
			"Any result labelled %q would actually be about the aggregator's cache. "+
			"Change the tool or engine, or use a direct account.",
			strings.Join(a.Seen, ", "), a.Provider)
	case a.Nothing:
		return "nothing reached the server in the window — either it answered from a " +
			"cache, or it never fetched at all. Read the answer to tell which."
	default:
		return fmt.Sprintf("something fetched the page but named no known crawler (%s). "+
			"Not necessarily wrong — some vendors fetch undeclared, using browser "+
			"user-agents. Confirm it is the vendor before attributing results, and "+
			"record that its identity is unverifiable.",
			strings.Join(a.Seen, ", "))
	}
}
