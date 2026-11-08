package probe

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"time"
)

// A crawler's user-agent is self-declared and trivially forged, which is why
// this study logs client IPs. Some operators publish the address ranges their
// crawlers use; checking a logged IP against them turns "claims to be X" into
// "is X", and turns the study's identity caveat from open into closed.
//
// It also works in the other direction, which is how the OpenRouter
// substitution was proven: an address absent from every published range is
// evidence the declared identity is wrong.

// publishedRanges maps a bot name to the URL of its operator's address list.
// Only add an entry the operator actually publishes.
var publishedRanges = map[string]string{
	"openai-query":  "https://openai.com/chatgpt-user.json",
	"openai-search": "https://openai.com/searchbot.json",
	// GPTBot is the training crawler; listed so its traffic can be checked too.
	"openai-training": "https://openai.com/gptbot.json",
	// Google publishes FOUR lists, and the distinction matters. url_context's
	// fetcher (66.249.93.203, observed 2026-08-31) is in
	// user-triggered-fetchers-GOOGLE — requests made from Google's own
	// infrastructure — not the plain user-triggered-fetchers list, which covers
	// fetches originating on a user's machine. Checked all four empirically;
	// only the -google variant contains it.
	"google-urlcontext": "https://developers.google.com/static/search/apis/ipranges/user-triggered-fetchers-google.json",
	"google-agent":      "https://developers.google.com/static/search/apis/ipranges/user-triggered-fetchers-google.json",
	"google-index":      "https://developers.google.com/static/search/apis/ipranges/googlebot.json",
	"google-other":      "https://developers.google.com/static/search/apis/ipranges/special-crawlers.json",
	// Anthropic publishes ONE feed covering all its crawlers; the user-agent
	// distinguishes them. It only began publishing recently (it previously said it
	// used service-provider IPs — which is why Claude-User is seen from Google
	// Cloud). Same {"prefixes":[{ipv4Prefix|ipv6Prefix}]} shape as Google's.
	"anthropic-query":    "https://claude.com/crawling/bots.json",
	"anthropic-search":   "https://claude.com/crawling/bots.json",
	"anthropic-training": "https://claude.com/crawling/bots.json",
}

// IdentityResult is the verdict for one bot name.
type IdentityResult struct {
	Bot        string
	Requests   int
	IPs        []string
	Verified   int      // IPs inside the operator's published ranges
	Unverified []string // IPs outside them
	Err        string   // ranges could not be fetched
	NoList     bool     // this operator publishes nothing to check against
}

func fetchPrefixes(url string) ([]*net.IPNet, error) {
	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	nets, err := parsePrefixes(body)
	if err != nil {
		return nil, err
	}
	if len(nets) == 0 {
		return nil, fmt.Errorf("no usable prefixes in %s", url)
	}
	return nets, nil
}

// parsePrefixes reads the {"prefixes":[{ipv4Prefix|ipv6Prefix}]} shape that
// OpenAI, Google and Anthropic all publish. Split from the fetch so it can be
// tested without the network.
func parsePrefixes(body []byte) ([]*net.IPNet, error) {
	var doc struct {
		Prefixes []struct {
			IPv4 string `json:"ipv4Prefix"`
			IPv6 string `json:"ipv6Prefix"`
		} `json:"prefixes"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	var nets []*net.IPNet
	for _, p := range doc.Prefixes {
		for _, cidr := range []string{p.IPv4, p.IPv6} {
			if cidr == "" {
				continue
			}
			if _, n, err := net.ParseCIDR(cidr); err == nil {
				nets = append(nets, n)
			}
		}
	}
	return nets, nil
}

// VerifyIdentities checks each bot's logged client IPs against its operator's
// published ranges. Requires network access to fetch those lists.
func VerifyIdentities(records []Record) []IdentityResult {
	byBot := map[string]map[string]int{}
	for _, r := range records {
		if r.Bot == "probe-selfcheck" || r.ClientIP == "" {
			continue
		}
		if byBot[r.Bot] == nil {
			byBot[r.Bot] = map[string]int{}
		}
		byBot[r.Bot][r.ClientIP]++
	}

	cache := map[string][]*net.IPNet{}
	out := make([]IdentityResult, 0, len(byBot))
	for bot, ips := range byBot {
		res := IdentityResult{Bot: bot}
		for ip, n := range ips {
			res.IPs = append(res.IPs, ip)
			res.Requests += n
		}
		sort.Strings(res.IPs)

		url, ok := publishedRanges[bot]
		if !ok {
			res.NoList = true
			out = append(out, res)
			continue
		}
		nets, seen := cache[url]
		if !seen {
			var err error
			nets, err = fetchPrefixes(url)
			if err != nil {
				res.Err = err.Error()
				out = append(out, res)
				continue
			}
			cache[url] = nets
		}
		for _, ip := range res.IPs {
			addr := net.ParseIP(ip)
			hit := false
			for _, n := range nets {
				if addr != nil && n.Contains(addr) {
					hit = true
					break
				}
			}
			if hit {
				res.Verified++
			} else {
				res.Unverified = append(res.Unverified, ip)
			}
		}
		out = append(out, res)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bot < out[j].Bot })
	return out
}

// WriteIdentityReport renders the verdicts. An unverified address is the
// interesting case in both directions: a forged identity, or an intermediary
// fetching on a vendor's behalf.
func WriteIdentityReport(w io.Writer, results []IdentityResult) {
	fmt.Fprintln(w, sep)
	fmt.Fprintln(w, "  IS EACH CRAWLER WHO IT SAYS IT IS?")
	fmt.Fprintln(w, sep)
	fmt.Fprintln(w, "\n  Logged client IPs checked against the operator's own published")
	fmt.Fprintln(w, "  address lists. A user-agent is a claim; an address is evidence.")

	for _, r := range results {
		fmt.Fprintf(w, "\n  %s  (%d requests, %d distinct IP%s)\n",
			r.Bot, r.Requests, len(r.IPs), plural(len(r.IPs)))
		switch {
		case r.NoList:
			fmt.Fprintf(w, "    this operator publishes no address list — identity remains a claim\n")
		case r.Err != "":
			fmt.Fprintf(w, "    could not check: %s\n", truncate(r.Err, 90))
		case len(r.Unverified) == 0:
			fmt.Fprintf(w, "    VERIFIED — every address is inside the published ranges\n")
		default:
			fmt.Fprintf(w, "    %d of %d addresses verified\n", r.Verified, len(r.IPs))
			fmt.Fprintf(w, "    NOT in the published ranges: %v\n", r.Unverified)
			fmt.Fprintf(w, "    Either the user-agent is forged, or something else fetched\n")
			fmt.Fprintf(w, "    on this crawler's behalf. Do not attribute these to the vendor.\n")
		}
	}
	fmt.Fprintln(w)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
