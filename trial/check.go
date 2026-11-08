package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"aisearch-probe/internal/probe"
)

// Preflight. Provider APIs drift — model ids get retired, tool type strings get
// versioned, endpoints move. Discovering that three hours into a timed run
// costs the run. This makes one real call per provider first and says plainly
// which ones are usable.

type checkResult struct {
	Provider    string
	KeyPresent  bool
	ModelsOK    bool
	ModelValid  bool
	ModelsErr   string
	CallOK      bool
	CallErr     string
	Attrib      string // who actually fetched, from the probe log
	AttribOK    bool
	Cached      bool // answered from a previous validation, not re-tested
	CachedAge   time.Duration
	AttribHard  bool // an aggregator fetched, or nothing did — a hard failure
	Fetched     bool // did the answer show evidence it actually retrieved the page?
	Suggestions []string
	Elapsed     time.Duration
}

func httpGet(ctx context.Context, endpoint string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%d: %s", resp.StatusCode, truncate(string(out), 400))
	}
	return out, nil
}

// listModels returns the model ids a provider will admit to. Perplexity has no
// public listing endpoint, so it returns nil and the live call is the only test.
func listModels(ctx context.Context, provider, key string) ([]string, error) {
	type modelList struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	var (
		raw []byte
		err error
	)
	switch provider {
	case "anthropic":
		raw, err = httpGet(ctx, "https://api.anthropic.com/v1/models?limit=100", map[string]string{
			"x-api-key": key, "anthropic-version": "2023-06-01",
		})
	case "openai":
		raw, err = httpGet(ctx, "https://api.openai.com/v1/models", map[string]string{
			"Authorization": "Bearer " + key,
		})
	case "or":
		raw, err = httpGet(ctx, openRouterBase+"/models", nil) // public, no key needed
	case "google":
		// Developer API lists models as "models/gemini-2.5-pro"; normalise below.
		raw, err = httpGet(ctx, geminiDirectBase+"/models",
			map[string]string{"x-goog-api-key": key})
	default:
		return nil, nil // no listing endpoint (perplexity, google/vertex)
	}
	if err != nil {
		return nil, err
	}
	var parsed modelList
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(parsed.Data)+len(parsed.Models))
	for _, m := range parsed.Data {
		ids = append(ids, m.ID)
	}
	// Gemini Developer API shape: {"models":[{"name":"models/gemini-2.5-pro"}]}
	for _, m := range parsed.Models {
		ids = append(ids, strings.TrimPrefix(m.Name, "models/"))
	}
	sort.Strings(ids)
	return ids, nil
}

// nearest turns "model not found" into "did you mean", ranked by how much of the
// requested id a candidate actually shares.
//
// The naive version returned the first six alphabetical matches on the vendor
// prefix, which hid the real answer: asked for "anthropic/claude-opus-4-8" it
// listed claude-3-haiku and claude-fable-5 while the correct
// "anthropic/claude-opus-4.8" sat further down. Version separators differ
// between vendors' own APIs and OpenRouter (dash vs dot), so a near miss is the
// most likely mistake and must be surfaced first.
func commonPrefixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func nearest(want string, have []string) []string {
	lw := strings.ToLower(want)
	// Candidates must share a coarse prefix with the request. OpenRouter ids are
	// "vendor/model"; a direct provider's own /models returns a bare
	// "claude-opus-4.8", so fall back to the first token before a dash.
	prefix := lw
	if i := strings.IndexByte(prefix, '/'); i > 0 {
		prefix = prefix[:i+1]
	} else if i := strings.IndexByte(prefix, '-'); i > 0 {
		prefix = prefix[:i]
	}
	type scored struct {
		id string
		n  int
	}
	var cands []scored
	for _, h := range have {
		lh := strings.ToLower(h)
		if !strings.HasPrefix(lh, prefix) {
			continue
		}
		// Compare ignoring the dash/dot distinction, which is the usual slip.
		norm := func(s string) string { return strings.ReplaceAll(s, "-", ".") }
		cands = append(cands, scored{h, commonPrefixLen(norm(lh), norm(lw))})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].n != cands[j].n {
			return cands[i].n > cands[j].n
		}
		return cands[i].id < cands[j].id
	})
	out := make([]string, 0, 6)
	for i, c := range cands {
		if i == 6 {
			break
		}
		out = append(out, c.id)
	}
	return out
}

// runCheck exercises each provider end to end against a real URL, so the check
// covers the tunnel and the page as well as the API.
// probeLogPath is set by -probe-log so the check can see who actually fetched.
var probeLogPath string

