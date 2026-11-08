package main

import (
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// THE ROBOTS.TXT INVALIDATION EXPERIMENT, origin side.
//
// The question: if a site owner disallows a page in robots.txt AFTER a provider
// has already fetched and stored it, does the provider drop the stored copy, or
// keep serving it? robots.txt is documented as permission to FETCH, not
// permission to KEEP — so the expected answer is "the copy survives", and a
// clean null is the sharp finding that a site owner has no working takedown
// lever. Only Anthropic can be affected: measured 2026-08-31, its Claude-User is
// the only on-demand fetcher that reads robots.txt at all.
//
// WHY THIS IS DYNAMIC AND NOT A HAND-EDITED FILE. The manipulation is a change
// to robots.txt made BETWEEN the warm and the re-ask, mid-run. Editing a file
// under probe-site/ during a run is exactly what the run lock forbids
// (CLAUDE.md § Operating constraint), and the origin runs behind a tunnel on
// someone else's machine, so the change has to be triggerable over the wire.
// This serves /robots.txt from the on-disk base file PLUS a set of Disallow
// rules held in memory, and flips those rules through a control endpoint. No
// file changes; the base file stays the source of the allow-all default.
//
// WHY IT IS OFF BY DEFAULT. A live control endpoint that mutates what crawlers
// are told is not something a public origin should carry unasked. It exists only
// when the operator sets a secret at startup (-robots-control-secret); with no
// secret the endpoint is a plain 404 and /robots.txt is exactly the file on disk.
//
// WHY robots.txt is served no-store. If an edge or the fetcher cached the
// allow-all robots.txt, the flip would never be seen and the experiment would
// measure nothing. The response carries Cache-Control: no-store and no ETag, so
// every read of robots.txt reaches this handler.

// controlRobotsPath is the reserved control endpoint. It is a POST, so the
// request handler (which logs only GET) never records it as a crawler visit, and
// it can never be mistaken for a page.
const controlRobotsPath = "/control/robots"

// defaultCrawler is which crawler a disallow applies to when the caller names
// none. Anthropic's Claude-User is the only fetcher this experiment can move.
const defaultCrawler = "Claude-User"

// builtinRobots is served only if the site has no robots.txt of its own, so the
// dynamic path still behaves like a real origin.
const builtinRobots = "User-agent: *\nAllow: /\n"

// robotsState holds the experimental Disallow rules layered on top of the base
// robots.txt. Each rule is "this crawler may not fetch this path". It starts
// empty — the default robots.txt is exactly the file on disk.
type robotsState struct {
	mu    sync.Mutex
	rules map[robotsRule]bool
}

// robotsRule is one "User-agent: Agent / Disallow: Path" pairing.
type robotsRule struct {
	Agent string
	Path  string
}

func newRobotsState() *robotsState { return &robotsState{rules: map[robotsRule]bool{}} }

func (rs *robotsState) disallow(agent, path string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.rules[robotsRule{Agent: agent, Path: path}] = true
}

func (rs *robotsState) allow(agent, path string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	delete(rs.rules, robotsRule{Agent: agent, Path: path})
}

func (rs *robotsState) clear() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.rules = map[robotsRule]bool{}
}

func (rs *robotsState) snapshot() []robotsRule {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]robotsRule, 0, len(rs.rules))
	for r := range rs.rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// renderRobots builds the robots.txt body: one group per crawler that has a
// disallow, then (usually) the base file.
//
// A crawler obeys the single most specific group that names it, and does NOT
// merge that with the wildcard group. So a Claude-User group with a Disallow
// leaves every other path allowed for Claude-User (no Disallow means allowed),
// and every other crawler still falls through to the base's "User-agent: * /
// Allow: /". A named page is disallowed; a page never named stays allowed —
// which is why the never-named page is the experiment's control.
//
// THE WILDCARD CASE. The respect-on-first-fetch test disallows a page for
// "User-agent: *" — every crawler — because we do not want to guess each
// provider's exact crawler name. But the base file already carries a
// "User-agent: *" group, and two groups for the same user-agent are ambiguous.
// So when a "*" rule is armed, the base is dropped and the single armed "*"
// group stands in for the wildcard: it lists only Disallow lines, and every
// path not listed is allowed by default (no rule means allowed). That leaves the
// control page allowed and the named page blocked, unambiguously.
func renderRobots(base string, rules []robotsRule) string {
	if len(rules) == 0 {
		return base
	}
	// Group paths by crawler.
	byAgent := map[string][]string{}
	var agents []string
	for _, r := range rules {
		if _, seen := byAgent[r.Agent]; !seen {
			agents = append(agents, r.Agent)
		}
		byAgent[r.Agent] = append(byAgent[r.Agent], r.Path)
	}
	sort.Strings(agents)
	_, hasStar := byAgent["*"]

	var b strings.Builder
	b.WriteString("# Experimental disallow(s) added at runtime by probe-proxy.\n")
	b.WriteString("# See proxy/robots.go — the robots.txt invalidation experiments.\n")
	for _, a := range agents {
		paths := byAgent[a]
		sort.Strings(paths)
		fmt.Fprintf(&b, "User-agent: %s\n", a)
		for _, p := range paths {
			fmt.Fprintf(&b, "Disallow: %s\n", p)
		}
		b.WriteString("\n")
	}
	// A "*" rule replaces the base wildcard, or the file would carry two
	// "User-agent: *" groups — ambiguous. Otherwise keep the base allow-all so
	// every unnamed crawler stays allowed.
	if !hasStar {
		b.WriteString(base)
	}
	return b.String()
}

