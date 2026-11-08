package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"aisearch-probe/internal/probe"
)

// Google is the odd one out: no API key, a service-account JSON file instead
// (the file GOOGLE_APPLICATION_CREDENTIALS points at). The file is exchanged
// for a short-lived access token via the JWT-bearer flow, which is done here
// with the standard library so the binary stays dependency-free.

type serviceAccount struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

func loadServiceAccount(path string) (*serviceAccount, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading credentials file: %w", err)
	}
	var sa serviceAccount
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("credentials file is not JSON: %w", err)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, fmt.Errorf("credentials file has no client_email/private_key — is it a service account key?")
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return &sa, nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signedJWT builds and signs the assertion Google exchanges for an access token.
func (sa *serviceAccount) signedJWT(scope string) (string, error) {
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return "", fmt.Errorf("private_key is not valid PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// Older keys may be PKCS#1.
		if k, err1 := x509.ParsePKCS1PrivateKey(block.Bytes); err1 == nil {
			parsed = k
		} else {
			return "", fmt.Errorf("parsing private_key: %w", err)
		}
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("private_key is not an RSA key")
	}

	now := time.Now()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iss":   sa.ClientEmail,
		"scope": scope,
		"aud":   sa.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})

	signingInput := b64url(header) + "." + b64url(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("signing JWT: %w", err)
	}
	return signingInput + "." + b64url(sig), nil
}

var (
	tokenMu     sync.Mutex
	cachedToken string
	tokenExpiry time.Time
)

// accessToken returns a cached bearer token, refreshing when it is close to
// expiry. Cached because a long run makes many calls and each exchange is a
// round trip.
func accessToken(ctx context.Context, credPath string) (string, error) {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	if cachedToken != "" && time.Now().Before(tokenExpiry.Add(-2*time.Minute)) {
		return cachedToken, nil
	}

	sa, err := loadServiceAccount(credPath)
	if err != nil {
		return "", err
	}
	assertion, err := sa.signedJWT("https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return "", err
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sa.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding token response: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("token exchange failed: %s %s", out.Error, out.ErrorDesc)
	}
	cachedToken = out.AccessToken
	tokenExpiry = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return cachedToken, nil
}

// projectFromCredentials reads project_id out of the service account file, so
// the caller does not have to repeat it on the command line.
func projectFromCredentials(credPath string) string {
	sa, err := loadServiceAccount(credPath)
	if err != nil {
		return ""
	}
	return sa.ProjectID
}

var vertexRegion = "global"

func vertexEndpoint(project, region, model string) string {
	host := "aiplatform.googleapis.com"
	if region != "global" {
		host = region + "-aiplatform.googleapis.com"
	}
	return fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
		host, project, region, model)
}

// askGoogle calls Gemini on Vertex AI with Google Search grounding enabled.
// Grounding has no strict "you must search" switch, so force is accepted and
// ignored — the marker check is what tells you whether it really fetched.
func askGoogle(ctx context.Context, credPath, model, pageURL, question string, force bool) (string, probe.Usage, error) {
	token, err := accessToken(ctx, credPath)
	if err != nil {
		return "", probe.Usage{}, err
	}
	project := projectFromCredentials(credPath)
	if project == "" {
		return "", probe.Usage{}, fmt.Errorf("no project_id in the credentials file")
	}

	// url_context reads a URL we name; googleSearch queries Google's index.
	// For this study url_context is the right primitive — our probe page is in
	// no index, so search grounding finds nothing and never fetches (measured
	// 2026-08-31 via OpenRouter, where search returned no fetch at all).
	//
	// Google documents url_context as cache-first: "the tool first attempts to
	// fetch the content from an internal index cache … if a URL is not available
	// in the index, the tool automatically falls back to do a live fetch."
	// So a brand-new URL should produce a live fetch we can observe, and any
	// later reuse is the phenomenon under study — documented, like Anthropic's.
	body := geminiBody(pageURL, question)

	out, err := httpPost(ctx, vertexEndpoint(project, vertexRegion, model),
		map[string]string{"Authorization": "Bearer " + token}, body)
	if err != nil {
		return "", probe.Usage{}, err
	}
	text, perr := parseGeminiResponse(out)
	usage, _ := probe.UsageGemini(out)
	return text, usage, perr
}

// geminiBody is shared by both Google routes: Vertex AI (service account) and
// the Gemini Developer API (plain key). Same request shape, different endpoint
// and auth.
func geminiBody(pageURL, question string) map[string]any {
	// urlContext only. A search tool could answer from an index snapshot without
	// fetching, which would make "no request arrived" ambiguous.
	tools := []map[string]any{{"urlContext": map[string]any{}}}
	return map[string]any{
		"contents": []map[string]any{{
			"role": "user",
			"parts": []map[string]any{
				{"text": buildPrompt(pageURL, question)},
			},
		}},
		"tools": tools,
	}
}

