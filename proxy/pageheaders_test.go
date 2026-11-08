package main

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aisearch-probe/internal/probe"
)

// writePage's zero headers are the NORMAL page every experiment except the
// cache-header test serves: an ETag, Last-Modified = the fixed date, and NO
// Cache-Control or Expires at all. Each pageHeaders field changes exactly one
// thing. Presence is checked in the map, not with Get(), because Get cannot tell
// "absent" from "present but empty", and absent is the point.
func TestWritePageHeaderRules(t *testing.T) {
	fixed := probe.PageLastModified.Format(http.TimeFormat)
	recent := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		h        pageHeaders
		wantCC   string // "" = header absent
		wantExp  string
		wantETag bool
		wantLM   string
	}{
		{"zero value is the normal page", pageHeaders{}, "", "", true, fixed},
		{"cache-control is sent", pageHeaders{CacheControl: "no-cache"}, "no-cache", "", true, fixed},
		{"expires is sent", pageHeaders{Expires: "Thu, 01 Jan 2026 00:00:00 GMT"}, "", "Thu, 01 Jan 2026 00:00:00 GMT", true, fixed},
		{"a recent last-changed date", pageHeaders{LastModified: recent}, "", "", true, recent.Format(http.TimeFormat)},
		{"no version info at all", pageHeaders{NoValidators: true}, "", "", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/page/x", nil)
			writePage(w, r, "x.html", "text/html; charset=utf-8", c.h, []byte("<!doctype html><title>x</title>hi"))
			resp := w.Result()
			for _, hc := range []struct{ name, want string }{
				{"Cache-Control", c.wantCC}, {"Expires", c.wantExp}, {"Last-Modified", c.wantLM},
			} {
				_, present := resp.Header[hc.name]
				if present != (hc.want != "") || resp.Header.Get(hc.name) != hc.want {
					t.Errorf("%s = %q (present %v), want %q", hc.name, resp.Header.Get(hc.name), present, hc.want)
				}
			}
			if _, present := resp.Header["Etag"]; present != c.wantETag {
				t.Errorf("ETag present = %v, want %v", present, c.wantETag)
			}
			if resp.Header.Get("Content-Type") == "" {
				t.Error("writePage must set Content-Type")
			}
		})
	}
}

// With no version info, a crawler's remembered tag or date cannot match anything:
// the honest answer is the full page, never a 304. (If-None-Match: * is not tested —
// RFC 9110 § 13.1.2 makes "*" match any existing page, so 304 is right there.)
func TestWritePageWithoutValidatorsNeverAnswers304(t *testing.T) {
	for _, hdr := range []map[string]string{
		{"If-None-Match": `"0123456789abcdef"`},
		{"If-Modified-Since": time.Now().UTC().Format(http.TimeFormat)},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		writePage(w, r, "x.html", "text/html; charset=utf-8", pageHeaders{NoValidators: true}, []byte("body"))
		if code := w.Result().StatusCode; code != http.StatusOK {
			t.Errorf("%v: status %d, want 200 (nothing to compare against)", hdr, code)
		}
	}
}

