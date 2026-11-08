package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// staticServer is a built-in origin, so the probe can run a complete experiment
// on its own: one binary, no nginx, no root, nothing else to install.
//
// It exists because the measurement is worthless unless the origin stamps its
// responses. Rather than hoping whatever sits behind the probe does that
// correctly, this serves pages that always carry an ETag and a Last-Modified
// and always answer "has this changed?" properly. The experiment is then valid
// by construction rather than by inspection.
type staticServer struct {
	root         http.Dir
	cacheControl string
	// robotsCacheControl is the Cache-Control sent with robots.txt (default
	// "no-store", as the original runs from 15 Sep 2026 served it). Empty means
	// the normal way: no Cache-Control, an ETag. See robots.go.
	robotsCacheControl string
	flaky              *flakyState // per-caller attempt counts for /fault/flaky/N
	heal               *healState  // per-caller first-sight time for /fault/heal/<sec>
	loops              *flakyState // per-caller hop counts for /fault/loop/{a,b} (the hop cap)

	// controlSecret guards the page controls (correction, heal). The controls
	// are off unless a secret is set.
	controlSecret string

	// versions is the changeable page's per-id current version, set through
	// POST /control/correction (same controlSecret). Memory-only unless main points
	// it at a state file. See verpage.go.
	versions *versionState

	// healPages is the healable page's per-id state (broken with a chosen failure,
	// or fixed), set through POST /control/heal (same controlSecret). Memory-only
	// unless main points it at a state file. See healpage.go. (Not the same as heal
	// below, which is the old timer-based /fault/heal/<sec>.)
	healPages *healPageState

	// pageCodeSecret keys the unforgeable per-page codes on the depth pages.
	// Empty means those pages refuse to serve (503).
	pageCodeSecret string

	// startedAt is when this origin started, to the second (an HTTP date has no
	// finer unit). The cache-header test's "recent" pages send it as their
	// Last-Modified: stable for the life of the process, so the ETag-carrying page
	// does not change, yet always within hours of the asks.
	startedAt time.Time
}

func newStaticServer(dir, cacheControl string) *staticServer {
	versions, _ := newVersionState("")   // memory-only never fails; main swaps in the file-backed one
	healPages, _ := newHealPageState("") // same
	return &staticServer{root: http.Dir(dir), cacheControl: cacheControl, robotsCacheControl: defaultRobotsCacheControl, flaky: newFlakyState(), heal: newHealState(), loops: newFlakyState(),
		versions: versions, healPages: healPages, startedAt: time.Now().UTC().Truncate(time.Second)}
}

// openFold opens name, and if that fails, retries against a sibling whose name
// differs only in capitalisation.
//
// THIS IS DELIBERATELY MORE PERMISSIVE THAN AN ORDINARY SERVER, and only
// because of what the address-variation experiment measures. That experiment
// asks which spellings of an address a provider treats as one page: it hands
// out /vary.html and then /VARY.html, and reads our log to see whether a second
// download arrives. On a case-sensitive filesystem the second address would be
// a 404 — so we would be measuring our own missing file rather than the
// provider's view of the address, and the page's text would never reach the
// model to confirm the result a second way.
//
// It changes nothing about the measurement itself: the provider decides whether
// to fetch BEFORE it learns what we answer.
func (s *staticServer) openFold(name string) (http.File, error) {
	if f, err := s.root.Open(name); err == nil {
		return f, nil
	}
	dir, base := path.Split(name)
	if dir == "" || base == "" {
		return nil, os.ErrNotExist
	}
	d, err := s.root.Open(path.Clean(dir))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	entries, err := d.Readdir(-1)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name(), base) {
			return s.root.Open(path.Join(dir, e.Name()))
		}
	}
	return nil, os.ErrNotExist
}

func (s *staticServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The version control sets which version each changeable page serves. Off
	// (falls through to a 404) unless a control secret is set. See verpage.go.
	if r.URL.Path == controlVersionPath {
		if s.serveVersionControl(w, r) {
			return
		}
	}

	// The heal control creates healable pages broken and fixes them. Off (falls
	// through to a 404) unless a control secret is set. See healpage.go.
	if r.URL.Path == controlHealPath {
		if s.serveHealControl(w, r) {
			return
		}
	}

	// robots.txt: see robots.go for its caching header.
	if r.URL.Path == "/robots.txt" {
		s.serveRobots(w, r)
		return
	}

	// Generated pages live at a per-run address that has never been fetched
	// before. Each route (/depth/..., /cacheHeaders/, /correction/..., /heal/) is a
	// genPage; serveGenerated runs the shared spine and returns true once it has
	// handled the request. See genpage.go.
	if s.serveGenerated(w, r) {
		return
	}

	// The fault corner is deliberately misbehaving pages for the failure-retry
	// experiment; it never touches the file system. An unknown fault name is
	// not handled here and falls through to a normal 404, so a typo cannot read
	// as a page that exists. See faults.go.
	if isFaultPath(r.URL.Path) {
		if s.serveFault(w, r) {
			return
		}
	}

	// http.Dir.Open rejects paths that try to escape the root. Clean also
	// resolves "." and ".." segments and drops a trailing slash, so those
	// spellings of an address arrive here as the same file — which is what the
	// address-variation experiment needs them to do.
	name := path.Clean("/" + r.URL.Path)

	f, err := s.openFold(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if st.IsDir() {
		f.Close()
		name = path.Join(name, "index.html")
		if f, err = s.openFold(name); err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		if st, err = f.Stat(); err != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
	}

	content, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}

	// A strong ETag over the actual bytes: it changes when and only when the
	// content changes, which is what a crawler needs to revalidate against.
	sum := sha256.Sum256(content)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:8])+`"`)
	if s.cacheControl != "" {
		w.Header().Set("Cache-Control", s.cacheControl)
	}

	// ServeContent sets Last-Modified and turns a matching If-None-Match or
	// If-Modified-Since into a 304 with no body.
	http.ServeContent(w, r, name, st.ModTime(), bytes.NewReader(content))
}
