package compass

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeLock(t *testing.T, backupDir string, pid int, op, phase string) {
	t.Helper()
	li := lockInfo{PID: pid, Op: op, Phase: phase}
	data, err := json.Marshal(li)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, ".backup.lock"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEngine_Lock_ConcurrentFailsFast(t *testing.T) {
	serverDir := newFixtureServer(t)
	backupDir := t.TempDir()
	// A live compass process (this test binary) holds the lock.
	writeLock(t, backupDir, os.Getpid(), ModeLive, "copy")
	c, sent := newScriptedConsole(t, map[string]string{
		"save hold":   "Saving...",
		"save query":  "Data saved. Files are now ready to be copied. TestWorld/level.dat: 5",
		"save resume": "Changes to the level are resumed.",
	})
	e := &Engine{ServerDir: serverDir, BackupDir: backupDir, Retention: 7, Console: c}

	_, err := e.LiveBackup(context.Background())
	if err == nil || !errors.Is(err, ErrLockHeld) {
		t.Fatalf("err = %v, want ErrLockHeld (fail fast)", err)
	}
	if len(*sent) != 0 {
		t.Errorf("console commands = %v, want none (never touched the server)", *sent)
	}
}

func TestEngine_Lock_StaleFailsFast(t *testing.T) {
	serverDir := newFixtureServer(t)
	backupDir := t.TempDir()
	writeLock(t, backupDir, 999999999, ModeLive, "copy")
	c, _ := newScriptedConsole(t, map[string]string{
		"save hold":   "Saving...",
		"save query":  "Data saved. Files are now ready to be copied. TestWorld/level.dat: 5",
		"save resume": "Changes to the level are resumed.",
	})
	e := &Engine{ServerDir: serverDir, BackupDir: backupDir, Retention: 7, Console: c}

	_, err := e.LiveBackup(context.Background())
	if err == nil || !errors.Is(err, ErrLockHeld) {
		t.Fatalf("err = %v, want ErrLockHeld (stale locks are recovered explicitly, never auto-claimed)", err)
	}
}

func TestEngine_Lock_ReleasedAfterSuccess(t *testing.T) {
	serverDir := newFixtureServer(t)
	backupDir := t.TempDir()
	c, _ := newScriptedConsole(t, map[string]string{
		"save hold":   "Saving...",
		"save query":  "Data saved. Files are now ready to be copied. TestWorld/level.dat: 5",
		"save resume": "Changes to the level are resumed.",
	})
	e := &Engine{ServerDir: serverDir, BackupDir: backupDir, Retention: 7, Console: c}

	if _, err := e.LiveBackup(context.Background()); err != nil {
		t.Fatalf("LiveBackup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backupDir, ".backup.lock")); !os.IsNotExist(err) {
		t.Error("lock must be released after a successful backup")
	}
}

func TestEngine_CheckRecovery_ReportsStaleAndTrash(t *testing.T) {
	backupDir := t.TempDir()
	writeLock(t, backupDir, 999999999, ModeLive, "copy")
	if err := os.MkdirAll(filepath.Join(backupDir, "20260816-110000"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureBackup(t, backupDir, "20260816-100000", manifestJSON("20260816-100000", ModeLive))
	e := &Engine{BackupDir: backupDir, Retention: 7}

	st, err := e.CheckRecovery()
	if err != nil {
		t.Fatalf("CheckRecovery: %v", err)
	}
	if !st.LockPresent || !st.Stale {
		t.Errorf("status = %+v, want a stale lock reported", st)
	}
	if st.Holder.Op != ModeLive || st.Holder.Phase != "copy" {
		t.Errorf("holder = %+v, want op=live phase=copy", st.Holder)
	}
	if len(st.TrashDirs) != 1 || st.TrashDirs[0] != "20260816-110000" {
		t.Errorf("trash = %v, want the manifest-less dir", st.TrashDirs)
	}
}

func TestEngine_CheckRecovery_LiveHolderNotStale(t *testing.T) {
	backupDir := t.TempDir()
	writeLock(t, backupDir, os.Getpid(), ModeLive, "copy")
	e := &Engine{BackupDir: backupDir, Retention: 7}

	st, err := e.CheckRecovery()
	if err != nil {
		t.Fatalf("CheckRecovery: %v", err)
	}
	if !st.LockPresent || st.Stale {
		t.Errorf("status = %+v, want lock present and NOT stale (live compass holder)", st)
	}
}

func TestEngine_CheckRecovery_NoLock_NoTrash(t *testing.T) {
	backupDir := t.TempDir()
	writeFixtureBackup(t, backupDir, "20260816-100000", manifestJSON("20260816-100000", ModeLive))
	e := &Engine{BackupDir: backupDir, Retention: 7}

	st, err := e.CheckRecovery()
	if err != nil {
		t.Fatalf("CheckRecovery: %v", err)
	}
	if st.LockPresent || st.Stale || len(st.TrashDirs) != 0 {
		t.Errorf("status = %+v, want a clean state", st)
	}
}

func TestEngine_FinishRecovery_LiveInHold_ResumesCleansClears(t *testing.T) {
	backupDir := t.TempDir()
	writeLock(t, backupDir, 999999999, ModeLive, "copy")
	if err := os.MkdirAll(filepath.Join(backupDir, "20260816-110000"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureBackup(t, backupDir, "20260816-100000", manifestJSON("20260816-100000", ModeLive))
	c, sent := newScriptedConsole(t, map[string]string{
		"save resume": "Changes to the level are resumed.",
	})
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{ServerDir: newFixtureServer(t), BackupDir: backupDir, Retention: 7, Console: c, Trail: trail}

	res, err := e.FinishRecovery(context.Background())
	if err != nil {
		t.Fatalf("FinishRecovery: %v", err)
	}
	if !res.Resumed {
		t.Error("must resume the interrupted live op")
	}
	if !res.LockCleared {
		t.Error("must clear the stale lock")
	}
	if len(res.Cleaned) != 1 || res.Cleaned[0] != "20260816-110000" {
		t.Errorf("cleaned = %v, want the trash dir", res.Cleaned)
	}
	if !reflect.DeepEqual(*sent, []string{"save resume"}) {
		t.Errorf("console = %v, want exactly [save resume]", *sent)
	}
	if _, err := os.Stat(filepath.Join(backupDir, ".backup.lock")); !os.IsNotExist(err) {
		t.Error("lock file must be gone")
	}
	if _, err := os.Stat(filepath.Join(backupDir, "20260816-110000")); !os.IsNotExist(err) {
		t.Error("trash dir must be gone")
	}
	if _, err := os.Stat(filepath.Join(backupDir, "20260816-100000", "manifest.json")); err != nil {
		t.Error("the complete backup must survive")
	}
	events, err := trail.Recent(10)
	if err != nil || len(events) != 1 {
		t.Fatalf("audit = %+v, %v", events, err)
	}
	if events[0].Verb != "recover-interrupted-save" {
		t.Errorf("verb = %q, want recover-interrupted-save", events[0].Verb)
	}
	if events[0].Params["op"] != ModeLive || events[0].Params["phase"] != "copy" {
		t.Errorf("params = %v", events[0].Params)
	}
	if !strings.Contains(events[0].Params["actions"], "resumed") {
		t.Errorf("actions = %q, want resumed recorded", events[0].Params["actions"])
	}
}

func TestEngine_FinishRecovery_LiveDiedDuringResume_StillResumes(t *testing.T) {
	// A live op that died AFTER "save resume" was requested but before its
	// response was confirmed is still in hold. The spec requires FinishRecovery
	// to resume it ("resume if the op was live, still in hold") — so the phase
	// gate must not skip the resume just because the phase advanced past
	// "resume". A server must never be left stuck in save hold.
	backupDir := t.TempDir()
	writeLock(t, backupDir, 999999999, ModeLive, "resume")
	c, sent := newScriptedConsole(t, map[string]string{
		"save resume": "Changes to the level are resumed.",
	})
	e := &Engine{ServerDir: newFixtureServer(t), BackupDir: backupDir, Retention: 7, Console: c}

	res, err := e.FinishRecovery(context.Background())
	if err != nil {
		t.Fatalf("FinishRecovery: %v", err)
	}
	if !res.Resumed {
		t.Error("must resume a live op that died during the resume step (still in hold)")
	}
	if !reflect.DeepEqual(*sent, []string{"save resume"}) {
		t.Errorf("console = %v, want exactly [save resume]", *sent)
	}
}

func TestEngine_FinishRecovery_NoLock_NoOp(t *testing.T) {
	backupDir := t.TempDir()
	c, sent := newScriptedConsole(t, map[string]string{"save resume": "Changes to the level are resumed."})
	e := &Engine{ServerDir: newFixtureServer(t), BackupDir: backupDir, Retention: 7, Console: c}

	res, err := e.FinishRecovery(context.Background())
	if err != nil {
		t.Fatalf("FinishRecovery with no lock must be a no-op: %v", err)
	}
	if res.Resumed || res.LockCleared || len(res.Cleaned) != 0 {
		t.Errorf("result = %+v, want nothing done", res)
	}
	if len(*sent) != 0 {
		t.Errorf("console = %v, want no commands", *sent)
	}
}

func TestEngine_FinishRecovery_StoppedOp_ServerLeftDown(t *testing.T) {
	backupDir := t.TempDir()
	writeLock(t, backupDir, 999999999, ModeStopped, "copy")
	sup, calls := newFakeSup(nil, nil, false) // server not running
	e := &Engine{ServerDir: newFixtureServer(t), BackupDir: backupDir, Retention: 7, Sup: sup}

	res, err := e.FinishRecovery(context.Background())
	if err != nil {
		t.Fatalf("FinishRecovery: %v", err)
	}
	if !res.ServerDown {
		t.Error("must report the server was left down (operator starts it)")
	}
	if res.Resumed {
		t.Error("a stopped op must not be resumed via the console")
	}
	if len(*calls) != 0 {
		t.Errorf("sup calls = %v, want none (never auto-start)", *calls)
	}
}