// THE LEAK GUARD (DECISIONS.md § D13). Only the cache-header test may choose a
// caching header. Every other generated page must serve exactly the normal
// headers, so no test setting — no-store above all — can contaminate another
// experiment. Every route needs a sample here, so a new route cannot skip it.
func TestOnlyTheCacheTestRouteChoosesCacheHeaders(t *testing.T) {
	st := cpageServer(t, fmtTestSecret)
	// The changeable page serves only ids the control has set; set one so its
	// sample address exists. Its Last-Modified is that switch time (see below).
	verEntry, err := st.versions.set("g-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	// The healable page serves only ids the control created; create one and fix it,
	// so its sample is the served page. Its Last-Modified is the flip time.
	if _, err := st.healPages.create("g-a", "503"); err != nil {
		t.Fatal(err)
	}
	healEntry, err := st.healPages.heal("g-a")
	if err != nil {
		t.Fatal(err)
	}
	srv, _, _ := newTestProxy(t, st, "")
	samples := map[string]string{
		"/page/":                   "/page/g-a",
		"/sizepage/":               "/sizepage/g-a/4096",
		"/depthpage/":              "/depthpage/g-a/65536",
		"/cpage/":                  "/cpage/cr01-xy",
		"/htmlpage/":               "/htmlpage/g-a",
		"/jsonpage/":               "/jsonpage/g-a",
		"/txtpage/":                "/txtpage/g-a",
		"/mdpage/":                 "/mdpage/g-a",
		"/xmlpage/":                "/xmlpage/g-a",
		"/csvpage/":                "/csvpage/g-a",
		"/pdfpage/":                "/pdfpage/g-a",
		"/docxpage/":               "/docxpage/g-a",
		"/depth/txt/":              "/depth/txt/g-a/4096",
		"/depth/pdf/":              "/depth/pdf/g-a/4096",
		"/depthpdfrawpage/":        "/depthpdfrawpage/g-a/4096",
		"/depth/docx/":             "/depth/docx/g-a/4096",
		"/depth/declaredType/":     "/depth/declaredType/plain/g-a/4096",
		"/depth/md/":               "/depth/md/g-a/4096",
		"/depth/html/":             "/depth/html/g-a/4096",
		"/depth/json/":             "/depth/json/g-a/4096",
		"/depth/xml/":              "/depth/xml/g-a/4096",
		"/depth/csv/":              "/depth/csv/g-a/4096",
		"/depth/jsInline/":         "/depth/jsInline/g-a/4096",
		"/depth/jsExternal/":       "/depth/jsExternal/g-a/4096",
		"/depth/jsData/":           "/depth/jsData/g-a/4096",
		"/depth/jsonLd/":           "/depth/jsonLd/g-a/4096",
		"/depth/jsExternalScript/": "/depth/jsExternalScript/g-a/4096.js",
		"/correction/html/":        "/correction/html/g-a/2048",
		"/correction/txt/":         "/correction/txt/g-a/2048",
		"/heal/":                   "/heal/g-a/2048",
		"/idpage/":                 "/idpage/g-a/mc_eid/2048?mc_eid=user-1",
	}
	fixed := probe.PageLastModified.Format(http.TimeFormat)
	for _, p := range genPageRoutes {
		if p.prefix() == ccDepthPrefix {
			continue // the one route allowed to choose
		}
		path, ok := samples[p.prefix()]
		if !ok {
			t.Errorf("route %s has no sample address in the leak guard — add one", p.prefix())
			continue
		}
		resp := get(t, srv.URL+path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d, want 200 (fix the sample address)", path, resp.StatusCode)
			continue
		}
		for _, h := range []string{"Cache-Control", "Expires"} {
			if _, present := resp.Header[h]; present {
				t.Errorf("%s sent %s: %q — only %s may choose a caching header", path, h, resp.Header.Get(h), ccDepthPrefix)
			}
		}
		if resp.Header.Get("ETag") == "" {
			t.Errorf("%s: no ETag — the normal page carries one", path)
		}
		// The ONE exception: the changeable page's Last-Modified is when its current
		// version was set. A fixed date would answer a date-only revalidation after a
		// flip with a 304, telling the fetcher the old version is still current.
		// It still sends no Cache-Control or Expires (checked above).
		wantLM := fixed
		if isVersionPagePrefix(p.prefix()) {
			wantLM = verEntry.SwitchedAt.Format(http.TimeFormat)
		}
		// Same reason for the healable page: its Last-Modified is when it was fixed.
		if p.prefix() == healPagePrefix {
			wantLM = healEntry.HealedAt.Format(http.TimeFormat)
		}
		if got := resp.Header.Get("Last-Modified"); got != wantLM {
			t.Errorf("%s: Last-Modified = %q, want %q", path, got, wantLM)
		}
	}
}

// The static probe-site/ pages (index.html, method.html, staleness.html, …) get the
// -cacheControl flag. Its default must be "send none" — the same normal page.
func TestStaticCacheControlDefaultsToNone(t *testing.T) {
	f := flag.Lookup("cacheControl")
	if f == nil {
		t.Fatal("no -cacheControl flag")
	}
	if f.DefValue != "" {
		t.Errorf("-cacheControl defaults to %q, want \"\" (send no Cache-Control)", f.DefValue)
	}
}
