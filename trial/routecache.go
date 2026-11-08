package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// A cache for PREFLIGHT VALIDATION ONLY.
//
// The preflight answers a question about a *route* — does provider P, with model
// M and tool T, actually cause that vendor's own fetcher to retrieve a page? —
// and that answer does not change between runs. Re-running run-pilot.sh six
// times in an afternoon paid for the same validation six times.
//
// THE TRIALS ARE NEVER CACHED, and must not be. A trial's whole purpose is to be
// a real call at a real moment so the server log can show whether a fetch
// followed. Serving a trial from cache would produce a timestamped record of a
// call that never happened, with no fetch behind it — indistinguishable in the
// data from a genuine cache hit at the provider. That would not save money; it
// would fabricate results.
//
// Two safety rules:
//   - Only SUCCESSES are cached. A failure is what you are actively fixing, so
//     it is always retried.
//   - Entries expire, and a cached answer is always labelled with its age, so a
//     route that has since broken cannot hide behind an old pass.

type routeResult struct {
	Route      string    `json:"route"` // provider|model|tool
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	Tool       string    `json:"tool"`
	Fetched    bool      `json:"fetched"`     // the answer carried the page marker
	Attrib     string    `json:"attribution"` // who fetched, verbatim
	AttribOK   bool      `json:"attribution_ok"`
	VerifiedAt time.Time `json:"verified_at"`
}

type routeCache struct {
	mu      sync.Mutex
	path    string
	ttl     time.Duration
	entries map[string]routeResult
	dirty   bool
}

func routeKey(provider, model, tool string) string {
	return provider + "|" + model + "|" + tool
}

func openRouteCache(path string, ttl time.Duration) *routeCache {
	c := &routeCache{path: path, ttl: ttl, entries: map[string]routeResult{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return c // absent or unreadable: start empty, this is only a cache
	}
	var list []routeResult
	if err := json.Unmarshal(raw, &list); err != nil {
		return c
	}
	for _, e := range list {
		c.entries[e.Route] = e
	}
	return c
}

// Get returns a cached success if one is present and still fresh, plus its age.
func (c *routeCache) Get(provider, model, tool string) (routeResult, time.Duration, bool) {
	if c == nil {
		return routeResult{}, 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[routeKey(provider, model, tool)]
	if !ok {
		return routeResult{}, 0, false
	}
	age := time.Since(e.VerifiedAt)
	if age > c.ttl {
		return routeResult{}, age, false
	}
	return e, age, true
}

// Put records a successful validation. Failures are deliberately not stored.
// "Success" means the MODEL demonstrably had the content, i.e. the marker was in
// the answer. A fetch observed at our server is not sufficient: measured with
// Gemini, the fetcher took the page with HTTP 200 and the model still reported it
// could not access the URL. Our log proves only that we wrote the bytes — not that
// the provider received them, and not that the model was given them.
func (c *routeCache) Put(e routeResult) {
	if c == nil || !e.Fetched {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e.Route = routeKey(e.Provider, e.Model, e.Tool)
	e.VerifiedAt = time.Now()
	c.entries[e.Route] = e
	c.dirty = true
}

func (c *routeCache) Save() error {
	if c == nil || !c.dirty {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	list := make([]routeResult, 0, len(c.entries))
	for _, e := range c.entries {
		list = append(list, e)
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(c.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(c.path, raw, 0o644)
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%.1fh ago", d.Hours())
	}
}
