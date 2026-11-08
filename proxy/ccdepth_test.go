package main

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"aisearch-probe/internal/probe"
)

// ccDepthServer is a secret-configured server with a known start time, so the
// "recent" last-changed date is checkable.
func ccDepthServer(t *testing.T, started time.Time) *staticServer {
	t.Helper()
	st := cpageServer(t, fmtTestSecret)
	st.startedAt = started
	return st
}

// Every (setting, version-info) pair must send EXACTLY its headers: the setting's
// Cache-Control and Expires, and the version info the word names. A wrong header
// here would mislabel every result under it.
func TestCcDepthServesEachSettingsHeaders(t *testing.T) {
	started := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	srv, read, _ := newTestProxy(t, ccDepthServer(t, started), "")
	fixed := probe.PageLastModified.Format(http.TimeFormat)

	for _, tok := range probe.CacheSettingOrder {
		set := probe.CacheSettings[tok]
		for _, v := range probe.ValidatorModes {
			path := "/cacheHeaders/" + tok + "/" + string(v) + "/cd-" + tok + "-" + string(v) + "/4096"
			resp := get(t, srv.URL+path, nil)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s: status %d", path, resp.StatusCode)
				continue
			}
			wantLM := map[probe.ValidatorMode]string{
				probe.ValidatorsOld:    fixed,
				probe.ValidatorsRecent: started.Format(http.TimeFormat),
				probe.ValidatorsNone:   "",
			}[v]
			for _, hc := range []struct{ name, want string }{
				{"Cache-Control", set.CacheControl}, {"Expires", set.Expires}, {"Last-Modified", wantLM},
			} {
				_, present := resp.Header[hc.name]
				if present != (hc.want != "") || resp.Header.Get(hc.name) != hc.want {
					t.Errorf("%s: %s = %q (present %v), want %q", path, hc.name, resp.Header.Get(hc.name), present, hc.want)
				}
			}
			if _, present := resp.Header["Etag"]; present != (v != probe.ValidatorsNone) {
				t.Errorf("%s: ETag present = %v", path, present)
			}
		}
	}

	// The log must carry what was sent, because the report grades from it.
	for _, r := range read() {
		if !strings.Contains(r.URL, "/expired/") {
			continue
		}
		if r.ExpiresSent != probe.CacheSettings["expired"].Expires {
			t.Errorf("log for %s: expires_sent = %q", r.URL, r.ExpiresSent)
		}
	}
}

// THE SAME CONTENT AS EVERY OTHER TEST: the body is byte-for-byte the shared depth
// HTML page for the same id and size, whatever the setting. Only headers differ.
func TestCcDepthBodyIsTheSharedDepthHTML(t *testing.T) {
	srv, _, _ := newTestProxy(t, ccDepthServer(t, time.Now()), "")
	want := body(t, get(t, srv.URL+"/depth/html/same-id/8192", nil))
	for _, tok := range []string{"nostore", "max3600", "none"} {
		for _, v := range []string{"tag", "notag"} {
			got := body(t, get(t, srv.URL+"/cacheHeaders/"+tok+"/"+v+"/same-id/8192", nil))
			if got != want {
				t.Errorf("%s/%s: body differs from /depth/html/ for the same id and size", tok, v)
			}
		}
	}
}

// A malformed address, or an unknown word, must never read as a page.
func TestCcDepthMalformedIs404(t *testing.T) {
	srv, _, _ := newTestProxy(t, ccDepthServer(t, time.Now()), "")
	for _, p := range []string{
		"/cacheHeaders/",
		"/cacheHeaders/none/tag/id",        // no size
		"/cacheHeaders/none/tag/id/",       // empty size
		"/cacheHeaders/none//id/4096",      // empty version word
		"/cacheHeaders//tag/id/4096",       // empty setting
		"/cacheHeaders/bogus/tag/id/4096",  // unknown setting
		"/cacheHeaders/none/bogus/id/4096", // unknown version word
		"/cacheHeaders/none/tag/id/abc",    // size not a number
		"/cacheHeaders/none/tag/id/4096/x", // extra segment
		"/cacheHeaders/none/tag//4096",     // empty id
	} {
		resp := get(t, srv.URL+p, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s returned %d, want 404", p, resp.StatusCode)
		}
	}
	resp := get(t, srv.URL+"/cacheHeaders/none/tag/id/100", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a size too small for the markers returned %d, want 400", resp.StatusCode)
	}
}

// The page prints per-page codes, so without a secret it must refuse, not serve
// a guessable page.
func TestCcDepthNeedsTheSecret(t *testing.T) {
	s := newStaticServer(t.TempDir(), "")
	srv, _, _ := newTestProxy(t, s, "")
	resp := get(t, srv.URL+"/cacheHeaders/none/tag/id/4096", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d without a secret, want 503", resp.StatusCode)
	}
}

// No real file under probe-site/ may shadow the reserved prefix.
func TestNoRealPageShadowsTheCcDepthPrefix(t *testing.T) {
	entries, err := os.ReadDir("../site")
	if err != nil {
		t.Fatalf("read probe-site: %v", err)
	}
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		if name == "cacheheaders" || strings.HasPrefix(name, "cacheheaders.") {
			t.Errorf("site/%s would shadow the reserved %s prefix", e.Name(), ccDepthPrefix)
		}
	}
}

// The day-2 cache-header question names one code by the words printed next to it
// on the page (probe.DepthMarkerLabel). If the page's wording and the question drift
// apart, the model cannot find the code it is asked for and every answer scores as
// "never had the page". Pin the pairing against the bytes actually served.
func TestCcDepthPageCarriesEveryNamedCode(t *testing.T) {
	srv, _, _ := newTestProxy(t, ccDepthServer(t, time.Now()), "")
	id := "label-pairing"
	page := body(t, get(t, srv.URL+"/cacheHeaders/none/tag/"+id+"/2048", nil))
	for _, pct := range probe.DepthPercents {
		code := probe.DepthCode(fmtTestSecret, id, pct, probe.DepthPageRevision)
		want := probe.DepthMarkerPrefix + code + " is " + probe.DepthMarkerLabel(pct)
		if !strings.Contains(page, want) {
			t.Errorf("depth %d%%: page does not contain %q", pct, want)
		}
	}
}
