package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// THE HEALABLE PAGE — one address that fails until the control fixes it, for the
// remembered-failure test (doc/handoff-cacheheader-day2-and-heal.md Part 2): after a
// broken page recovers, how long does a vendor keep answering "it's broken"?
//
// WHY A CONTROL, NOT A TIMER. The old /fault/heal/<sec> heals a set time after the
// first request. A vendor that sends several requests per ask blurs "first request",
// so the recovery time was never ours. Here the server answers broken until POST
// /control/heal says otherwise, and the reply records the exact flip time; re-asks
// are timed from that. It never heals on a request COUNT (that would trip inside one
// ask).
//
// THE FAILURE IS SERVER STATE, NEVER IN THE ADDRESS. The control creates each id with
// one failure — 503, 404 or 403 — and every page has the same address shape,
// /heal/<id>/<bytes>, so the fetcher cannot tell from the URL which failure it
// will get. A broken page sends that status with the standard reason phrase as its
// body (RFC 9110 leaves an error body to the server; the reason phrase is what most
// servers print) and no ETag, Last-Modified, Cache-Control or Expires, like every
// fault page: nothing a cache could store or revalidate by.
//
// THE FIXED PAGE IS THE DEPTH PAGE. Once fixed, the address serves byte for byte what
// /depth/html/<id>/<bytes> serves — one content definition everywhere, as the
// changeable page's version 1 is the depth page. Its codes are unique to the page
// because every run uses a fresh id. Last-Modified is the flip time (a fixed date
// could answer a date-only revalidation with a 304 for a page that never had a 200);
// otherwise it is the normal page (no Cache-Control, no Expires; DECISIONS.md § D13).
//
// AN ID NEVER CREATED IS A PLAIN 404 ("404 page not found"), never a silent broken
// page, so a mistake on our side — a runner asking about an id it never made, or a
// lost record — shows up instead of reading as "the vendor remembered a failure".
// A deliberate 404 says "Not Found"; the runner also knows which ids it created.
//
// A FIXED PAGE NEVER GOES BACK TO BROKEN. Repeating an action changes nothing (the
// first creation and flip times stay), as with the version switch.
//
// THE STATE IS SAVED TO A FILE (-heal-serverStateFile) and read back at start-up, so an
// origin restart mid-run does not turn fixed pages back into 404s. No file is an
// empty start; a file that cannot be read is a start-up error.

const (
	healPagePrefix  = "/heal/"
	controlHealPath = "/control/heal"
	healStateFormat = 1
	// defaultHealPageBytes is the size the control reports the fixed page's ETag
	// for when the caller names none: the 2 KB page every test uses.
	defaultHealPageBytes = 2048
)

// healFailures are the failures a page can be created with.
var healFailures = map[string]int{
	"503": http.StatusServiceUnavailable,
	"404": http.StatusNotFound,
	"403": http.StatusForbidden,
}

// healEntry is one page: its failure, when it was created, and when it was fixed
// (zero while broken). Times are to the second, as an HTTP date is.
type healEntry struct {
	Fail      string    `json:"fail"`
	CreatedAt time.Time `json:"created_at"`
	HealedAt  time.Time `json:"healed_at"`
}

func (e healEntry) healed() bool { return !e.HealedAt.IsZero() }

// healStateFile is the on-disk shape. Unknown fields are ignored on read.
type healStateFile struct {
	Format int                  `json:"format"`
	Pages  map[string]healEntry `json:"pages"`
}

// healPageState holds every healable page.
type healPageState struct {
	mu    sync.Mutex
	path  string // "" = memory only
	pages map[string]healEntry
	now   func() time.Time // a test swaps in a fake clock
}

// newHealPageState loads the state from path. An empty path is memory-only; a
// missing file is an empty start; a file that cannot be parsed is an error.
func newHealPageState(path string) (*healPageState, error) {
	hs := &healPageState{path: path, pages: map[string]healEntry{}, now: time.Now}
	if path == "" {
		return hs, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return hs, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read heal state %s: %w", path, err)
	}
	var f healStateFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("heal state %s is not readable (fix or move it; it is not overwritten): %w", path, err)
	}
	for id, e := range f.Pages {
		hs.pages[id] = e
	}
	return hs, nil
}

func (hs *healPageState) get(id string) (healEntry, bool) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	e, ok := hs.pages[id]
	return e, ok
}

// create makes id a broken page that fails with fail. Creating it again with the
// same failure changes nothing; another failure, or an id already fixed, is refused.
func (hs *healPageState) create(id, fail string) (healEntry, error) {
	if err := validVersionID(id); err != nil {
		return healEntry{}, err
	}
	if _, ok := healFailures[fail]; !ok {
		return healEntry{}, fmt.Errorf("fail must be one of %s, got %q", healFailureList(), fail)
	}
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if prev, had := hs.pages[id]; had {
		switch {
		case prev.healed():
			return healEntry{}, fmt.Errorf("%s is already fixed; a fixed page cannot go back to broken", id)
		case prev.Fail != fail:
			return healEntry{}, fmt.Errorf("%s already exists with fail=%s", id, prev.Fail)
		default:
			return prev, nil
		}
	}
	e := healEntry{Fail: fail, CreatedAt: hs.now().UTC().Truncate(time.Second)}
	hs.pages[id] = e
	if err := hs.saveLocked(); err != nil {
		delete(hs.pages, id)
		return healEntry{}, err
	}
	return e, nil
}