// checkCache skips re-validating a route that passed recently. Preflight only —
// never the trials. See routecache.go for why that line matters.
var checkCache *routeCache

func runCheck(provs []Caller, url, marker string, force bool) bool {
	fmt.Printf("Checking providers against %s\n\n", url)

	// If an OpenRouter key is in play, report what it has left. A run that dies
	// part-way because credit ran out is not a partial result, it is a void one.
	if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		c, err := openRouterCredit(ctx, key)
		cancel()
		switch {
		case err != nil:
			fmt.Printf("  OPENROUTER CREDIT: could not read (%s)\n\n", truncate(err.Error(), 120))
		default:
			fmt.Printf("  OPENROUTER CREDIT: %s\n", c)
			// 3 cents per call is deliberately pessimistic: the measured blended
			// figure is about 2 cents (see PROVIDERS.md).
			if ok, need := c.FitsRun(24, 0.03); !ok {
				fmt.Printf("  ⚠ a 24-call run needs about $%.2f — top up before starting\n", need)
			}
			fmt.Println()
		}
	}

	results := make([]checkResult, 0, len(provs))
	for _, p := range provs {
		r := checkResult{Provider: p.Name, KeyPresent: p.Enabled}
		if !p.Enabled {
			results = append(results, r)
			continue
		}
		// A route that passed recently need not be paid for again. Only
		// successes are cached, and the age is always shown.
		tool := ""
		if strings.HasPrefix(p.Name, "or:") {
			tool = toolFor(p.Model)
		}
		if e, age, hit := checkCache.Get(p.Name, p.Model, tool); hit {
			r.ModelsOK, r.ModelValid, r.CallOK = true, true, true
			r.Fetched, r.Attrib, r.AttribOK = e.Fetched, e.Attrib, e.AttribOK
			r.Cached, r.CachedAge = true, age
			results = append(results, r)
			continue
		}

		key := os.Getenv(p.EnvKey) // for google this is a file path, not a secret
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)

		// OpenRouter entries are named "or:<vendor>"; they share one model list.
		listKind := p.Name
		if strings.HasPrefix(p.Name, "or:") {
			listKind = "or"
		}
		models, err := listModels(ctx, listKind, key)
		switch {
		case err != nil:
			r.ModelsErr = err.Error()
		case models == nil:
			r.ModelsOK = true   // no listing endpoint; not an error
			r.ModelValid = true // can only be judged by the live call
		default:
			r.ModelsOK = true
			for _, m := range models {
				if m == p.Model {
					r.ModelValid = true
					break
				}
			}
			if !r.ModelValid {
				r.Suggestions = nearest(p.Model, models)
			}
		}

		// A unique query token per provider: two providers must never be able to
		// satisfy each other's fetch. Two routes to the same vendor share that
		// vendor's cache, so without this the second route legitimately does not
		// fetch and the check reports a failure that is really our own doing.
		// The probe strips the query when deriving page_key, so logging and
		// analysis are unaffected.
		probeURL, token := probeURLFor(url, p.Tag())

		start := time.Now()
		startUnix := float64(start.UnixNano()) / 1e9
		answer, _, err := p.Ask(ctx, key, p.Model, probeURL,
			"In one sentence, what is this page about? Quote any reference code it contains.", force)
		r.Elapsed = time.Since(start)
		cancel()

		if err != nil {
			r.CallErr = err.Error()
		} else {
			r.CallOK = true
			r.Fetched = containsFold(answer, marker)
		}

		// The API response cannot say WHO fetched. The server log can, and this
		// is the only way to catch an aggregator fetching on a vendor's behalf —
		// which silently turns the result into one about the aggregator.
		if probeLogPath != "" {
			if recs, _, lerr := probe.LoadRecords(probeLogPath, "", time.Time{}); lerr == nil {
				at := probe.AttributeFetch(p.Name, recs, pathOf(probeURL), startUnix, 120, token)
				r.Attrib = at.Verdict()
				r.AttribOK = at.OK
				r.AttribHard = at.Aggregat || at.Nothing
			}
		}
		// A route is proven either by the marker OR by the server seeing that
		// vendor's own fetcher — the latter is the stronger evidence.
		// Only the marker proves the model had the content. A fetch that reached
		// our server but never reached the model is not a working route.
		if r.CallOK && r.Fetched {
			checkCache.Put(routeResult{
				Provider: p.Name, Model: p.Model, Tool: tool,
				Fetched: true, Attrib: r.Attrib, AttribOK: r.AttribOK,
			})
		}
		results = append(results, r)
	}
	if err := checkCache.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "[trial] could not save the route cache: %v\n", err)
	}

	allGood := true
	for _, r := range results {
		fmt.Printf("  %s\n", strings.ToUpper(r.Provider))
		if !r.KeyPresent {
			fmt.Printf("    skipped — no API key in the environment\n\n")
			continue
		}
		if r.ModelsErr != "" {
			fmt.Printf("    model list   : could not read (%s)\n", truncate(r.ModelsErr, 120))
		} else if !r.ModelValid {
			allGood = false
			fmt.Printf("    model        : \033[31mNOT a real model id on this platform\033[0m\n")
			fmt.Printf("                   If the live call below still succeeds, the platform\n")
			fmt.Printf("                   substituted a model we cannot name — the results would\n")
			fmt.Printf("                   not say which model produced them. Fix the id.\n")
			fmt.Printf("                   (version separators differ: OpenRouter uses dots, some\n")
			fmt.Printf("                   vendor APIs use dashes)\n")
			if len(r.Suggestions) > 0 {
				fmt.Printf("                   try one of: %s\n", strings.Join(r.Suggestions, ", "))
			}
		} else {
			fmt.Printf("    model        : accepted\n")
		}

		if r.Cached {
			fmt.Printf("    route        : \033[36mvalidated %s — not re-tested, no API call made\033[0m\n",
				humanAge(r.CachedAge))
			if r.Attrib != "" {
				fmt.Printf("    who fetched  : %s\n", r.Attrib)
			}
			fmt.Printf("    fetched page : YES (from that earlier validation)\n")
			fmt.Println()
			continue
		}
		if r.CallOK {
			fmt.Printf("    live call    : worked (%.1fs)\n", r.Elapsed.Seconds())
			if r.Attrib != "" {
				switch {
				case r.AttribOK:
					fmt.Printf("    who fetched  : %s\n", r.Attrib)
				case r.AttribHard:
					allGood = false
					fmt.Printf("    who fetched  : \033[31m%s\033[0m\n", r.Attrib)
				default:
					fmt.Printf("    who fetched  : \033[33m%s\033[0m\n", r.Attrib)
				}
			}
			if r.Fetched {
				fmt.Printf("    fetched page : YES — the answer quoted the page marker\n")
			} else if r.AttribOK {
				// CORRECTED 2026-08-31. This branch used to pass, on the reasoning
				// that a confirmed fetch is stronger evidence than the marker. That
				// was wrong. Measured with Gemini: its fetcher took the page with
				// HTTP 200 and the full body, yet the model answered "I am unable to
				// access the requested URL".
				//
				// A 200 in our log means OUR PROCESS WROTE THE BYTES. It does not
				// establish that the provider received them, nor that the model was
				// given them; and the fetch may even belong to unrelated activity by
				// that vendor. Server-side logging cannot separate those cases. The
				// marker in the answer is the only evidence the MODEL held the
				// content — so when the two disagree, the marker wins.
				//
				// The known instance had a documented cause: Google's urlContext does
				// not support tunneling domains, and our host is *.trycloudflare.com.
				// Hence the wording below states the observation, not a mechanism.
				allGood = false
				fmt.Printf("    fetched page : \033[31mFETCHED BUT NO CONTENT IN ANSWER\033[0m — this vendor's fetcher\n")
				fmt.Printf("                   took the page from us, but the answer shows no sign of it.\n")
				fmt.Printf("                   Either the content never reached the model, or that fetch\n")
				fmt.Printf("                   was unrelated activity. Caching cannot be measured here:\n")
				fmt.Printf("                   every round would fetch, which is the failure path rather\n")
				fmt.Printf("                   than the behaviour under study.\n")
				fmt.Printf("                   For Gemini on a *.trycloudflare.com host this is expected —\n")
				fmt.Printf("                   tunneling domains are unsupported by urlContext (PROVIDERS.md).\n")
			} else {
				allGood = false
				fmt.Printf("    fetched page : NO — no marker in the answer AND no fetch by this\n")
				fmt.Printf("                   vendor reached the server.\n")
				fmt.Printf("                   Check the URL is publicly reachable and the tunnel is up.\n")
			}
		} else {
			allGood = false
			fmt.Printf("    live call    : FAILED\n")
			fmt.Printf("                   %s\n", truncate(r.CallErr, 500))
		}
		fmt.Println()
	}

	if allGood {
		fmt.Println("All checked providers are usable. Safe to start a run.")
	} else {
		fmt.Println("Some providers are not usable. Fix the above before starting a timed run —")
		fmt.Println("a provider that fails mid-run leaves a gap you cannot re-collect.")
	}
	return allGood
}
