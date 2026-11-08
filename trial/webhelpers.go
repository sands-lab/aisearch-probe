package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Helpers for the web-app sharing helper: the reply a person pastes, and the raw
// record kept for each ask.

// standardConfirmReply is the ONE fixed line the console gives, identical for
// every app, when an app asks permission before browsing and we are running the
// "when permitted" (confirmed) condition. It must never be improvised per app, or
// the confirmed results stop being comparable.
const standardConfirmReply = "Yes, please go ahead."

// webRawRecord is one line of an append-only raw run file: the trial's identity
// plus the verbatim reply. Everything else in the dataset is derived from this
// and the server log.
type webRawRecord struct {
	TrialID  string  `json:"trial_id"`
	RunID    string  `json:"run_id"`
	SentUTC  string  `json:"sent_utc"`
	SentUnix float64 `json:"sent_unix"`
	Phase    string  `json:"phase"`
	App      string  `json:"app"`
	Config   string  `json:"config"`
	PageURL  string  `json:"page_url"`
	Marker   string  `json:"marker"`
	Reply    string  `json:"reply"`

	// Ask-first handling. AskedBack: the app asked a question / for permission in
	// round 1 instead of acting. Confirmed: we then gave standardConfirmReply and
	// Reply2 is the app's reply to it ("when permitted" condition).
	AskedBack   bool   `json:"asked_back,omitempty"`
	Confirmed   bool   `json:"confirmed,omitempty"`
	ConfirmText string `json:"confirm_text,omitempty"`
	Reply2      string `json:"reply2,omitempty"`

	// ReaskOf, when set, is the trial_id of an earlier trial whose SAME page this
	// trial asked about again (the holds-a-copy test). The page URL is reused; the
	// trial id is new.
	ReaskOf string `json:"reask_of,omitempty"`

	// Notes is a free-text hook for anything unexpected the person saw.
	Notes string `json:"notes,omitempty"`

	// AnsweredUnix: when the reply was pasted back (-sharingWeb-run only). Requests
	// after it cannot have produced the answer.
	AnsweredUnix float64 `json:"answered_unix,omitempty"`
}

// waitAndAlert sleeps until untilUnix, then rings the terminal bell several times
// so the person notices without watching the terminal (the bell reaches the local
// terminal even over ssh). Being late is fine: the next ask records its own real
// time, so the measured gap is exact regardless.
func waitAndAlert(untilUnix float64, label string) {
	if remain := untilUnix - nowUnix(); remain > 0 {
		fmt.Printf("\n  waiting %s for the next step (%s) — you'll hear a beep…\n",
			time.Duration(remain*float64(time.Second)).Round(time.Second), label)
	}
	for {
		remain := untilUnix - nowUnix()
		if remain <= 0 {
			break
		}
		time.Sleep(time.Duration(math.Min(remain, 5) * float64(time.Second)))
	}
	for i := 0; i < 6; i++ {
		fmt.Print("\a")
		time.Sleep(250 * time.Millisecond)
	}
}

func nowUnix() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// captureReplyViaEditor opens the person's editor on an empty temp file and reads
// back whatever they saved. Empty file, no instruction lines: a chat reply can
// start with '#' (a markdown heading), so we must not strip any lines. The temp
// file is removed afterwards.
func captureReplyViaEditor(trialID string) (string, error) {
	f, err := os.CreateTemp("", "webtrial-"+safeName(trialID)+"-*.txt")
	if err != nil {
		return "", err
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	name, args := editorCommand()
	args = append(args, path)
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("running %s: %w", name, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// editorCommand picks the editor: $VISUAL, then $EDITOR, then the first installed
// of vim, vi, nano. An editor set with flags (e.g. "code -w") is honoured by
// splitting on spaces.
func editorCommand() (string, []string) {
	for _, env := range []string{os.Getenv("VISUAL"), os.Getenv("EDITOR")} {
		if fields := strings.Fields(env); len(fields) > 0 {
			return fields[0], fields[1:]
		}
	}
	for _, cand := range []string{"vim", "vi", "nano"} {
		if _, err := exec.LookPath(cand); err == nil {
			return cand, nil
		}
	}
	return "vi", nil // last resort; almost always present
}

// safeName keeps a temp-file fragment to characters that are safe in a filename.
func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		default:
			return '_'
		}
	}, s)
}

// appendRaw appends one raw record to a run file. Append-only: the raw log is the
// observation and must survive a crash mid-batch.
func appendRaw(path string, rec webRawRecord) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(rec)
}
