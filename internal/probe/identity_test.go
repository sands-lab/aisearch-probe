package probe

import (
	"net"
	"strings"
	"testing"
)

// Anthropic now publishes a crawler-IP feed, so Claude's identities must be wired
// to it — otherwise Claude-User verifies as "no list" despite a real published range.
func TestPublishedRangesCoverAnthropic(t *testing.T) {
	for _, id := range []string{"anthropic-query", "anthropic-search", "anthropic-training"} {
		url, ok := publishedRanges[id]
		if !ok || url == "" {
			t.Errorf("%s has no published range URL", id)
			continue
		}
		if !strings.Contains(url, "claude.com/crawling/bots.json") {
			t.Errorf("%s -> %q, want Anthropic's bots.json feed", id, url)
		}
	}
}

// The prefix parser must handle the ipv4Prefix/ipv6Prefix shape that Anthropic and
// Google both publish, independent of any network fetch.
func TestParsePrefixes(t *testing.T) {
	body := []byte(`{"prefixes":[{"ipv4Prefix":"34.162.0.0/16"},{"ipv6Prefix":"2600:1f1c::/32"},{}]}`)
	nets, err := parsePrefixes(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(nets) != 2 {
		t.Fatalf("got %d nets, want 2 (empty entry ignored)", len(nets))
	}
	if !nets[0].Contains(net.ParseIP("34.162.191.81")) {
		t.Error("34.162.191.81 should be inside 34.162.0.0/16")
	}
}
