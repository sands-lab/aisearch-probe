package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"aisearch-probe/internal/probe"
)

// Small helpers shared by several runners.

func unixf(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func compactDur(d time.Duration) string {
	return strings.NewReplacer("h0m0s", "h", "m0s", "m", "0s", "").Replace(d.String())
}

// publicReachable reports whether the public origin returns ANY HTTP response.
// The status and body are not checked: a CDN's bot management may answer our own
// client with a 403 or a challenge, but that still proves the edge and the path
// to the origin are up. A connection error means the origin is unreachable.
func publicReachable(base string) bool {
	url := strings.TrimRight(base, "/") + "/robots.txt"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (probe liveness)")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// countHits counts the requests to host whose address carries token, inside
// [sinceUnix, sinceUnix+window]. Our own probe-* requests are skipped.
func countHits(recs []probe.Record, token, host string, sinceUnix, window float64) int {
	return len(fetchesFor(recs, token, host, sinceUnix, window))
}

func fetchesFor(recs []probe.Record, token, host string, sinceUnix, window float64) []probe.Record {
	var out []probe.Record
	for _, r := range recs {
		if r.TSUnix < sinceUnix || r.TSUnix > sinceUnix+window {
			continue
		}
		if strings.HasPrefix(r.Bot, "probe-") {
			continue
		}
		if hostOfURL(r.URL) != host {
			continue
		}
		if strings.Contains(r.URL, token) {
			out = append(out, r)
		}
	}
	return out
}

// fetchPageBody downloads one of our own pages, bypassing caches on the way.
func fetchPageBody(pageURL string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", livenessUA)
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := probeHTTPClient(30 * time.Second).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(b), nil
}

// sizeLabel writes a byte count the way the size switches take it (4k, 1m).
func sizeLabel(n int) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%dm", n/(1<<20))
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%dk", n/(1<<10))
	default:
		return fmt.Sprintf("%db", n)
	}
}
