package main

import (
	"aisearch-probe/internal/probe"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// oneRound is a minimal schedule for tests that only care about arm wiring.
var oneRound = []time.Duration{0}

func TestParseSchedule(t *testing.T) {
	got, err := parseSchedule("0s, 2m ,10m,1h")
	if err != nil {
		t.Fatalf("parseSchedule: %v", err)
	}
	want := []time.Duration{0, 2 * time.Minute, 10 * time.Minute, time.Hour}
	if len(got) != len(want) {
		t.Fatalf("got %d offsets, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("offset %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestParseScheduleRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "nonsense", "-5m", "2 minutes"} {
		if _, err := parseSchedule(in); err == nil {
			t.Errorf("parseSchedule(%q) should have failed", in)
		}
	}
}

func TestNearestSuggestsSimilarModelIDs(t *testing.T) {
	have := []string{"claude-opus-4-8", "claude-sonnet-5", "gpt-5", "gpt-5-mini"}
	got := nearest("claude-opus-5", have)
	if len(got) == 0 {
		t.Fatal("no suggestions for a claude-* model")
	}
	for _, g := range got {
		if !strings.HasPrefix(g, "claude") {
			t.Errorf("suggested %q for a claude model", g)
		}
	}
}

func TestPathOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://x.test/method.html": "/method.html",
		"https://x.test":             "/",
	} {
		if got := pathOf(in); got != want {
			t.Errorf("pathOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// Two Google routes with different credential shapes, and the report must keep
// them apart: "google" is the Developer API (a plain key, free tier, no admin)
// and "google-vertex" is Vertex AI (a service-account FILE, needs GCP billing).
func TestGoogleRoutesAreSeparateAndUseTheirOwnCredentials(t *testing.T) {
	for _, v := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY",
		"PERPLEXITY_API_KEY", "OPENROUTER_API_KEY", "GEMINI_API_KEY",
		"GOOGLE_APPLICATION_CREDENTIALS"} {
		t.Setenv(v, "")
	}

	find := func(ps []Caller, name string) *Caller {
		for i := range ps {
			if ps[i].Name == name {
				return &ps[i]
			}
		}
		return nil
	}

	// A key alone enables the Developer API route, not the Vertex one.
	t.Setenv("GEMINI_API_KEY", "k")
	ps := callers("m", "m", "m", "m", nil)
	if g := find(ps, "google"); g == nil || !g.Enabled {
		t.Error("google (Developer API) should be enabled by GEMINI_API_KEY alone")
	} else if g.EnvKey != "GEMINI_API_KEY" {
		t.Errorf("google reads %q, want GEMINI_API_KEY", g.EnvKey)
	}
	if gv := find(ps, "google-vertex"); gv == nil || gv.Enabled {
		t.Error("google-vertex must NOT be enabled without a credentials file")
	}
}

// The Vertex credential is a FILE path, not a secret. A set-but-unreadable path
// must not count as enabled — otherwise the run starts and fails on first call.
func TestGoogleVertexRequiresReadableCredentialsFile(t *testing.T) {
	for _, v := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY",
		"PERPLEXITY_API_KEY", "OPENROUTER_API_KEY", "GEMINI_API_KEY"} {
		t.Setenv(v, "")
	}

	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/definitely/not/here.json")
	for _, p := range callers("m", "m", "m", "m", nil) {
		if p.Name == "google-vertex" && p.Enabled {
			t.Error("google-vertex enabled despite an unreadable credentials file")
		}
	}

	f := t.TempDir() + "/sa.json"
	if err := os.WriteFile(f, []byte(`{"client_email":"x@y.iam.gserviceaccount.com","private_key":"k","project_id":"p"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", f)
	found := false
	for _, p := range callers("m", "m", "m", "m", nil) {
		if p.Name == "google-vertex" {
			found = true
			if !p.Enabled {
				t.Error("google-vertex not enabled despite a readable credentials file")
			}
		}
	}
	if !found {
		t.Error("google-vertex provider missing from the list")
	}
}

func TestLoadServiceAccountRejectsWrongFileShape(t *testing.T) {
	dir := t.TempDir()

	notJSON := dir + "/a.json"
	os.WriteFile(notJSON, []byte("this is not json"), 0o600)
	if _, err := loadServiceAccount(notJSON); err == nil {
		t.Error("non-JSON file should be rejected")
	}

	// A valid JSON file that is not a service-account key — e.g. an OAuth
	// client secret, which is easy to grab by mistake.
	wrongKind := dir + "/b.json"
	os.WriteFile(wrongKind, []byte(`{"installed":{"client_id":"x"}}`), 0o600)
	if _, err := loadServiceAccount(wrongKind); err == nil {
		t.Error("a non-service-account JSON should be rejected with a clear error")
	}
}

func TestLoadServiceAccountDefaultsTokenURI(t *testing.T) {
	f := t.TempDir() + "/sa.json"
	os.WriteFile(f, []byte(`{"client_email":"x@y.iam.gserviceaccount.com","private_key":"k","project_id":"proj"}`), 0o600)
	sa, err := loadServiceAccount(f)
	if err != nil {
		t.Fatalf("loadServiceAccount: %v", err)
	}
	if sa.TokenURI == "" {
		t.Error("token_uri should default when absent")
	}
	if sa.ProjectID != "proj" {
		t.Errorf("ProjectID = %q, want proj", sa.ProjectID)
	}
}

func TestVertexEndpoint(t *testing.T) {
	global := vertexEndpoint("proj", "global", "gemini-2.5-pro")
	if !strings.Contains(global, "//aiplatform.googleapis.com/") {
		t.Errorf("global endpoint should use the unprefixed host: %s", global)
	}
	regional := vertexEndpoint("proj", "us-central1", "gemini-2.5-pro")
	if !strings.Contains(regional, "//us-central1-aiplatform.googleapis.com/") {
		t.Errorf("regional endpoint should use the region-prefixed host: %s", regional)
	}
	if !strings.Contains(regional, "/locations/us-central1/") {
		t.Errorf("region missing from path: %s", regional)
	}
}

// A model that writes the marker in a different case has still read the page.
// Counting that as "did not fetch" would manufacture a false caching finding —
// the most damaging error this tool can make.
func TestMarkerMatchIsCaseInsensitive(t *testing.T) {
	if !containsFold("the code is rcp-2026-08 apparently", "RCP-2026-08") {
		t.Error("lower-case marker in the answer should still count as found")
	}
	if !containsFold("Reference: RCP-2026-08.", "rcp-2026-08") {
		t.Error("marker matching must ignore case in both directions")
	}
	if containsFold("no code here", "RCP-2026-08") {
		t.Error("absent marker must not match")
	}
	if containsFold("anything at all", "") {
		t.Error("an empty marker must never match — no marker means no evidence, not proof")
	}
}

// The repeat arm only ever asks question 0, against its own page — so question
// 0 must be answerable from every page in the site.
func TestQuestionZeroIsAnswerableFromEveryPage(t *testing.T) {
	for _, name := range []string{"index.html", "method.html", "metrics.html", "limitations.html"} {
		page, err := os.ReadFile("../probe-site/" + name)
		if err != nil {
			t.Skipf("probe-site not readable: %v", err)
		}
		if !strings.Contains(strings.ToLower(string(page)), "reference") {
			t.Errorf("%s has no study reference code; the repeat arm's question is unanswerable there", name)
		}
	}
}

// engine:native is the whole reason OpenRouter is usable for this study. Any
// other engine means OpenRouter fetches the page itself and the run measures
// OpenRouter, not the vendor.
func TestOpenRouterPinsNativeEngine(t *testing.T) {
	src, err := os.ReadFile("openrouter.go")
	if err != nil {
		t.Fatalf("read openrouter.go: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, `"engine": "native"`) {
		t.Error(`engine is not pinned to "native" — the vendor's own fetcher would not be used`)
	}
	for _, wrong := range []string{`"engine": "auto"`, `"engine": "openrouter"`, `"engine": "exa"`, `"engine": "parallel"`} {
		if strings.Contains(body, wrong) {
			t.Errorf("found %s — that engine makes OpenRouter fetch the page, voiding the measurement", wrong)
		}
	}
}

// Each OpenRouter model becomes its own provider entry, named by vendor, so the
// report never merges two vendors and never confuses or:openai with openai.
func TestOpenRouterProvidersAreNamedPerVendor(t *testing.T) {
	ps := openRouterCallers([]string{
		"openai/gpt-5.5", "anthropic/claude-opus-4-8", "x-ai/grok-4.6",
	})
	if len(ps) != 3 {
		t.Fatalf("got %d providers, want 3", len(ps))
	}
	want := map[string]string{
		"or:openai":    "openai/gpt-5.5",
		"or:anthropic": "anthropic/claude-opus-4-8",
		"or:x-ai":      "x-ai/grok-4.6",
	}
	for _, p := range ps {
		model, ok := want[p.Name]
		if !ok {
			t.Errorf("unexpected provider name %q", p.Name)
			continue
		}
		if p.Model != model {
			t.Errorf("%s has model %q, want %q", p.Name, p.Model, model)
		}
		if p.EnvKey != "OPENROUTER_API_KEY" {
			t.Errorf("%s reads %q, want OPENROUTER_API_KEY", p.Name, p.EnvKey)
		}
		if p.Name == "openai" {
			t.Error("an OpenRouter entry must not be named plainly 'openai' — it would be indistinguishable from the direct account, which is the calibration control")
		}
	}
}

func TestVendorOf(t *testing.T) {
	for in, want := range map[string]string{
		"openai/gpt-5.5":            "openai",
		"anthropic/claude-opus-4-8": "anthropic",
		"nosuffix":                  "nosuffix",
	} {
		if got := vendorOf(in); got != want {
			t.Errorf("vendorOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// A run that dies because credit ran out is void, not partial. The precheck
// must refuse only when it is confident the run cannot finish.
func TestCreditFitsRun(t *testing.T) {
	lim, rem := 25.0, 25.0
	c := &OpenRouterCredit{Usage: 0, Limit: &lim, Remaining: &rem}
	if ok, _ := c.FitsRun(24, 0.03); !ok {
		t.Error("$25 should comfortably cover a 24-call run")
	}

	lim2, rem2 := 25.0, 0.10
	tight := &OpenRouterCredit{Usage: 24.9, Limit: &lim2, Remaining: &rem2}
	if ok, need := tight.FitsRun(24, 0.03); ok {
		t.Errorf("$0.10 left should not cover a 24-call run needing $%.2f", need)
	}

	unlimited := &OpenRouterCredit{Usage: 5}
	if ok, _ := unlimited.FitsRun(10000, 0.03); !ok {
		t.Error("a key with no limit must never be reported as unable to fit")
	}
}

// A near miss must be suggested first. The naive version ranked alphabetically
// and hid the real answer: asked for "anthropic/claude-opus-4-8" it offered
// claude-3-haiku while "anthropic/claude-opus-4.8" sat further down the list.
func TestNearestSurfacesTheNearMissFirst(t *testing.T) {
	have := []string{
		"anthropic/claude-3-haiku",
		"anthropic/claude-fable-5",
		"anthropic/claude-haiku-4.5",
		"anthropic/claude-opus-4",
		"anthropic/claude-opus-4.8",
		"anthropic/claude-opus-5",
		"openai/gpt-5.5",
	}
	got := nearest("anthropic/claude-opus-4-8", have)
	if len(got) == 0 {
		t.Fatal("no suggestions")
	}
	if got[0] != "anthropic/claude-opus-4.8" {
		t.Errorf("first suggestion = %q, want anthropic/claude-opus-4.8 (dash vs dot is the usual slip)\nfull list: %v", got[0], got)
	}
	for _, g := range got {
		if strings.HasPrefix(g, "openai/") {
			t.Errorf("suggested %q for an anthropic model", g)
		}
	}
}

// Every default model must be a real id, or a run records data against a model
// the platform silently chose for us.
func TestDefaultOpenRouterModelsUseDotVersions(t *testing.T) {
	for _, m := range defaultOpenRouterModels {
		name := m
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		// A trailing "-<digit>" is the dashed-version style OpenRouter rejects.
		parts := strings.Split(name, "-")
		last := parts[len(parts)-1]
		if len(parts) > 1 && len(last) == 1 && last[0] >= '0' && last[0] <= '9' {
			prev := parts[len(parts)-2]
			if len(prev) > 0 && prev[len(prev)-1] >= '0' && prev[len(prev)-1] <= '9' {
				t.Errorf("%q looks like a dashed version (…-%s); OpenRouter uses dots", m, last)
			}
		}
	}
}

// A Gemini Developer API key supersedes the Vertex service-account route: both
// measure the same vendor, so enabling both would double-count Google — and
// Vertex additionally needs GCP billing that the key does not.
func TestGeminiKeySupersedesVertex(t *testing.T) {
	for _, v := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY",
		"PERPLEXITY_API_KEY", "OPENROUTER_API_KEY"} {
		t.Setenv(v, "")
	}
	f := t.TempDir() + "/sa.json"
	if err := os.WriteFile(f, []byte(`{"client_email":"x@y.iam.gserviceaccount.com","private_key":"k","project_id":"p"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", f)
	t.Setenv("GEMINI_API_KEY", "k")

	var enabled []string
	for _, p := range callers("m", "m", "m", "m", nil) {
		if p.Enabled {
			enabled = append(enabled, p.Name)
		}
	}
	has := func(n string) bool {
		for _, e := range enabled {
			if e == n {
				return true
			}
		}
		return false
	}
	if !has("google") {
		t.Errorf("google (Developer API) should be enabled; enabled=%v", enabled)
	}
	if has("google-vertex") {
		t.Errorf("google-vertex must be superseded by the key; enabled=%v", enabled)
	}

	// Without a key, Vertex is the fallback and must still work.
	t.Setenv("GEMINI_API_KEY", "")
	fallback := false
	for _, p := range callers("m", "m", "m", "m", nil) {
		if p.Name == "google-vertex" && p.Enabled {
			fallback = true
		}
	}
	if !fallback {
		t.Error("with no key, google-vertex must remain available as the fallback")
	}
}

// The cache exists to avoid paying twice for the same route validation. It must
// never cache a failure as a pass, and must expire.
func TestRouteCacheOnlyStoresSuccessesAndExpires(t *testing.T) {
	f := t.TempDir() + "/rc.json"
	c := openRouteCache(f, time.Hour)

	// A failure must not be stored.
	c.Put(routeResult{Provider: "or:openai", Model: "m", Tool: "t", Fetched: false})
	if _, _, hit := c.Get("or:openai", "m", "t"); hit {
		t.Error("a failed validation must never be cached — it is the thing being fixed")
	}

	// A success is stored, survives a reload, and reports its age.
	c.Put(routeResult{Provider: "or:openai", Model: "m", Tool: "t", Fetched: true, AttribOK: true})
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded := openRouteCache(f, time.Hour)
	e, age, hit := reloaded.Get("or:openai", "m", "t")
	if !hit {
		t.Fatal("a stored success should be found after reload")
	}
	if !e.Fetched || !e.AttribOK {
		t.Errorf("entry lost fields: %+v", e)
	}
	if age > time.Minute {
		t.Errorf("age = %v, expected near zero", age)
	}

	// An expired entry is a miss, so a route that has since broken cannot hide.
	expired := openRouteCache(f, 0)
	if _, _, hit := expired.Get("or:openai", "m", "t"); hit {
		t.Error("a zero TTL must expire everything")
	}
}

// Changing the model or the tool is a different route and must not reuse the
// previous validation — a wrong model id or wrong tool is exactly what the
// check is for.
func TestRouteCacheKeyIncludesModelAndTool(t *testing.T) {
	c := openRouteCache(t.TempDir()+"/rc.json", time.Hour)
	c.Put(routeResult{Provider: "or:anthropic", Model: "claude-opus-4.8",
		Tool: "openrouter:web_fetch", Fetched: true})

	if _, _, hit := c.Get("or:anthropic", "claude-opus-5", "openrouter:web_fetch"); hit {
		t.Error("a different model must not reuse the validation")
	}
	if _, _, hit := c.Get("or:anthropic", "claude-opus-4.8", "openrouter:web_search"); hit {
		t.Error("a different tool must not reuse the validation")
	}
	if _, _, hit := c.Get("or:anthropic", "claude-opus-4.8", "openrouter:web_fetch"); !hit {
		t.Error("the same route should hit")
	}
}

// A route proven by the server observing the vendor's own fetcher must be
// treated as validated even when the model did not echo the marker back. The
// marker is an indirect proxy for retrieval; a confirmed fetch is direct.
func TestRouteNeedsTheContentToReachTheModel(t *testing.T) {
	c := openRouteCache(t.TempDir()+"/rc.json", time.Hour)

	// A fetch our server saw is NOT enough: measured with Gemini, the fetcher
	// took the page with HTTP 200 and the model still could not access it. Only
	// the marker shows the content reached the model.
	c.Put(routeResult{Provider: "google", Model: "gemini-3.1-flash-lite",
		Fetched: false, AttribOK: true})
	if _, _, hit := c.Get("google", "gemini-3.1-flash-lite", ""); hit {
		t.Error("a fetch that never reached the model must not count as a working route")
	}

	// Neither: nothing to remember.
	c.Put(routeResult{Provider: "nobody", Model: "m", Fetched: false, AttribOK: false})
	if _, _, hit := c.Get("nobody", "m", ""); hit {
		t.Error("a route with no evidence of retrieval must not be cached")
	}
}

// A failed retrieval must never reach the report as an answer. Gemini returns
// prose ("I am unable to access the requested URL") alongside a machine-readable
// failure status; scoring the prose would file a broken route under "the model
// did not fetch", which is the same shape in the data as a cache hit.
func TestGeminiRetrievalFailureIsAnError(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"parts":[{"text":
		"I am sorry, but I am unable to access the requested URL."}]},
		"urlContextMetadata":{"urlMetadata":[
		  {"retrievedUrl":"https://x.trycloudflare.com/method.html",
		   "urlRetrievalStatus":"URL_RETRIEVAL_STATUS_ERROR"}]}}]}`)
	if _, err := parseGeminiResponse(body); err == nil {
		t.Fatal("a URL_RETRIEVAL_STATUS_ERROR response parsed as a valid answer")
	} else if !strings.Contains(err.Error(), "URL_RETRIEVAL_STATUS_ERROR") {
		t.Fatalf("the error should name the status Google gave; got: %v", err)
	}
}

func TestGeminiSuccessAndMissingMetadataBothParse(t *testing.T) {
	// Success: the status is present and good.
	ok := []byte(`{"candidates":[{"content":{"parts":[{"text":"code RCP-2026-08"}]},
		"urlContextMetadata":{"urlMetadata":[{"retrievedUrl":"https://x/y",
		"urlRetrievalStatus":"URL_RETRIEVAL_STATUS_SUCCESS"}]}}]}`)
	if got, err := parseGeminiResponse(ok); err != nil || !strings.Contains(got, "RCP-2026-08") {
		t.Fatalf("success case: got %q, err %v", got, err)
	}
	// Absent metadata is not failure — the field is missing on API versions that
	// do not send it, and on answers where the tool was never invoked.
	none := []byte(`{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`)
	if got, err := parseGeminiResponse(none); err != nil || got != "hello" {
		t.Fatalf("absent metadata should parse: got %q, err %v", got, err)
	}
}

// Google is never turned off for its address; instead every run with a Google
// route prints the tunnel warning, and a run without one does not.
func TestGoogleWarningOnlyWhenGoogleRuns(t *testing.T) {
	if !warnGoogle([]Caller{{Name: "openai"}, {Name: "google"}}) {
		t.Error("a run with Google must print the tunnel warning")
	}
	if !warnGoogle([]Caller{{Name: "google-vertex"}}) {
		t.Error("the Vertex route is Google too and must print the warning")
	}
	if warnGoogle([]Caller{{Name: "openai"}, {Name: "or:x-ai"}}) {
		t.Error("a run without Google must not print the warning")
	}
}

// The liveness check must be right in both directions: a live page with the
// marker passes, and every way the page can be wrong is caught. A false "page is
// fine" is the expensive error — it lets a dead-tunnel round be scored.
func TestPageLivenessDetectsEachFailure(t *testing.T) {
	const marker = "RCP-2026-08"

	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); !strings.Contains(got, "probe-liveness") {
			t.Errorf("liveness check must identify itself, got %q", got)
		}
		fmt.Fprintf(w, "<html><footer>study reference code %s</footer></html>", marker)
	}))
	defer live.Close()
	if l := checkPageLive(context.Background(), live.URL, marker); !l.OK || !l.Marker {
		t.Errorf("a healthy page should pass: %+v", l)
	}

	// 200, but somebody else's page — a recycled tunnel address serves this.
	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>unrelated service</html>")
	}))
	defer wrong.Close()
	if l := checkPageLive(context.Background(), wrong.URL, marker); l.OK {
		t.Error("a 200 without the marker must not pass — it may not be our page")
	}

	// An error status.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer bad.Close()
	if l := checkPageLive(context.Background(), bad.URL, marker); l.OK || l.Status != 502 {
		t.Errorf("a 502 must fail and record its status: %+v", l)
	}

	// Nothing listening at all — the dead-tunnel case.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := dead.URL
	dead.Close()
	l := checkPageLive(context.Background(), addr, marker)
	if l.OK || l.Err == "" {
		t.Errorf("an unreachable page must fail with a reason: %+v", l)
	}
	// And it must be distinguishable from "never checked", or old logs get
	// thrown away.
	if l.CheckAt == "" {
		t.Error("a failed check must still record that a check happened")
	}
}

