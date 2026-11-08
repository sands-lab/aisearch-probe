package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// REACHING OUR OWN PROBE WHEN THE LOCAL RESOLVER CANNOT SEE IT.
//
// This network does not resolve *.trycloudflare.com. run-pilot.sh has always
// worked around that for curl, with DNS-over-HTTPS plus --resolve. The Go driver
// did not, so its own page check failed with "i/o timeout" against a page that
// was serving perfectly — and refused to start the sharing experiment.
//
// Fixed here rather than by passing an address in, because anyone running
// probe-trial by hand meets the same wall. Provider API calls are untouched:
// only the probe's own hostname is pinned.
//
// Resolution order:
//  1. -resolve host:ip, if given, wins. An explicit override for a network where
//     DNS-over-HTTPS is blocked too.
//  2. the system resolver.
//  3. DNS-over-HTTPS via 1.1.1.1, reached by literal address so it needs no DNS
//     of its own.

// resolveOverrides holds -resolve host:ip pairs.
var resolveOverrides = map[string]string{}

// addResolveOverride parses one host:ip pair.
func addResolveOverride(spec string) error {
	host, ip, ok := strings.Cut(spec, ":")
	if !ok || host == "" || net.ParseIP(ip) == nil {
		return fmt.Errorf("-resolve wants host:ip, got %q", spec)
	}
	resolveOverrides[strings.ToLower(host)] = ip
	return nil
}

var (
	pinnedMu sync.Mutex
	pinned   = map[string]string{} // hostname -> address, resolved once
)

// resolveHost returns an address for hostname, trying the three sources in order.
func resolveHost(ctx context.Context, hostname string) (string, error) {
	hostname = strings.ToLower(hostname)

	if ip, ok := resolveOverrides[hostname]; ok {
		return ip, nil
	}

	pinnedMu.Lock()
	if ip, ok := pinned[hostname]; ok {
		pinnedMu.Unlock()
		return ip, nil
	}
	pinnedMu.Unlock()

	sysCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	if addrs, err := net.DefaultResolver.LookupHost(sysCtx, hostname); err == nil && len(addrs) > 0 {
		return addrs[0], nil
	}

	ip, err := resolveOverHTTPS(ctx, hostname)
	if err != nil {
		return "", fmt.Errorf("%s resolves neither locally nor over DNS-over-HTTPS: %w", hostname, err)
	}
	pinnedMu.Lock()
	pinned[hostname] = ip
	pinnedMu.Unlock()
	return ip, nil
}

var dohAnswerRE = regexp.MustCompile(`^[0-9.]+$`)

// resolveOverHTTPS asks 1.1.1.1 over HTTPS, addressed by literal IP so the
// lookup itself needs no working resolver.
func resolveOverHTTPS(ctx context.Context, hostname string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	url := "https://1.1.1.1/dns-query?name=" + hostname + "&type=A"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("accept", "application/dns-json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var doc struct {
		Answer []struct {
			Data string `json:"data"`
			Type int    `json:"type"`
		} `json:"Answer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", err
	}
	for _, a := range doc.Answer {
		// Type 1 is an A record; CNAME answers also appear and are not addresses.
		if a.Type == 1 && dohAnswerRE.MatchString(a.Data) {
			return a.Data, nil
		}
	}
	return "", fmt.Errorf("no address in the DNS-over-HTTPS answer")
}

// probeHTTPClient is for fetching OUR OWN pages. It keeps TLS verification on
// and the Host header intact — only the address dialled is substituted, exactly
// as curl --resolve does — so this is not a way around certificate checking.
func probeHTTPClient(timeout time.Duration) *http.Client {
	base := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return base.DialContext(ctx, network, addr)
				}
				if net.ParseIP(host) != nil {
					return base.DialContext(ctx, network, addr)
				}
				ip, rerr := resolveHost(ctx, host)
				if rerr != nil {
					return nil, rerr
				}
				return base.DialContext(ctx, network, net.JoinHostPort(ip, port))
			},
			TLSHandshakeTimeout: 15 * time.Second,
		},
	}
}
