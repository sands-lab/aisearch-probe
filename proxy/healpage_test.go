package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// THE HEALABLE PAGE (remembered-failure redo; docs/experiments/heal.md
// Part 2). One address that fails until the control fixes it, so the time a page
// recovers is ours and exact — not "a set time after the first request", which a
// vendor sending several requests per ask blurs. These tests pin the spec first:
//   - an id is created BROKEN with a chosen failure (503, 404 or 403); the failure is
//     server state, never in the address, so the fetcher cannot read it off the URL
//   - a broken page sends that status with the standard reason phrase as its body
//     and NO ETag, Last-Modified, Cache-Control or Expires (like every fault page)
//   - one flip makes it the 2 KB depth page, byte for byte /depth/html/ for the
//     same id, with Last-Modified = the flip time and otherwise normal headers
//   - it can never go back to broken; an id never created is a plain 404
//   - the control is secret-guarded; the state survives a restart (saved to a file)

// healServer is a static server with both secrets and a fake clock, so creation and
// flip times are deterministic.
func healServer(t *testing.T, statePath string) (*staticServer, *time.Time) {
	t.Helper()
	st := newStaticServer(t.TempDir(), "")
	st.pageCodeSecret = fmtTestSecret
	st.controlSecret = verTestControlSecret
	hs, err := newHealPageState(statePath)
	if err != nil {
		t.Fatalf("newHealPageState: %v", err)
	}
	clock := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	hs.now = func() time.Time { return clock }
	st.healPages = hs
	return st, &clock
}

// healCtl drives the control the way the runner will. It returns the status and,
// on 200, the decoded reply.
func healCtl(t *testing.T, base string, form url.Values) (int, healControlReply) {
	t.Helper()
	resp := postForm(t, base+controlHealPath, form,
		map[string]string{"X-Probe-Control-Secret": verTestControlSecret})
	b := body(t, resp)
	var rep healControlReply
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(b), &rep); err != nil {
			t.Fatalf("control reply is not JSON: %v\n%s", err, b)
		}
	}
	return resp.StatusCode, rep
}

func create(t *testing.T, base, id, fail string) healControlReply {
	t.Helper()
	code, rep := healCtl(t, base, url.Values{"id": {id}, "action": {"create"}, "fail": {fail}})
	if code != http.StatusOK {
		t.Fatalf("create %s fail=%s: status %d", id, fail, code)
	}
	return rep
}

func heal(t *testing.T, base, id string) healControlReply {
	t.Helper()
	code, rep := healCtl(t, base, url.Values{"id": {id}, "action": {"heal"}})
	if code != http.StatusOK {
		t.Fatalf("heal %s: status %d", id, code)
	}
	return rep
}

// noValidators: a broken page must carry nothing a cache could store or revalidate by.
func noValidators(t *testing.T, label string, resp *http.Response) {
	t.Helper()
	for _, h := range []string{"Etag", "Last-Modified", "Cache-Control", "Expires"} {
		if _, present := resp.Header[h]; present {
			t.Errorf("%s: broken page sent %s: %q", label, h, resp.Header.Get(h))
		}
	}
}

// The whole life of one page: unknown -> broken -> fixed, for each failure kind.
func TestHealPageLifecycle(t *testing.T) {
	for _, c := range []struct {
		fail   string
		status int
		text   string
	}{
		{"503", http.StatusServiceUnavailable, "Service Unavailable"},
		{"404", http.StatusNotFound, "Not Found"},
		{"403", http.StatusForbidden, "Forbidden"},
	} {
		t.Run(c.fail, func(t *testing.T) {
			st, clock := healServer(t, "")
			srv, _, _ := newTestProxy(t, st, "")
			id := "hp-" + c.fail
			page := srv.URL + "/heal/" + id + "/2048"

			// Never created: a plain 404, with Go's own wording, so it can never be
			// mistaken for a created page that fails with 404.
			resp := get(t, page, nil)
			if b := body(t, resp); resp.StatusCode != http.StatusNotFound || b != "404 page not found\n" {
				t.Fatalf("uncreated id: status %d body %q, want 404 %q", resp.StatusCode, b, "404 page not found\n")
			}

			rep := create(t, srv.URL, id, c.fail)
			if rep.State != "broken" || rep.Fail != c.fail || rep.CreatedAt != clock.Format(time.RFC3339) || rep.HealedAt != "" {
				t.Errorf("create reply = %+v", rep)
			}
			resp = get(t, page, nil)
			b := body(t, resp)
			if resp.StatusCode != c.status || b != c.text+"\n" {
				t.Errorf("broken: status %d body %q, want %d %q", resp.StatusCode, b, c.status, c.text+"\n")
			}
			noValidators(t, "broken "+c.fail, resp)

			*clock = clock.Add(90 * time.Second)
			rep = heal(t, srv.URL, id)
			flip := clock.UTC().Truncate(time.Second)
			if rep.State != "healed" || rep.HealedAt != flip.Format(time.RFC3339) || rep.LastModified != flip.Format(http.TimeFormat) {
				t.Errorf("heal reply = %+v", rep)
			}

			// Fixed: byte for byte the depth page for the same id, Last-Modified =
			// the flip time, otherwise the normal page.
			resp = get(t, page, nil)
			got := body(t, resp)
			want := body(t, get(t, srv.URL+"/depth/html/"+id+"/2048", nil))
			if resp.StatusCode != http.StatusOK || got != want {
				t.Fatalf("healed: status %d; body equals the depth page: %v", resp.StatusCode, got == want)
			}
			if lm := resp.Header.Get("Last-Modified"); lm != flip.Format(http.TimeFormat) {
				t.Errorf("healed Last-Modified = %q, want the flip time %q", lm, flip.Format(http.TimeFormat))
			}
			if et := resp.Header.Get("ETag"); et == "" || et != rep.ETag {
				t.Errorf("healed ETag = %q, reply said %q", et, rep.ETag)
			}
			for _, h := range []string{"Cache-Control", "Expires"} {
				if _, present := resp.Header[h]; present {
					t.Errorf("healed page sent %s — it must be the normal page", h)
				}
			}
		})
	}
}

