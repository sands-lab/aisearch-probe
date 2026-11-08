package probe

import "strings"

// botSignatures maps a substring of the User-Agent to a short bot name.
//
// Order matters: the first match wins, so more specific tokens must come
// before the general ones they contain (Claude-SearchBot before ClaudeBot).
//
// Only tokens that actually appear in a User-Agent header belong here.
// Google-Extended and Applebot-Extended are deliberately absent: they are
// robots.txt opt-out tokens, not user agents, and will never match live
// traffic. Identity here is self-declared and trivially forged — treat these
// names as "claims to be", not "is". Client IPs are logged so a bot's identity
// can be confirmed by reverse DNS later without re-running collection.
var botSignatures = []struct {
	needle string
	name   string
}{
	// Our own instrumentation — not a crawler. Classified so the report can
	// set it aside instead of scoring requests we made ourselves.
	{"probe-selfcheck", "probe-selfcheck"},
	// The driver's own per-round page check (trial/liveness.go). Classified so
	// it is attributed to the instrument instead of swelling "unknown".
	{"probe-liveness", "probe-liveness"},
	// NOT a vendor crawler — an aggregator fetching on a vendor's behalf.
	// Measured on 2026-08-31: OpenRouter sends this instead of the vendor's own
	// fetcher for providers that have no native fetch tool, even with
	// engine:native requested. Any result attributed to a vendor while THIS is
	// what arrived is a result about OpenRouter. Classified so it can never sit
	// unnoticed in the "unknown" bucket.
	{"OpenRouter-WebFetch", "openrouter-fetcher"},
	{"OpenRouter", "openrouter-fetcher"},
	// OpenAI
	{"OAI-SearchBot", "openai-search"},
	{"ChatGPT-User", "openai-query"},
	{"GPTBot", "openai-training"},
	// Anthropic
	{"Claude-SearchBot", "anthropic-search"},
	{"Claude-User", "anthropic-query"},
	{"ClaudeBot", "anthropic-training"},
	// Perplexity
	{"Perplexity-User", "perplexity-query"},
	{"PerplexityBot", "perplexity-index"},
	// Google — Googlebot is the positive control: it is documented to send
	// conditional requests, so if it shows up as never asking, suspect the
	// origin's ETag delivery before suspecting the bot.
	// url_context's fetcher, observed 2026-08-31 from 66.249.93.203 — Google's
	// own long-standing crawler range. Declares itself plainly, unlike xAI's.
	{"GoogleAgent-URLContext", "google-urlcontext"},
	{"GoogleAgent", "google-agent"},
	{"Google-CloudVertexBot", "google-vertex"},
	{"GoogleOther", "google-other"},
	{"Googlebot", "google-index"},
	// Others
	{"bingbot", "bing-index"},
	{"Applebot", "apple-index"},
	{"Amazonbot", "amazon-index"},
	{"meta-externalagent", "meta-training"},
	{"FacebookBot", "meta-index"},
	{"Bytespider", "bytedance-training"},
	{"CCBot", "commoncrawl"},
	{"cohere-ai", "cohere"},
	{"MistralAI-User", "mistral-query"},
	{"DuckAssistBot", "duckduckgo-assist"},
	{"YouBot", "you-search"},
	// xAI's fetcher user-agent is not documented publicly; these are guesses
	// pending observation. If a run shows an xAI fetch landing in "unknown",
	// read the real string out of the log and correct this. [unverified]
	{"xAI-Bot", "xai-fetch"},
	{"Grok", "xai-fetch"},
}

// ClassifyBot names the crawler behind a User-Agent string, or "unknown".
// Classification happens at read time, so adding a signature reclassifies
// historical logs on the next analysis run.
func ClassifyBot(ua string) string {
	lower := strings.ToLower(ua)
	for _, sig := range botSignatures {
		if strings.Contains(lower, strings.ToLower(sig.needle)) {
			return sig.name
		}
	}
	return "unknown"
}
