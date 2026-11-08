package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// THE CHANGEABLE PAGE — one address whose content flips from version 1 (A) to
// version 2 (B), for the correction-visibility and type-vs-correction tests
// (docs/experiments/correction.md; plan items 3 and 8).
//
// IT IS THE DEPTH PAGE, NOT A NEW PAGE. /correction/html/<id>/<bytes> and
// /correction/txt/<id>/<bytes> serve exactly what /depth/html/ and /depth/txt/
// serve — same generator, same five markers at the same depths — except that the
// codes are made at the page's CURRENT VERSION instead of the fixed
// probe.DepthPageRevision. probe.DepthCode already takes a revision, so version 1
// is byte-for-byte the depth page and version 2 differs only in its five codes
// (TestVersionOneIsTheDepthPage, TestFlipChangesCodesTagAndDateOnly).
//
// THE VERSION IS SERVER STATE, NEVER IN THE REQUEST. The fetcher asks for the
// same address before and after; the server answers with whatever version the
// control last set for that id. POST /control/correction (guarded by the same
// control secret as /control/robots) sets it. An id the control never set is a
// 404, so a typo — or a page lost to a restart — is visible, never a silent
// version 1.
//
// LAST-MODIFIED IS THE SWITCH TIME. Every other generated page says it last
// changed on the fixed probe.PageLastModified. Here that would be a lie with a
// consequence: a fetcher revalidating by date alone after the flip would be told
// "not modified" and keep version 1, and that would be OUR bug read as theirs. So
// the page carries the time its current version was set. It still sends no
// Cache-Control and no Expires (the normal page).
//
// THE STATE IS SAVED TO A FILE (-correction-serverStateFile) and read back at start-up,
// so an origin restart mid-run does not turn every version-2 page back into a 404.
// No file is an empty start, which is how an origin that never used this page has
// always started; a file that cannot be read is a start-up error, never a silent
// empty start.

const (
	verHTMLPrefix      = "/correction/html/"
	verTxtPrefix       = "/correction/txt/"
	controlVersionPath = "/control/correction"

	// maxPageVersion bounds what the control accepts. The tests use 1 and 2; a
	// single digit leaves room for a second correction without inviting typos.
	maxPageVersion = 9

	// defaultVersionPageBytes is the size the control reports tags for when the
	// caller names none: the 2 KB page both tests use.
	defaultVersionPageBytes = 2048

	versionStateFormat = 1
)

// verEntry is one page's current version and when it was set (to the second: an
// HTTP date has no finer unit, and the stored time is what Last-Modified says).
type verEntry struct {
	Version    int       `json:"version"`
	SwitchedAt time.Time `json:"switched_at"`
}

// versionStateFile is the on-disk shape. Unknown fields are ignored on read, so a
// later field never breaks an older origin reading a newer file.
type versionStateFile struct {
	Format int                 `json:"format"`
	Pages  map[string]verEntry `json:"pages"`
}

// versionState holds every changeable page's current version.
type versionState struct {
	mu    sync.Mutex
	path  string // "" = memory only
	pages map[string]verEntry
	now   func() time.Time // a test swaps in a fake clock
}

// newVersionState loads the state from path. An empty path is memory-only; a
// missing file is an empty start; a file that cannot be parsed is an error.
func newVersionState(path string) (*versionState, error) {
	vs := &versionState{path: path, pages: map[string]verEntry{}, now: time.Now}
	if path == "" {
		return vs, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return vs, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read version state %s: %w", path, err)
	}
	var f versionStateFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("version state %s is not readable (fix or move it; it is not overwritten): %w", path, err)
	}
	for id, e := range f.Pages {
		vs.pages[id] = e
	}
	return vs, nil
}

func (vs *versionState) get(id string) (verEntry, bool) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	e, ok := vs.pages[id]
	return e, ok
}