// baseRobots reads the site's own robots.txt, falling back to a built-in
// allow-all if there is none, so the dynamic endpoint always looks like a real
// origin.
func (s *staticServer) baseRobots() string {
	f, err := s.openFold("/robots.txt")
	if err != nil {
		return builtinRobots
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil || len(body) == 0 {
		return builtinRobots
	}
	return string(body)
}

// serveRobots serves the base robots.txt plus any experimental disallows. It is
// served no-store with no ETag so the flip is never hidden by a cache. See the
// package comment for why.
//
// TODO(2026-10-07): make robots.txt follow normal behaviour by default — the same
// headers an ordinary site's robots.txt carries (no Cache-Control, an ETag) — and
// keep no-store only as an explicit opt-in for the robots-flip experiment. As it
// stands, every crawler reading our robots.txt sees a cache-test-style setting,
// the leak DECISIONS.md § D13 removed from every generated page.
func (s *staticServer) serveRobots(w http.ResponseWriter) {
	body := renderRobots(s.baseRobots(), s.robots.snapshot())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, body)
}

// controlAuthorized checks the control secret, from the X-Probe-Control-Secret
// header or the form's secret field (r.ParseForm must have run). Shared by every
// control endpoint (robots, version). Constant-time compare so the secret cannot
// be guessed a byte at a time.
func (s *staticServer) controlAuthorized(r *http.Request) bool {
	got := r.Header.Get("X-Probe-Control-Secret")
	if got == "" {
		got = r.Form.Get("secret")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.controlSecret)) == 1
}

// serveControl handles the robots control endpoint. It reports whether it
// handled the request; when the feature is off (no secret configured) it returns
// false so the caller 404s it like any unknown path — the endpoint simply does
// not exist.
//
//	POST /control/robots   op=disallow|allow|clear
//	                       agent=<crawler>   (default Claude-User)
//	                       path=<path>       (required for disallow/allow)
//	                       secret=<secret>   (or header X-Probe-Control-Secret)
//
// On success it returns the resulting robots.txt body, so the caller can verify
// the flip landed before it waits for the crawler to re-read it.
func (s *staticServer) serveControl(w http.ResponseWriter, r *http.Request) bool {
	if s.controlSecret == "" {
		return false // feature disabled: not a real endpoint
	}
	if r.Method != http.MethodPost {
		http.Error(w, "control: POST only", http.StatusMethodNotAllowed)
		return true
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "control: bad form", http.StatusBadRequest)
		return true
	}
	if !s.controlAuthorized(r) {
		http.Error(w, "control: forbidden", http.StatusForbidden)
		return true
	}

	agent := strings.TrimSpace(r.Form.Get("agent"))
	if agent == "" {
		agent = defaultCrawler
	}
	path := strings.TrimSpace(r.Form.Get("path"))
	op := strings.TrimSpace(r.Form.Get("op"))

	switch op {
	case "disallow":
		if path == "" {
			http.Error(w, "control: disallow needs a path", http.StatusBadRequest)
			return true
		}
		s.robots.disallow(agent, path)
	case "allow":
		if path == "" {
			http.Error(w, "control: allow needs a path", http.StatusBadRequest)
			return true
		}
		s.robots.allow(agent, path)
	case "clear":
		s.robots.clear()
	default:
		http.Error(w, "control: op must be disallow, allow, or clear", http.StatusBadRequest)
		return true
	}

	// Echo the resulting robots.txt so the caller can confirm the flip.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, renderRobots(s.baseRobots(), s.robots.snapshot()))
	return true
}
