//go:build integration

package compass

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests drive the full backup engine against the scratch rig (a real
// BDS 1.26.x under systemd with its console on a FIFO). They self-skip
// when the rig unit is not active and are opt-in: -tags integration,
// shipped via make integ RIG_HOST=<target>.

func rigServerDir() string {
	return envOr("COMPASS_TEST_SERVER_DIR", "/opt/test-bedrock")
}

func rigEngine(t *testing.T, backupDir string) *Engine {
	t.Helper()
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{
		ServerDir: rigServerDir(),
		BackupDir: backupDir,
		Retention: 7,
		Console:   rigConsole(t),
		Sup:       NewSystemdSupervisor(rigUnit(), nil),
		Trail:     trail,
	}
}

// TestIntegration_Backup_Live runs a live backup on the real server and
// verifies the manifest and that the world is resumed (server still live
// and answering the console).
func TestIntegration_Backup_Live(t *testing.T) {
	skipIfRigDown(t)
	e := rigEngine(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	b, err := e.LiveBackup(ctx)
	if err != nil {
		t.Fatalf("LiveBackup: %v", err)
	}

	got, err := e.Inspect(b.ID)
	if err != nil {
		t.Fatalf("Inspect(%s): %v", b.ID, err)
	}
	if got.Mode != ModeLive || got.World == "" || len(got.Files) == 0 {
		t.Fatalf("manifest = %+v, want a complete live backup", got)
	}
	foundLevelDat := false
	for _, f := range got.Files {
		if strings.HasSuffix(f.Path, "level.dat") {
			foundLevelDat = true
			data, err := os.ReadFile(filepath.Join(e.BackupDir, b.ID, f.Path))
			if err != nil || len(data) == 0 {
				t.Fatalf("backed up %s: %v, len=%d", f.Path, err, len(data))
			}
			break
		}
	}
	if !foundLevelDat {
		t.Fatalf("level.dat not in the copy set: %+v", got.Files)
	}

	// World resumed: the engine only succeeds when the server confirmed
	// the resume, and the server must still be live and answering.
	running, err := e.Sup.Running(ctx)
	if err != nil || !running {
		t.Fatalf("server running = %v, %v", running, err)
	}
	out, err := rigConsole(t).Send(ctx, "list")
	if err != nil || !strings.Contains(out, "players online") {
		t.Fatalf("server not answering after backup: %v / %s", err, out)
	}
}

// TestIntegration_Recovery_InterruptedSave simulates an engine crash while
// the world is still held: puts the real server into hold, plants the
// post-mortem state (stale lock + trash), then verifies CheckRecovery
// reports it and FinishRecovery resumes the world, cleans the trash, and
// clears the lock.
func TestIntegration_Recovery_InterruptedSave(t *testing.T) {
	skipIfRigDown(t)
	backupDir := t.TempDir()
	e := rigEngine(t, backupDir)
	c := rigConsole(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Never leave the rig world held if this test aborts.
	defer func() {
		if out, err := c.Send(context.Background(), "save resume"); err != nil {
			t.Logf("cleanup save resume: %v (out: %s)", err, out)
		} else {
			t.Logf("cleanup resume: %s", strings.TrimSpace(out))
		}
	}()

	// Put the world into hold, as an interrupted live op would have.
	out, err := c.Send(ctx, "save hold")
	if err != nil {
		t.Fatalf("save hold: %v (partial: %s)", err, out)
	}
	t.Logf("hold: %s", strings.TrimSpace(out))

	// Plant the post-mortem state of a crashed engine.
	writeLock(t, backupDir, 999999999, ModeLive, "copy")
	if err := os.MkdirAll(filepath.Join(backupDir, "19990101-000000"), 0o755); err != nil {
		t.Fatal(err)
	}

	st, err := e.CheckRecovery()
	if err != nil {
		t.Fatalf("CheckRecovery: %v", err)
	}
	if !st.LockPresent || !st.Stale {
		t.Fatalf("status = %+v, want a stale lock reported", st)
	}
	if len(st.TrashDirs) != 1 {
		t.Fatalf("trash = %v, want the planted dir", st.TrashDirs)
	}

	res, err := e.FinishRecovery(ctx)
	if err != nil {
		t.Fatalf("FinishRecovery: %v", err)
	}
	if !res.Resumed {
		t.Fatalf("result = %+v: the real server must confirm the resume", res)
	}
	if !res.LockCleared || len(res.Cleaned) != 1 {
		t.Fatalf("result = %+v, want lock cleared and trash cleaned", res)
	}

	// The world must be usable again.
	out, err = c.Send(ctx, "list")
	if err != nil || !strings.Contains(out, "players online") {
		t.Fatalf("server not answering after recovery: %v / %s", err, out)
	}
}

// TestIntegration_Restore_RoundTrip restores a fresh live backup into the
// rig, verifies the post-backup mutation is gone, then undoes via the
// safety net and verifies the pre-restore state is back.
func TestIntegration_Restore_RoundTrip(t *testing.T) {
	skipIfRigDown(t)
	e := rigEngine(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	b, err := e.LiveBackup(ctx)
	if err != nil {
		t.Fatalf("LiveBackup: %v", err)
	}
	world, err := worldName(e.ServerDir)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(e.ServerDir, "worlds", world, "compass-integ-marker.txt")
	defer func() { os.Remove(marker) }() // test litter: keep the rig clean

	// Mutate the live world after the backup: absent after the restore,
	// captured by the safety net, back after the undo.
	if err := os.WriteFile(marker, []byte("AFTER"), 0o644); err != nil {
		t.Fatalf("plant marker: %v", err)
	}

	res, err := e.Restore(ctx, b.ID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("marker must be gone after restore (the backup predates it)")
	}
	if res.SafetyNet == "" {
		t.Fatal("no safety-net id reported")
	}

	if _, err := e.Restore(ctx, res.SafetyNet); err != nil {
		t.Fatalf("undo Restore(safety-net): %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "AFTER" {
		t.Fatalf("after undo marker = %q, %v (the safety net must restore it)", data, err)
	}
}