// heal fixes id. Fixing it again changes nothing (the first flip time stays); an id
// never created is refused.
func (hs *healPageState) heal(id string) (healEntry, error) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	prev, had := hs.pages[id]
	if !had {
		return healEntry{}, fmt.Errorf("%s was never created", id)
	}
	if prev.healed() {
		return prev, nil
	}
	e := prev
	e.HealedAt = hs.now().UTC().Truncate(time.Second)
	hs.pages[id] = e
	if err := hs.saveLocked(); err != nil {
		hs.pages[id] = prev
		return healEntry{}, err
	}
	return e, nil
}

// saveLocked writes the whole state atomically (temp file + rename). Caller holds hs.mu.
func (hs *healPageState) saveLocked() error {
	if hs.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(healStateFile{Format: healStateFormat, Pages: hs.pages}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(hs.path), ".heal-state-*")
	if err != nil {
		return fmt.Errorf("save heal state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("save heal state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("save heal state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("save heal state: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("save heal state: %w", err)
	}
	return os.Rename(tmpName, hs.path)
}

func healFailureList() string {
	var l []string
	for f := range healFailures {
		l = append(l, f)
	}
	sort.Strings(l)
	return strings.Join(l, ", ")
}

// --- the page route -------------------------------------------------------------

type healPageRoute struct{}

func (healPageRoute) prefix() string    { return healPagePrefix }
func (healPageRoute) needsSecret() bool { return true }

// healedBody is the fixed page: exactly the depth HTML page for (id, nbytes).
func healedBody(secret, id string, nbytes int) []byte {
	return renderDepthHTML(depthFormatLines(secret, id, nbytes))
}

func (healPageRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	id, nbytes, ok := parseSizePath(w, r, healPagePrefix, maxDepthPageBytes)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	e, known := s.healPages.get(id)
	if !known {
		http.NotFound(w, r) // never created by the control: not a page
		return nil, "", "", pageHeaders{}, false
	}
	if nbytes < minDepthFormatBytes {
		http.Error(w, fmt.Sprintf("page too small: %d bytes cannot hold the markers", nbytes), http.StatusBadRequest)
		return nil, "", "", pageHeaders{}, false
	}
	if !e.healed() {
		// Broken: the chosen status, its standard reason phrase, nothing to cache by.
		code := healFailures[e.Fail]
		http.Error(w, http.StatusText(code), code)
		return nil, "", "", pageHeaders{}, false
	}
	return healedBody(s.pageCodeSecret, id, nbytes), "depth.html", "text/html; charset=utf-8",
		pageHeaders{LastModified: e.HealedAt}, true
}

// --- the control ------------------------------------------------------------------

// healControlReply is what POST /control/heal returns: the page's state, plus the
// ETag the fixed page serves at the given size, so the runner can label each logged
// fetch (probe.jsonl etag_sent) as "got the fixed page" without fetching it itself.
type healControlReply struct {
	ID           string `json:"id"`
	State        string `json:"state"` // "broken" or "healed"
	Fail         string `json:"fail"`
	CreatedAt    string `json:"created_at"`              // RFC 3339, UTC
	HealedAt     string `json:"healed_at,omitempty"`     // RFC 3339, UTC; empty while broken
	LastModified string `json:"last_modified,omitempty"` // as the fixed page sends it
	Size         int    `json:"size"`
	ETag         string `json:"etag"` // the fixed page's ETag at Size
}

// serveHealControl handles the control. It reports whether it handled the request;
// with no control secret configured it returns false, so the caller 404s it.
//
//	POST /control/heal   id=<page id>
//	                     action=create  fail=503|404|403   (make it, broken)
//	                     action=heal                       (fix it)
//	                     size=<bytes>       (optional; which size's ETag to report, default 2048)
//	                     secret=<secret>    (or header X-Probe-Control-Secret)
func (s *staticServer) serveHealControl(w http.ResponseWriter, r *http.Request) bool {
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
		http.Error(w, "control: the page-code secret is not configured, so the fixed page cannot be served", http.StatusServiceUnavailable)
		return true
	}
	size := defaultHealPageBytes
	if raw := r.Form.Get("size"); raw != "" {
		var err error
		size, err = strconv.Atoi(raw)
		if err != nil || size < minDepthFormatBytes || size > maxDepthPageBytes {
			http.Error(w, "control: bad size", http.StatusBadRequest)
			return true
		}
	}

	id := r.Form.Get("id")
	var e healEntry
	var err error
	switch r.Form.Get("action") {
	case "create":
		e, err = s.healPages.create(id, r.Form.Get("fail"))
	case "heal":
		e, err = s.healPages.heal(id)
	default:
		err = fmt.Errorf("action must be create or heal")
	}
	if err != nil {
		http.Error(w, "control: "+err.Error(), http.StatusBadRequest)
		return true
	}

	rep := healControlReply{
		ID: id, State: "broken", Fail: e.Fail,
		CreatedAt: e.CreatedAt.Format(time.RFC3339),
		Size:      size,
		ETag:      pageETag(healedBody(s.pageCodeSecret, id, size)),
	}
	if e.healed() {
		rep.State = "healed"
		rep.HealedAt = e.HealedAt.Format(time.RFC3339)
		rep.LastModified = e.HealedAt.Format(http.TimeFormat)
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
