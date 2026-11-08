package main

import (
	"crypto/subtle"
	"io"
	"net/http"
)

// robots.txt is served from the site folder's own file, or a built-in allow-all
// when there is none. It is sent with Cache-Control: no-store and no ETag. That
// is how robots.txt was served in every run this code reproduces, so it is kept.

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

// serveRobots serves robots.txt, no-store and with no ETag.
func (s *staticServer) serveRobots(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, s.baseRobots())
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
