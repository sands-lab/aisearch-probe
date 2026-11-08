package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"testing"
)

// The whole point of the sized route is byte-exact control: a request for N bytes
// must return a body of EXACTLY N bytes. The size lives in the path, so nothing
// rides on a strippable query.
func TestSizePageReturnsExactBytes(t *testing.T) {
	for _, n := range []int{1024, 4096, 65536, 262144} {
		got := renderSizePage("s-a", n)
		if len(got) != n {
			t.Errorf("renderSizePage(id, %d) returned %d bytes, want exactly %d", n, len(got), n)
		}
	}
}

// Bytes are a pure function of the address (id + size), so the ETag over the body
// is stable across fetches — the same discipline /page/ follows, which is what
// lets a provider cache and revalidate. Same address twice => byte-identical.
func TestSizePageSameAddressIsByteIdentical(t *testing.T) {
	a := renderSizePage("s-a", 4096)
	b := renderSizePage("s-a", 4096)
	if a != b {
		t.Error("the same (id, size) produced different bytes — the ETag would not be stable")
	}
}

// Two different sizes must differ (different bytes => different ETag), so the log
// can tell one rung of the ladder from another.
func TestSizePageDifferentSizesDiffer(t *testing.T) {
	a := renderSizePage("s-a", 4096)
	b := renderSizePage("s-a", 8192)
	if a == b {
		t.Error("two sizes produced identical bytes")
	}
	sumA := sha256.Sum256([]byte(a))
	sumB := sha256.Sum256([]byte(b))
	if sumA == sumB {
		t.Error("two sizes produced the same ETag")
	}
}

// CONFOUND GUARD: a fetcher may truncate long content by token count and cache
// the stub. If the marker sat at the bottom, a truncated-but-cached big page
// would read as "never had the content" — a false cliff. So the marker must sit
// near the TOP, before any plausible truncation point, on pages of every size.
func TestSizePageMarkerIsNearTheTop(t *testing.T) {
	const topWindow = 512 // bytes; well inside any token-cap truncation
	for _, n := range []int{1024, 262144, 1 << 20} {
		got := renderSizePage("s-a", n)
		head := got
		if len(head) > topWindow {
			head = head[:topWindow]
		}
		if !strings.Contains(head, "RCP-2026-08") {
			t.Errorf("size %d: marker not in the first %d bytes — truncation would hide it", n, topWindow)
		}
	}
}

// Served end to end, a sized page must be a real, stamped, cacheable page: an
// ETag over its exact bytes, no Cache-Control (the normal page), and the
// marker present.
func TestSizePageServedIsStampedAndExact(t *testing.T) {
	srv, read, _ := newTestProxy(t, newStaticServer(t.TempDir(), "public, max-age=3600"), "")

	resp := get(t, srv.URL+"/sizepage/s-a/4096", nil)
	got := body(t, resp)

	if len(got) != 4096 {
		t.Errorf("served body is %d bytes, want 4096", len(got))
	}
	sum := sha256.Sum256([]byte(got))
	wantETag := `"` + hex.EncodeToString(sum[:8]) + `"`
	if et := resp.Header.Get("ETag"); et != wantETag {
		t.Errorf("ETag = %q, want %q (over the exact body)", et, wantETag)
	}
	if _, present := resp.Header["Cache-Control"]; present {
		t.Errorf("Cache-Control = %q, want none (the normal page)", resp.Header.Get("Cache-Control"))
	}
	if !strings.Contains(got, "RCP-2026-08") {
		t.Error("served sized page is missing the marker")
	}
	recs := read()
	if len(recs) != 1 || !recs[0].Stamped() {
		t.Fatalf("the log must show the sized page as stamped: %+v", recs)
	}
}

// It must answer "has this changed?" with a 304, or a polite crawler re-downloads
// it and looks wasteful — the same as /page/.
func TestSizePageAnswers304(t *testing.T) {
	srv, _, _ := newTestProxy(t, newStaticServer(t.TempDir(), ""), "")

	first := get(t, srv.URL+"/sizepage/s-b/8192", nil)
	first.Body.Close()
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on first fetch")
	}
	second := get(t, srv.URL+"/sizepage/s-b/8192", map[string]string{"If-None-Match": etag})
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation returned %d, want 304", second.StatusCode)
	}
}

// A malformed address must 404, never read as a real page: no id, no size,
// non-numeric size, or an extra path segment.
func TestSizePageMalformedIs404(t *testing.T) {
	srv, _, _ := newTestProxy(t, newStaticServer(t.TempDir(), ""), "")
	for _, p := range []string{
		"/sizepage/",         // nothing
		"/sizepage/s-a",      // no size segment
		"/sizepage/s-a/",     // empty size
		"/sizepage//4096",    // empty id
		"/sizepage/s-a/abc",  // non-numeric size
		"/sizepage/s-a/0",    // zero
		"/sizepage/s-a/-1",   // negative
		"/sizepage/s-a/40/9", // extra segment
	} {
		resp := get(t, srv.URL+p, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s returned %d, want 404", p, resp.StatusCode)
		}
	}
}

// An absurd size must be refused, not served — the origin must never be pushed to
// build a huge body on request.
func TestSizePageTooLargeIsRefused(t *testing.T) {
	srv, _, _ := newTestProxy(t, newStaticServer(t.TempDir(), ""), "")
	resp := get(t, srv.URL+"/sizepage/s-a/999999999", nil)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("an absurd size was served (status %d); it must be refused", resp.StatusCode)
	}
}

// The sized route must not shadow /page/ (retention run) or /fault/, and no real
// file under probe-site/ may shadow the /sizepage/ prefix.
func TestNoRealPageShadowsTheSizePagePrefix(t *testing.T) {
	entries, err := os.ReadDir("../site")
	if err != nil {
		t.Fatalf("read probe-site: %v", err)
	}
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		if name == "sizepage" || strings.HasPrefix(name, "sizepage.") {
			t.Errorf("probe-site/%s would shadow the reserved /sizepage/ prefix", e.Name())
		}
	}
	// The prefixes must be distinct corners of the origin.
	if strings.HasPrefix(sizePagePrefix, pagePrefix) || strings.HasPrefix(pagePrefix, sizePagePrefix) {
		t.Errorf("/sizepage/ (%q) and /page/ (%q) shadow each other", sizePagePrefix, pagePrefix)
	}
	if strings.HasPrefix(sizePagePrefix, faultPrefix) || strings.HasPrefix(faultPrefix, sizePagePrefix) {
		t.Errorf("/sizepage/ (%q) and /fault/ (%q) shadow each other", sizePagePrefix, faultPrefix)
	}
}