// set makes version the current one for id. Setting the version a page already
// has changes nothing — the switch time stays, or a no-op would read as a
// correction. The new state is saved before set returns; if the save fails the
// change is undone, so memory never claims a version the file would lose.
func (vs *versionState) set(id string, version int) (verEntry, error) {
	if err := validVersionID(id); err != nil {
		return verEntry{}, err
	}
	if version < 1 || version > maxPageVersion {
		return verEntry{}, fmt.Errorf("version must be 1..%d, got %d", maxPageVersion, version)
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	prev, had := vs.pages[id]
	if had && prev.Version == version {
		return prev, nil
	}
	e := verEntry{Version: version, SwitchedAt: vs.now().UTC().Truncate(time.Second)}
	vs.pages[id] = e
	if err := vs.saveLocked(); err != nil {
		if had {
			vs.pages[id] = prev
		} else {
			delete(vs.pages, id)
		}
		return verEntry{}, err
	}
	return e, nil
}

// saveLocked writes the whole state atomically (temp file + rename), so a crash
// mid-write leaves the previous state, never half of one. Caller holds vs.mu.
func (vs *versionState) saveLocked() error {
	if vs.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(versionStateFile{Format: versionStateFormat, Pages: vs.pages}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(vs.path), ".verpage-state-*")
	if err != nil {
		return fmt.Errorf("save version state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("save version state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("save version state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("save version state: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("save version state: %w", err)
	}
	return os.Rename(tmpName, vs.path)
}

// validVersionID accepts what parseSizePath would route: one non-empty path
// segment. Anything else could never be fetched, so setting it is a mistake.
func validVersionID(id string) error {
	if id == "" || strings.ContainsAny(id, "/?#") || strings.TrimSpace(id) != id {
		return fmt.Errorf("id must be one non-empty path segment, got %q", id)
	}
	return nil
}

// --- the two page routes ------------------------------------------------------

// verRoute is one format of the changeable page. render/name/contentType are the
// depth route's own, so version 1 is that route's page exactly.
type verRoute struct {
	pfx         string
	render      func(lines []string) []byte
	name        string
	contentType string
}

func (v verRoute) prefix() string  { return v.pfx }
func (verRoute) needsSecret() bool { return true }

// versionRoutes are the formats, keyed by the label the control reply and the
// runner use.
var versionRoutes = map[string]verRoute{
	"html": {verHTMLPrefix, renderDepthHTML, "depth.html", "text/html; charset=utf-8"},
	"txt":  {verTxtPrefix, renderDepthTxt, "depth.txt", "text/plain; charset=utf-8"},
}

func isVersionPagePrefix(p string) bool { return p == verHTMLPrefix || p == verTxtPrefix }

// body is the page for (secret, id, nbytes) at version: the depth generator with
// the codes made at that version.
func (v verRoute) body(secret, id string, nbytes, version int) []byte {
	return v.render(depthFormatLinesRev(secret, id, nbytes, true, version))
}

func (v verRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	id, nbytes, ok := parseSizePath(w, r, v.pfx, maxDepthPageBytes)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	e, known := s.versions.get(id)
	if !known {
		http.NotFound(w, r) // never set by the control: not a page
		return nil, "", "", pageHeaders{}, false
	}
	if nbytes < minDepthFormatBytes {
		http.Error(w, fmt.Sprintf("page too small: %d bytes cannot hold the markers", nbytes), http.StatusBadRequest)
		return nil, "", "", pageHeaders{}, false
	}
	// Normal headers, except Last-Modified = when this version was set (see top).
	return v.body(s.pageCodeSecret, id, nbytes, e.Version), v.name, v.contentType,
		pageHeaders{LastModified: e.SwitchedAt}, true
}

// --- the control ----------------------------------------------------------------

// versionControlReply is what POST /control/correction returns: the state it set,
// plus the ETag every version serves in each format at the given size. The runner
// keeps the tags so each logged fetch (probe.jsonl etag_sent) can be labelled as
// version 1 or 2 without our own fetch of the page.
type versionControlReply struct {
	ID           string                       `json:"id"`
	Version      int                          `json:"version"`
	SwitchedAt   string                       `json:"switched_at"`   // RFC 3339, UTC
	LastModified string                       `json:"last_modified"` // as the page sends it
	Size         int                          `json:"size"`
	ETags        map[string]map[string]string `json:"etags"` // format -> version -> tag
}

// serveVersionControl handles the control. It reports whether it handled the
// request; with no control secret configured it returns false, so the caller
// 404s it like any unknown path.
//
//	POST /control/correction   id=<page id>
//	                        version=<1..9>
//	                        size=<bytes>        (optional; which size's tags to report, default 2048)
//	                        secret=<secret>     (or header X-Probe-Control-Secret)
func (s *staticServer) serveVersionControl(w http.ResponseWriter, r *http.Request) bool {
	if s.controlSecret == "" {
		return false // feature disabled: not a real endpoint
	}
	if r.Method != http.MethodPost {
		http.Error(w, "control: POST only", http.StatusMethodNotAllowed)
		return true
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "control: bad form", http.StatusBadRequest)
		return true
	}
	if !s.controlAuthorized(r) {
		http.Error(w, "control: forbidden", http.StatusForbidden)
		return true
	}
	if s.pageCodeSecret == "" {
		http.Error(w, "control: the page-code secret is not configured, so the page cannot be served", http.StatusServiceUnavailable)
		return true
	}

	id := r.Form.Get("id")
	version, err := strconv.Atoi(r.Form.Get("version"))
	if err != nil {
		http.Error(w, "control: version must be a number 1..9", http.StatusBadRequest)
		return true
	}
	size := defaultVersionPageBytes
	if raw := r.Form.Get("size"); raw != "" {
		size, err = strconv.Atoi(raw)
		if err != nil || size < minDepthFormatBytes || size > maxDepthPageBytes {
			http.Error(w, "control: bad size", http.StatusBadRequest)
			return true
		}
	}
	e, err := s.versions.set(id, version)
	if err != nil {
		http.Error(w, "control: "+err.Error(), http.StatusBadRequest)
		return true
	}

	rep := versionControlReply{
		ID: id, Version: e.Version,
		SwitchedAt:   e.SwitchedAt.Format(time.RFC3339),
		LastModified: e.SwitchedAt.Format(http.TimeFormat),
		Size:         size,
		ETags:        map[string]map[string]string{},
	}
	top := e.Version
	if top < 2 {
		top = 2
	}
	for format, route := range versionRoutes {
		rep.ETags[format] = map[string]string{}
		for v := 1; v <= top; v++ {
			rep.ETags[format][strconv.Itoa(v)] = pageETag(route.body(s.pageCodeSecret, id, size, v))
		}
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		http.Error(w, "control: "+err.Error(), http.StatusInternalServerError)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, string(raw)+"\n")
	return true
}
