package main

import (
	"strings"
	"testing"

	"aisearch-probe/internal/probe"
)

// Every runner must keep the provider's OWN report of what happened to the fetch
// (memory rule, 2026-10-25). For Gemini that is urlRetrievalStatus per URL. It was
// only ever kept on failure, inside the error text; these pin that it now travels
// with every call, success included, through probe.Usage.

const geminiOK = `{"candidates":[{"content":{"parts":[{"text":"ORCHID"}]},
 "urlContextMetadata":{"urlMetadata":[{"retrievedUrl":"https://b.test/vary.html?probe=g-v1&k=alpha","urlRetrievalStatus":"URL_RETRIEVAL_STATUS_SUCCESS"}]}}],
 "usageMetadata":{"promptTokenCount":900,"candidatesTokenCount":20}}`

const geminiFail = `{"candidates":[{"content":{"parts":[{"text":"I cannot access it"}]},
 "urlContextMetadata":{"urlMetadata":[{"retrievedUrl":"https://b.test/VARY.HTML","urlRetrievalStatus":"URL_RETRIEVAL_STATUS_ERROR"}]}}],
 "usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20}}`

func TestGeminiFetchStatusKeptOnSuccess(t *testing.T) {
	_, u, err := geminiVendor{}.ParseResponse([]byte(geminiOK))
	if err != nil {
		t.Fatal(err)
	}
	want := "URL_RETRIEVAL_STATUS_SUCCESS https://b.test/vary.html?probe=g-v1&k=alpha"
	if u.FetchStatus != want {
		t.Errorf("FetchStatus = %q, want %q", u.FetchStatus, want)
	}
}

func TestGeminiFetchStatusKeptOnFailure(t *testing.T) {
	_, u, err := geminiVendor{}.ParseResponse([]byte(geminiFail))
	if err == nil {
		t.Fatal("a failed retrieval must still be an error")
	}
	if !strings.HasPrefix(u.FetchStatus, "URL_RETRIEVAL_STATUS_ERROR ") {
		t.Errorf("FetchStatus = %q, want the ERROR status kept alongside the error", u.FetchStatus)
	}
}

// Statuses from more than one round are joined, never overwritten.
func TestFetchStatusAddsUp(t *testing.T) {
	a := probe.Usage{FetchStatus: "S1 u1"}
	b := probe.Usage{FetchStatus: "S2 u2"}
	if got := a.Add(b).FetchStatus; got != "S1 u1; S2 u2" {
		t.Errorf("Add = %q", got)
	}
	if got := (probe.Usage{}).Add(b).FetchStatus; got != "S2 u2" {
		t.Errorf("Add onto empty = %q", got)
	}
}
