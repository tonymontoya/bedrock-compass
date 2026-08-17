package compass

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrLockHeld is returned when a backup operation cannot start because the
// backup lock is already claimed by a live compass process.
var ErrLockHeld = errors.New("a backup operation is already in progress")

// lockInfo is the .backup.lock content: who holds the lock and where the
// operation was when it stopped making progress.
type lockInfo struct {
	PID      int       `json:"pid"`
	Op       string    `json:"op"`
	Phase    string    `json:"phase"`
	Acquired time.Time `json:"acquired"`
}

func lockPath(e *Engine) string {
	return filepath.Join(e.BackupDir, ".backup.lock")
}

// acquireLock claims the backup lock atomically. An existing lock — even a
// stale one — fails fast: stale locks are recovered explicitly through
// CheckRecovery/FinishRecovery, never auto-claimed (Q7).
func (e *Engine) acquireLock(op string) error {
	if err := os.MkdirAll(e.BackupDir, 0o755); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}
	data, err := json.Marshal(lockInfo{PID: os.Getpid(), Op: op, Phase: "begin", Acquired: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("marshal lock: %w", err)
	}
	f, err := os.OpenFile(lockPath(e), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrLockHeld, e.describeLock())
		}
		return fmt.Errorf("acquire lock: %w", err)
	}
	if _, werr := f.Write(data); werr != nil {
		f.Close()
		os.Remove(lockPath(e))
		return fmt.Errorf("write lock: %w", werr)
	}
	return f.Close()
}

// releaseLock clears the lock. Idempotent.
func (e *Engine) releaseLock() {
	os.Remove(lockPath(e))
}

// setLockPhase records the current stage for a crash post-mortem. Best
// effort: a phase update must never fail the operation.
func (e *Engine) setLockPhase(phase string) {
	li, err := e.readLock()
	if err != nil {
		return
	}
	li.Phase = phase
	data, err := json.Marshal(li)
	if err != nil {
		return
	}
	_ = os.WriteFile(lockPath(e), data, 0o644)
}

func (e *Engine) readLock() (lockInfo, error) {
	data, err := os.ReadFile(lockPath(e))
	if err != nil {
		return lockInfo{}, err
	}
	var li lockInfo
	if err := json.Unmarshal(data, &li); err != nil {
		return lockInfo{}, fmt.Errorf("parse lock: %w", err)
	}
	return li, nil
}

func (e *Engine) describeLock() string {
	li, err := e.readLock()
	if err != nil {
		return "lock file present but unreadable"
	}
	return fmt.Sprintf("pid %d, op %s, phase %s (use CheckRecovery/FinishRecovery)", li.PID, li.Op, li.Phase)
}

// lockHolderLive reports whether the lock's holder process is alive and
// whether it looks like a compass binary. A live pid running some other
// program means the pid was reused: the lock is stale (pid-reuse guard).
func lockHolderLive(pid int) (alive, isCompass bool) {
	if pid <= 0 {
		return false, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false, false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return true, false
	}
	return true, strings.Contains(string(data), "compass")
}

// holderStale is the decision used by recovery: a lock is stale when its
// holder is dead or is not a compass process.
func holderStale(pid int) bool {
	alive, isCompass := lockHolderLive(pid)
	return !alive || !isCompass
}

// RecoveryStatus is the read-only report from CheckRecovery.
type RecoveryStatus struct {
	LockPresent bool
	Stale       bool
	Holder      lockInfo
	TrashDirs   []string // manifest-less dirs (incomplete backups)
}

// CheckRecovery inspects the backup dir for an interrupted operation and
// reports it. It changes nothing.
func (e *Engine) CheckRecovery() (*RecoveryStatus, error) {
	st := &RecoveryStatus{}
	li, err := e.readLock()
	switch {
	case err == nil:
		st.LockPresent = true
		st.Holder = li
		st.Stale = holderStale(li.PID)
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}

	entries, err := os.ReadDir(e.BackupDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return nil, err
	}
	for _, en := range entries {
		if !en.IsDir() {
			continue
		}
		if _, ierr := e.Inspect(en.Name()); ierr != nil {
			st.TrashDirs = append(st.TrashDirs, en.Name())
		}
	}
	return st, nil
}

// FinishRecoveryResult reports what FinishRecovery did.
type FinishRecoveryResult struct {
	Resumed     bool     // save resume was sent for an interrupted live op
	ResumedErr  string   // the resume attempt did not confirm
	Cleaned     []string // trash dirs removed
	LockCleared bool
	ServerDown  bool // a stopped op left the server not running (operator starts it)
}

// FinishRecovery acts on an interrupted operation: resumes a live op that
// died before its resume, removes incomplete trash, clears the stale lock,
// and audits the actions. It never starts the server (Q7: explicit human
// actions only) and is a no-op when no lock is present.
func (e *Engine) FinishRecovery(ctx context.Context) (*FinishRecoveryResult, error) {
	start := time.Now()
	res := &FinishRecoveryResult{}

	li, err := e.readLock()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return res, nil
		}
		return nil, err
	}
	if !holderStale(li.PID) {
		return nil, fmt.Errorf("%w: %s", ErrLockHeld, e.describeLock())
	}

	if li.Op == ModeLive && li.Phase != "manifest" && e.Console != nil {
		out, rerr := e.Console.Send(ctx, "save resume")
		switch {
		case rerr == nil && strings.Contains(out, resumeOK):
			res.Resumed = true
		case rerr == nil:
			res.ResumedErr = oneLine(out)
		default:
			res.ResumedErr = rerr.Error()
		}
	}

	entries, err := os.ReadDir(e.BackupDir)
	if err == nil {
		for _, en := range entries {
			if !en.IsDir() {
				continue
			}
			if _, ierr := e.Inspect(en.Name()); ierr != nil {
				if rerr := os.RemoveAll(filepath.Join(e.BackupDir, en.Name())); rerr == nil {
					res.Cleaned = append(res.Cleaned, en.Name())
				}
			}
		}
	}

	if li.Op == ModeStopped && e.Sup != nil {
		if running, rerr := e.Sup.Running(ctx); rerr == nil && !running {
			res.ServerDown = true
		}
	}

	if err := os.Remove(lockPath(e)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, fmt.Errorf("clear lock: %w", err)
	}
	res.LockCleared = true

	actions := []string{}
	if res.Resumed {
		actions = append(actions, "resumed")
	}
	if res.ResumedErr != "" {
		actions = append(actions, "resume-unconfirmed")
	}
	if len(res.Cleaned) > 0 {
		actions = append(actions, "cleaned:"+strings.Join(res.Cleaned, ","))
	}
	if res.ServerDown {
		actions = append(actions, "server-down")
	}
	actions = append(actions, "lock-cleared")
	e.audit("recover-interrupted-save", map[string]string{
		"op":      li.Op,
		"phase":   li.Phase,
		"actions": strings.Join(actions, "; "),
	}, "ok", time.Since(start))
	return res, nil
}