// Gemini reports, per URL, whether its own retrieval succeeded:
// candidates[].urlContextMetadata.urlMetadata[].urlRetrievalStatus, one of
// URL_RETRIEVAL_STATUS_{UNSPECIFIED,SUCCESS,ERROR}. Source:
// https://docs.cloud.google.com/java/docs/reference/google-cloud-vertexai/latest/com.google.cloud.vertexai.api.UrlMetadata.UrlRetrievalStatus
// (retrieved 2026-08-31).
//
// This field is why the study does not have to guess. Without it, a Gemini run
// that answered "I am unable to access the requested URL" while our server
// logged a 200 was ambiguous between three stories: the bytes never arrived,
// Google retrieved and then discarded them, or the fetch we saw belonged to
// some other Google activity. The status says which, in Google's own words, and
// a mismatch between it and our access log is itself a reportable observation.
const (
	urlRetrievalSuccess = "URL_RETRIEVAL_STATUS_SUCCESS"
)

type urlRetrieval struct {
	URL    string `json:"retrievedUrl"`
	Status string `json:"urlRetrievalStatus"`
}

func parseGeminiResponse(out []byte) (string, error) {
	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason       string `json:"finishReason"`
			URLContextMetadata struct {
				URLMetadata []urlRetrieval `json:"urlMetadata"`
			} `json:"urlContextMetadata"`
		} `json:"candidates"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("gemini: %s", truncate(parsed.Error.Message, 300))
	}
	if len(parsed.Candidates) == 0 {
		return "", fmt.Errorf("no candidates in response: %s", truncate(string(out), 300))
	}

	// A failed retrieval is a broken route, not a shy model. Report it as the
	// error it is, naming the status, so it is never scored as "the model chose
	// not to fetch" or "the model reused a cached copy".
	if err := retrievalError(parsed.Candidates[0].URLContextMetadata.URLMetadata); err != nil {
		return "", err
	}

	var sb strings.Builder
	for _, p := range parsed.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	return sb.String(), nil
}

// retrievalError returns nil when every URL Gemini reports was retrieved
// successfully, or when it reports nothing at all (older API versions, and
// responses where the tool was never invoked — absence of the field is not
// evidence of failure).
func retrievalError(md []urlRetrieval) error {
	for _, u := range md {
		if u.Status != "" && u.Status != urlRetrievalSuccess {
			return fmt.Errorf("gemini urlContext could not retrieve %s: %s — "+
				"Google documents that tunneling services are unsupported by this tool; see PROVIDERS.md",
				u.URL, u.Status)
		}
	}
	return nil
}

const geminiDirectBase = "https://generativelanguage.googleapis.com/v1beta"

// geminiVendor uses the Gemini Developer API: a plain API key from AI Studio, no
// cloud project and no billing switch. Preferred for a pilot because it needs no
// administrator. (The billed Vertex route — askGoogle above — is deliberately left
// off the Vendor interface: its JWT-bearer exchange and project-in-URL are the odd
// shape the interface no longer carries. It remains only as the no-key fallback.)
//
// Gemini is the one vendor that folds the model into the URL, and — unlike the
// others — force is accepted and ignored: url_context has no "you must fetch"
// switch, so the marker check is what tells you whether it really fetched.
type geminiVendor struct{}

// The key goes in a header, never the query string — a URL can end up in logs,
// proxies and error messages.
func (geminiVendor) Endpoint(model string) string {
	return fmt.Sprintf("%s/models/%s:generateContent", geminiDirectBase, model)
}

func (geminiVendor) Auth(_ context.Context, key string) (map[string]string, error) {
	return map[string]string{"x-goog-api-key": key}, nil
}

func (geminiVendor) BuildRequest(_, pageURL, question string, _ bool) (any, error) {
	if blindNoTools {
		// Gemini's request always declares urlContext (geminiBody has no tool-free
		// mode), so it CANNOT honour the blind "no tools" step the content-reuse
		// test relies on: it would still fetch the page, and any "reuse" it showed
		// would be the address-keyed reuse we already know, not a finding. Warn
		// loudly rather than return a page-fetching request in a run that asked for
		// none. Google is parked off tunnel hosts anyway, so this should not fire.
		fmt.Fprintln(os.Stderr,
			"[trial] WARNING: Gemini does not support the blind no-tools mode — it "+
				"always declares urlContext and will still fetch the page. A "+
				"content-reuse blind step including Gemini must not be trusted.")
	}
	return geminiBody(pageURL, question), nil
}

func (geminiVendor) ParseResponse(raw []byte) (string, probe.Usage, error) {
	text, perr := parseGeminiResponse(raw)
	usage, _ := probe.UsageGemini(raw)
	usage.FetchStatus = geminiFetchStatus(raw)
	return text, usage, perr
}

// geminiFetchStatus is Google's own per-URL retrieval report, "<STATUS> <url>"
// joined by "; ", success and failure alike ("" when the reply has none).
func geminiFetchStatus(raw []byte) string {
	var parsed struct {
		Candidates []struct {
			URLContextMetadata struct {
				URLMetadata []urlRetrieval `json:"urlMetadata"`
			} `json:"urlContextMetadata"`
		} `json:"candidates"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return ""
	}
	var parts []string
	for _, c := range parsed.Candidates {
		for _, m := range c.URLContextMetadata.URLMetadata {
			parts = append(parts, m.Status+" "+m.URL)
		}
	}
	return strings.Join(parts, "; ")
}

func (geminiVendor) Continue(any, []byte) (any, error) { return nil, nil }
