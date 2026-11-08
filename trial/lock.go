package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// A lock that says "an experiment is in progress, and it owns these files".
//
// Written because the rule was broken 2026-09-02, by me, mid-run: the staleness
// experiment was five minutes into a two-hour run when a separate command
// rewrote the very page it was measuring. Nothing was corrupted — no request
// landed in the two-second window — but that was luck, and the study's own
// standing rule (DECISIONS.md § D4, content frozen during a run) had held only
// as long as everybody remembered it.
//
// A rule that depends on memory is not a rule. This makes the run announce
// itself, so anything about to touch probe-site/ can see that it must not.

// runLockPath is where a live run announces itself. Under logs/ because that
// directory always exists for a run and is never checked into git.
const runLockPath = "logs/.probe-run.lock"

// retentionLockPath is a SEPARATE lock for the multi-day retention probe. It only
// self-excludes retention ticks from each other (so a slow tick and the next
// cron firing cannot double-fire an action); it is deliberately NOT the shared
// runLockPath, because retention touches only unique /page/<id> addresses and so
// cannot contaminate probe-site/ or collide with a dev run. Sharing the run lock
// would force needless mutual exclusion every few hours. See DECISIONS / the
// retention runner.
const retentionLockPath = "logs/.retention.lock"

// negPairedLockPath is a SEPARATE lock for the paired negative-caching run, for
// the same reason as retentionLockPath: it runs for a day-plus and touches only
// unique /page/<id> and /fault/ addresses, never probe-site/, so it must not grab
// the shared runLockPath and needlessly block dev runs for that whole time. It
// self-excludes two paired runs from each other, and nothing more.
const negPairedLockPath = "logs/.negcache-paired.lock"

// objsizeLockPath is a SEPARATE lock for the object-size sweep, for the same
// reason as retentionLockPath and negPairedLockPath: it touches only unique
// /sizepage/<id>/<bytes> addresses, never probe-site/ or /page/, so it must not
// grab the shared runLockPath and block dev runs. It self-excludes two size runs
// from each other, and nothing more.
const objsizeLockPath = "logs/.objsize.lock"

// truncLockPath is a SEPARATE lock for the truncation-depth probe, for the same
// reason as the others: it touches only unique /depthpage/<id>/<bytes> addresses,
// never probe-site/ or /page/, so it must not grab the shared runLockPath and block
// dev runs. It self-excludes two depth runs from each other, and nothing more.
const truncLockPath = "logs/.truncprobe.lock"

// ttlmapLockPath is a SEPARATE lock for the TTL-map run, for the same reason as
// the others: it touches only generated /page/<id> addresses, never probe-site/,
// so it must not grab the shared runLockPath and block dev runs. It self-excludes
// two TTL-map runs from each other, and nothing more.
const ttlmapLockPath = "logs/.ttlmap.lock"

// contentReuseLockPath is a SEPARATE lock for the content-level-reuse run, for
// the same reason as the others: it touches only unique /cpage/<id> addresses,
// never probe-site/ or /page/, so it must not grab the shared runLockPath and
// block dev runs. It self-excludes two content-reuse runs from each other.
const contentReuseLockPath = "logs/.content-reuse.lock"

// The correction run's ticks take a SEPARATE lock per sitting (correctionLockFor
// in correctionrun.go, next to the sitting's state file): they touch only their own
// changeable pages (/correction/html/, /correction/txt/), never probe-site/, so they must
// not grab runLockPath, and sittings started on different days must not block
// each other. The lock stops two ticks firing the same sitting's step twice.

type runLockInfo struct {
	PID     int      `json:"pid"`
	What    string   `json:"what"`
	Started string   `json:"started"`
	Owns    []string `json:"owns"` // files this run will modify or measure
}

type runLock struct{ path string }

// acquireRunLock refuses to start when another run is live. A lock whose process
// is gone is treated as debris and replaced — a crash must not block the next
// run for ever.
func acquireRunLock(path, what string, owns []string) (*runLock, error) {
	if info, alive := readRunLock(path); alive {
		return nil, fmt.Errorf("another run is in progress (pid %d, %s, started %s)\n"+
			"  it owns: %v\n"+
			"  wait for it to finish, or stop it, before starting another — two runs\n"+
			"  sharing these pages would corrupt both",
			info.PID, info.What, info.Started, info.Owns)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	raw, err := json.MarshalIndent(runLockInfo{
		PID: os.Getpid(), What: what,
		Started: time.Now().UTC().Format(time.RFC3339), Owns: owns,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return nil, err
	}
	return &runLock{path: path}, nil
}

// readRunLock reports what the lock says and whether that process still exists.
func readRunLock(path string) (runLockInfo, bool) {
	var info runLockInfo
	raw, err := os.ReadFile(path)
	if err != nil {
		return info, false
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return info, false
	}
	if info.PID <= 0 {
		return info, false
	}
	// Signal 0 asks "does this process exist and may I signal it?" without
	// sending anything.
	if err := syscall.Kill(info.PID, 0); err != nil {
		return info, false // gone, or not ours: the lock is debris
	}
	return info, true
}

func (l *runLock) Release() {
	if l != nil {
		os.Remove(l.path)
	}
}
