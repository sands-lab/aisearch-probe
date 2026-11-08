package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"time"
)

// robots.txt is served from the site folder's own file, or a built-in allow-all
// when there is none.
//
// Its caching header is a server switch (-robotsCacheControl). The default is
// "no-store" with no ETag: the original servers sent robots.txt that way from
// 15 Sep 2026, when a robots.txt experiment (not in this release) needed every
// read to reach the server. An empty value serves it the normal way: no
// Cache-Control, and an ETag so a crawler can ask "has this changed?".

// defaultRobotsCacheControl is the -robotsCacheControl default.
const defaultRobotsCacheControl = "no-store"

// builtinRobots is served only if the site has no robots.txt of its own.
const builtinRobots = "User-agent: *\nAllow: /\n"

// baseRobots reads the site's own robots.txt, falling back to builtinRobots.
func (s *staticServer) baseRobots() string {
	f, err := s.openFold("/robots.txt")
	if err != nil {
		return builtinRobots
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil || len(body) == 0 {
		return builtinRobots
	}
	return string(body)
}

// serveRobots serves robots.txt with the configured caching header.
func (s *staticServer) serveRobots(w http.ResponseWriter, r *http.Request) {
	body := s.baseRobots()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if s.robotsCacheControl != "" {
		w.Header().Set("Cache-Control", s.robotsCacheControl)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, body)
		return
	}
	// The normal way: a strong ETag over the bytes, and a 304 for a matching
	// If-None-Match.
	sum := sha256.Sum256([]byte(body))
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:8])+`"`)
	http.ServeContent(w, r, "robots.txt", time.Time{}, strings.NewReader(body))
}

// controlAuthorized checks the control secret, from the X-Probe-Control-Secret
// header or the form's secret field (r.ParseForm must have run). Shared by every
// control endpoint (correction, heal). Constant-time compare so the secret cannot
// be guessed a byte at a time.
func (s *staticServer) controlAuthorized(r *http.Request) bool {
	got := r.Header.Get("X-Probe-Control-Secret")
	if got == "" {
		got = r.Form.Get("secret")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.controlSecret)) == 1
}