// The control's rules, the same as the version switch's: repeating an action changes
// nothing; a fixed page never goes back; nothing happens to an id never created.
func TestHealControlRules(t *testing.T) {
	st, clock := healServer(t, "")
	srv, _, _ := newTestProxy(t, st, "")

	first := create(t, srv.URL, "r1", "503")
	*clock = clock.Add(time.Minute)
	again := create(t, srv.URL, "r1", "503")
	if again.CreatedAt != first.CreatedAt {
		t.Errorf("re-creating with the same failure must change nothing: %s -> %s", first.CreatedAt, again.CreatedAt)
	}
	for _, c := range []struct {
		name string
		form url.Values
	}{
		{"another failure on an existing page", url.Values{"id": {"r1"}, "action": {"create"}, "fail": {"404"}}},
		{"an unsupported failure", url.Values{"id": {"r2"}, "action": {"create"}, "fail": {"500"}}},
		{"no failure named", url.Values{"id": {"r2"}, "action": {"create"}}},
		{"heal an id never created", url.Values{"id": {"r3"}, "action": {"heal"}}},
		{"an unknown action", url.Values{"id": {"r1"}, "action": {"break"}}},
		{"an id that is not one path segment", url.Values{"id": {"a/b"}, "action": {"create"}, "fail": {"503"}}},
	} {
		if code, _ := healCtl(t, srv.URL, c.form); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", c.name, code)
		}
	}

	*clock = clock.Add(time.Minute)
	healed := heal(t, srv.URL, "r1")
	*clock = clock.Add(time.Minute)
	if twice := heal(t, srv.URL, "r1"); twice.HealedAt != healed.HealedAt {
		t.Errorf("healing twice must keep the first flip time: %s -> %s", healed.HealedAt, twice.HealedAt)
	}
	if code, _ := healCtl(t, srv.URL, url.Values{"id": {"r1"}, "action": {"create"}, "fail": {"503"}}); code != http.StatusBadRequest {
		t.Errorf("re-creating a fixed page (back to broken) must be refused, got %d", code)
	}
}

// The control is a secret-guarded POST, off entirely without a control secret, and
// refuses to make pages it could not serve (no page-code secret).
func TestHealControlGuards(t *testing.T) {
	st, _ := healServer(t, "")
	srv, _, _ := newTestProxy(t, st, "")
	form := url.Values{"id": {"g1"}, "action": {"create"}, "fail": {"503"}}

	if resp := postForm(t, srv.URL+controlHealPath, form, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("no secret: status %d, want 403", resp.StatusCode)
	}
	if resp := get(t, srv.URL+controlHealPath, nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET on the control: status %d, want 405", resp.StatusCode)
	}

	off, _ := healServer(t, "")
	off.controlSecret = ""
	srvOff, _, _ := newTestProxy(t, off, "")
	if resp := postForm(t, srvOff.URL+controlHealPath, form, map[string]string{"X-Probe-Control-Secret": verTestControlSecret}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("no control secret configured: status %d, want 404 (not a real endpoint)", resp.StatusCode)
	}

	noCode, _ := healServer(t, "")
	noCode.pageCodeSecret = ""
	srvNoCode, _, _ := newTestProxy(t, noCode, "")
	if resp := postForm(t, srvNoCode.URL+controlHealPath, form, map[string]string{"X-Probe-Control-Secret": verTestControlSecret}); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no page-code secret: status %d, want 503 (the fixed page could not be served)", resp.StatusCode)
	}
}

// A restart must not lose a page: the state is saved on every change and read back
// at start-up; a missing file is an empty start; an unreadable one is an error.
func TestHealStateSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heal-state.json")
	st, clock := healServer(t, path)
	srv, _, _ := newTestProxy(t, st, "")
	create(t, srv.URL, "keep-broken", "404")
	create(t, srv.URL, "keep-healed", "503")
	*clock = clock.Add(time.Minute)
	healed := heal(t, srv.URL, "keep-healed")

	again, err := newHealPageState(path)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := again.get("keep-broken"); !ok || e.Fail != "404" || !e.HealedAt.IsZero() {
		t.Errorf("broken page after restart = %+v, %v", e, ok)
	}
	if e, ok := again.get("keep-healed"); !ok || e.HealedAt.Format(time.RFC3339) != healed.HealedAt {
		t.Errorf("healed page after restart = %+v, %v", e, ok)
	}

	if hs, err := newHealPageState(filepath.Join(t.TempDir(), "missing.json")); err != nil || len(hs.pages) != 0 {
		t.Errorf("a missing file must be an empty start: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newHealPageState(bad); err == nil {
		t.Error("an unreadable file must be a start-up error, never a silent empty start")
	}
}