// The lock exists because the rule it enforces was broken by hand, mid-run.
// It must refuse a second run, survive a crash, and never block on debris.
func TestRunLockRefusesASecondRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".probe-run.lock")

	first, err := acquireRunLock(path, "staleness experiment", []string{"probe-site/staleness.html"})
	if err != nil {
		t.Fatalf("first run could not take the lock: %v", err)
	}

	// A second run must be refused, and the message must say what holds it and
	// which files are at stake — otherwise the operator cannot act on it.
	_, err = acquireRunLock(path, "retention run", []string{"probe-site/"})
	if err == nil {
		t.Fatal("a second run took the lock while the first was live")
	}
	for _, want := range []string{"another run is in progress", "staleness experiment", "staleness.html"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q; got: %v", want, err)
		}
	}

	// Released, the lock must not block anyone.
	first.Release()
	second, err := acquireRunLock(path, "retention run", []string{"probe-site/"})
	if err != nil {
		t.Fatalf("the lock still blocks after release: %v", err)
	}
	second.Release()
}

// A crashed run leaves a lock behind. That must not block the next run for ever
// — the process is gone, so the file is debris.
func TestRunLockIgnoresDebrisFromADeadProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".probe-run.lock")
	// PID 0 is never a live process we could signal.
	if err := os.WriteFile(path,
		[]byte(`{"pid":0,"what":"crashed run","started":"2026-01-01T00:00:00Z","owns":["x"]}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	l, err := acquireRunLock(path, "retention run", []string{"probe-site/"})
	if err != nil {
		t.Fatalf("a dead run's lock blocked a new one: %v", err)
	}
	l.Release()

	// Unparseable debris must also not block.
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	l2, err := acquireRunLock(path, "retention run", nil)
	if err != nil {
		t.Fatalf("corrupt lock file blocked a new run: %v", err)
	}
	l2.Release()
}

// The dry-run backstop must make a paid call impossible: after wrapping, a
// provider's Ask returns an error and never invokes the real function.
func TestDryRunBackstopRefusesEveryProviderCall(t *testing.T) {
	realCalled := false
	provs := []Caller{
		{Name: "p1", Ask: func(ctx context.Context, key, model, url, q string, force bool) (string, probe.Usage, error) {
			realCalled = true
			return "should not happen", probe.Usage{}, nil
		}},
		{Name: "p2", Ask: func(ctx context.Context, key, model, url, q string, force bool) (string, probe.Usage, error) {
			realCalled = true
			return "should not happen", probe.Usage{}, nil
		}},
	}
	refuseUnderDryRun(provs)
	for _, p := range provs {
		out, _, err := p.Ask(context.Background(), "k", "m", "https://x/y", "q", true)
		if err == nil {
			t.Errorf("%s: Ask returned nil error under dry-run; a paid call could slip through", p.Name)
		}
		if out != "" {
			t.Errorf("%s: Ask returned content %q under dry-run", p.Name, out)
		}
	}
	if realCalled {
		t.Error("the real Ask was invoked under dry-run — the backstop did not replace it")
	}
}

// POLICY (BULLETIN 2026-10-11): every Anthropic call uses Sonnet 5. Haiku cannot
// fetch a page (direct: nothing; OpenRouter: its own fetcher answers instead), so it
// must never be what a run gets by leaving -anthropicModel unset.
func TestDefaultAnthropicModelIsSonnet(t *testing.T) {
	if defaultAnthropicModel != "claude-sonnet-5" {
		t.Errorf("default Anthropic model = %q, want claude-sonnet-5", defaultAnthropicModel)
	}
}
