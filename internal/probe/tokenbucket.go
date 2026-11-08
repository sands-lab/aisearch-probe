package probe

import (
	"sort"
	"strings"
)

// probeTagSuffix marks a bot bucket that was named by the token we planted,
// not by the request's user-agent. The user-agent's own claim (or its silence)
// stays untouched; anything we had to infer some other way carries a marker so
// a reader can see the identity did not come from what the request declared.
const probeTagSuffix = " (probe-tag)"

// IsProbeTagBucket reports whether a bot bucket name came from token
// attribution rather than from the user-agent.
func IsProbeTagBucket(bot string) bool {
	return strings.HasSuffix(bot, probeTagSuffix)
}

// probeTagBucket names the bucket for a request attributed by its token. The
// model is kept in the name because we do not assume a vendor's fetcher behaves
// the same across its models — grok-2 and grok-4 stay apart.
func probeTagBucket(provider, model string) string {
	id := provider
	if model != "" {
		id = provider + "/" + model
	}
	if id == "" {
		id = "unknown"
	}
	return id + probeTagSuffix
}

// tokenBucket pairs a token the trial log recorded with the bucket name for the
// provider it was given to.
type tokenBucket struct {
	token  string
	bucket string
}

// buildTokenIndex reads the trial log — the only authority on which planted
// token meant which provider — and returns the tokens longest first. Longest
// first matters because historical tokens carry no random tail, so one token
// (probe=openai) can be a prefix of another (probe=openai-gpt-5-mini); matching
// the longer one first stops the short one shadowing it.
func buildTokenIndex(trials []Trial) []tokenBucket {
	seen := map[string]bool{}
	var idx []tokenBucket
	for _, t := range trials {
		if t.Token == "" || seen[t.Token] {
			continue
		}
		seen[t.Token] = true
		idx = append(idx, tokenBucket{token: t.Token, bucket: probeTagBucket(t.Provider, t.Model)})
	}
	sort.SliceStable(idx, func(i, j int) bool { return len(idx[i].token) > len(idx[j].token) })
	return idx
}

// AttributeUnknownByToken reclassifies requests the user-agent could not name
// ("unknown") but that carry a secret token we planted in exactly one
// provider's URL and nobody else's. Such a request is that provider's fetcher
// wearing a forged user-agent — measured for xAI, whose fetches arrive as an
// ordinary browser from a pool of residential addresses. It returns a copy;
// requests the user-agent already named, and unknown requests carrying no
// planted token (background scanners, link previewers), are left untouched.
//
// Attribution matches the whole token string the trial log recorded against the
// request's URL. It never takes the token apart: historical tokens have many
// hand-made shapes, so only the trial log knows which token meant which
// provider. See doc/unknown-bucket-split-handover.md.
func AttributeUnknownByToken(records []Record, trials []Trial) []Record {
	idx := buildTokenIndex(trials)
	if len(idx) == 0 {
		return records
	}
	out := make([]Record, len(records))
	copy(out, records)
	for i := range out {
		if out[i].Bot != "unknown" {
			continue
		}
		for _, tb := range idx {
			if strings.Contains(out[i].URL, tb.token) {
				out[i].Bot = tb.bucket
				break
			}
		}
	}
	return out
}
