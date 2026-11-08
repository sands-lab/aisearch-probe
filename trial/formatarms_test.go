package main

import (
	"strings"
	"testing"

	"aisearch-probe/internal/probe"
)

// fmtTestSecret is the page-code secret the object-type arm tests run with.
const fmtTestSecret = "test-page-code-secret"

// formatArms is the object-type study (exp A): one arm per document format on the
// UNIFIED depth routes at a FIXED size, polled in ONE session so fetch + read + TTL
// are directly comparable across formats (same source, same model, same day, same
// question). FORMAT is the only variable. Every arm is a /depth<fmt>page/<id>/<size>
// route carrying the shared marker ladder, and the read is judged by the DEEPEST
// marker — its presence means the model read the document all the way down.
func TestFormatArmsBuildTheRightRoutes(t *testing.T) {
	const fixedSize = 2048
	arms := formatArms(fixedSize)
	if len(arms) != 8 {
		t.Fatalf("want 8 arms (txt, html, md, json, xml, csv, pdf, docx), got %d", len(arms))
	}

	provs := []Caller{{Name: "openai", Model: "gpt-5-mini"}}
	pages := buildTTLPages(arms, provs, "https://probe.example.org", "runObj", 6, fmtTestSecret, "unused-global-marker")
	if len(pages) != 48 {
		t.Fatalf("want 48 pages (8 arms x 1 provider x 6 reps), got %d", len(pages))
	}

	wantRoute := map[string]string{
		"txt":  "/depthtxtpage/",
		"html": "/depthhtmlpage/",
		"md":   "/depthmdpage/",
		"json": "/depthjsonpage/",
		"xml":  "/depthxmlpage/",
		"csv":  "/depthcsvpage/",
		"pdf":  "/depthpdfpage/",
		"docx": "/depthdocxpage/",
	}
	deepest := probe.DepthPercents[len(probe.DepthPercents)-1]
	seen := map[string]bool{}
	countByArm := map[string]int{}
	for _, pg := range pages {
		if seen[pg.PageID] {
			t.Errorf("duplicate page id %q across arms — a fetch for one would satisfy another", pg.PageID)
		}
		seen[pg.PageID] = true
		countByArm[pg.Arm]++
		frag, ok := wantRoute[pg.Arm]
		if !ok {
			t.Errorf("unexpected arm %q", pg.Arm)
			continue
		}
		if !strings.Contains(pg.URL, frag) {
			t.Errorf("arm %q url %q missing route fragment %q", pg.Arm, pg.URL, frag)
		}
		if !strings.Contains(pg.URL, pg.PageID) {
			t.Errorf("arm %q url %q must contain its page id %q (the fetch join keys on it)", pg.Arm, pg.URL, pg.PageID)
		}
		// Every arm's read must key on the PER-PAGE unforgeable code, not the global
		// marker — otherwise a model could carry one arm's answer to another.
		wantCode := probe.DepthCode(fmtTestSecret, pg.PageID, deepest, probe.DepthPageRevision)
		if wantCode == "" {
			t.Fatalf("arm %q page %q: deepest depth code came out empty (secret not threaded?)", pg.Arm, pg.PageID)
		}
		if pg.Marker != wantCode {
			t.Errorf("arm %q page %q: Marker = %q, want the deepest depth code %q", pg.Arm, pg.PageID, pg.Marker, wantCode)
		}
		if !strings.Contains(pg.URL, "/2048") {
			t.Errorf("arm %q url %q missing the fixed size /2048", pg.Arm, pg.URL)
		}
	}
	// The HTML arm must be the dedicated /htmlpage/ route — NOT /page/ (shared static
	// marker), which would make HTML the odd one out.
	for _, pg := range pages {
		if pg.Arm != "html" {
			continue
		}
		if !strings.Contains(pg.URL, "/depthhtmlpage/") {
			t.Errorf("html arm url %q must use the unified /depthhtmlpage/ route", pg.URL)
		}
	}
	for arm, n := range countByArm {
		if n != 6 {
			t.Errorf("arm %q has %d pages, want 6", arm, n)
		}
	}
	if len(wantRoute) != len(countByArm) {
		t.Errorf("built %d distinct arms, want %d", len(countByArm), len(wantRoute))
	}
}
