package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// fakeBackupsHost records the shell calls the screen makes.
type fakeBackupsHost struct {
	asks    []string // "title" per Ask
	toasts  []string // "msg" per Toast
	lastRun func() tea.Cmd
}

func (f *fakeBackupsHost) Ask(title, consequence string, tier Tier, target string, onConfirm func() tea.Cmd) {
	f.asks = append(f.asks, title)
	f.lastRun = onConfirm
}

func (f *fakeBackupsHost) Toast(msg string, kind ToastKind) {
	f.toasts = append(f.toasts, msg)
}

// plantBackup writes a minimal valid manifest into backupDir/id so the
// engine's List/Inspect treat it as a complete backup.
func plantBackup(t *testing.T, backupDir, id string, created time.Time) {
	t.Helper()
	dir := filepath.Join(backupDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b := compass.Backup{
		ID:            id,
		Created:       created,
		ServerVersion: "test",
		Mode:          compass.ModeLive,
		World:         "world",
		Files:         []compass.File{{Path: "worlds/world/level.dat", Size: 100}},
		TotalSize:     100,
	}
	data, err := json.MarshalIndent(&b, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newTestBackups(t *testing.T, host *fakeBackupsHost, eng *compass.Engine, tmr *compass.Timer) *Backups {
	t.Helper()
	return NewBackups(host, &compass.Stack{Engine: eng, Timer: tmr})
}

func testEngine(t *testing.T, retention int) *compass.Engine {
	t.Helper()
	return &compass.Engine{
		ServerDir: t.TempDir(),
		BackupDir: t.TempDir(),
		Retention: retention,
	}
}

func TestBackupsList(t *testing.T) {
	host := &fakeBackupsHost{}
	eng := testEngine(t, 7)
	plantBackup(t, eng.BackupDir, "20260101-000000", time.Now().UTC().Add(-3*time.Hour))
	plantBackup(t, eng.BackupDir, "20260101-010000", time.Now().UTC().Add(-2*time.Hour))
	plantBackup(t, eng.BackupDir, "20260101-020000", time.Now().UTC().Add(-1*time.Hour))

	b := newTestBackups(t, host, eng, nil)
	if len(b.backups) != 3 {
		t.Fatalf("list = %d, want 3", len(b.backups))
	}
	// Newest first.
	if b.backups[0].ID != "20260101-020000" {
		t.Errorf("newest first = %q, want 20260101-020000", b.backups[0].ID)
	}
	view := b.View()
	if !strings.Contains(view, "20260101-020000") || !strings.Contains(view, "live") {
		t.Errorf("view missing backup row:\n%s", view)
	}
	if !strings.Contains(view, ">") {
		t.Errorf("view missing cursor marker:\n%s", view)
	}
}

func TestBackupsCursorMovement(t *testing.T) {
	host := &fakeBackupsHost{}
	eng := testEngine(t, 7)
	plantBackup(t, eng.BackupDir, "20260101-000000", time.Now().UTC().Add(-2*time.Hour))
	plantBackup(t, eng.BackupDir, "20260101-010000", time.Now().UTC().Add(-1*time.Hour))

	b := newTestBackups(t, host, eng, nil)
	if b.cursor != 0 {
		t.Fatalf("initial cursor = %d, want 0", b.cursor)
	}
	b.Update(tea.KeyMsg{Type: tea.KeyDown})
	if b.cursor != 1 {
		t.Errorf("after down: cursor = %d, want 1", b.cursor)
	}
	b.Update(tea.KeyMsg{Type: tea.KeyDown}) // already at last
	if b.cursor != 1 {
		t.Errorf("past end: cursor = %d, want 1 (clamped)", b.cursor)
	}
	b.Update(tea.KeyMsg{Type: tea.KeyUp})
	if b.cursor != 0 {
		t.Errorf("after up: cursor = %d, want 0", b.cursor)
	}
}

func TestBackupsPrunePreview(t *testing.T) {
	host := &fakeBackupsHost{}
	eng := testEngine(t, 2) // keep 2
	now := time.Now().UTC()
	plantBackup(t, eng.BackupDir, "20260101-000000", now.Add(-5*time.Hour))
	plantBackup(t, eng.BackupDir, "20260101-010000", now.Add(-4*time.Hour))
	plantBackup(t, eng.BackupDir, "20260101-020000", now.Add(-3*time.Hour))
	plantBackup(t, eng.BackupDir, "20260101-030000", now.Add(-2*time.Hour))
	plantBackup(t, eng.BackupDir, "20260101-040000", now.Add(-1*time.Hour))
	// One incomplete (manifest-less) dir.
	if err := os.MkdirAll(filepath.Join(eng.BackupDir, "20260101-050000"), 0o755); err != nil {
		t.Fatal(err)
	}

	b := newTestBackups(t, host, eng, nil)
	removable, trash, total := b.prunePreview()
	if len(removable) != 3 {
		t.Errorf("removable = %v, want 3 (5 - retention 2)", removable)
	}
	if len(trash) != 1 {
		t.Errorf("trash = %v, want 1", trash)
	}
	if total != 300 {
		t.Errorf("total = %d, want 300 (3 x 100)", total)
	}
}

func TestBackupsPruneNothing(t *testing.T) {
	host := &fakeBackupsHost{}
	eng := testEngine(t, 7)
	plantBackup(t, eng.BackupDir, "20260101-000000", time.Now().UTC())

	b := newTestBackups(t, host, eng, nil)
	before := len(host.toasts)
	b.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if len(host.toasts) != before+1 {
		t.Fatalf("expected a 'nothing to prune' toast, got %v", host.toasts)
	}
	if !strings.Contains(host.toasts[len(host.toasts)-1], "nothing to prune") {
		t.Errorf("toast = %q, want 'nothing to prune'", host.toasts[len(host.toasts)-1])
	}
	if len(host.asks) != 0 {
		t.Errorf("no Ask expected when nothing to prune, got %v", host.asks)
	}
}

func TestBackupsRecoveryBanner(t *testing.T) {
	host := &fakeBackupsHost{}
	eng := testEngine(t, 7)
	// A stale lock: holder pid 99999 is almost certainly dead.
	lock := `{"pid":99999,"op":"live","phase":"copy","acquired":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(eng.BackupDir, ".backup.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}

	b := newTestBackups(t, host, eng, nil)
	if b.recovery == nil || !b.recovery.LockPresent || !b.recovery.Stale {
		t.Fatalf("recovery = %+v, want stale lock present", b.recovery)
	}
	view := b.View()
	if !strings.Contains(view, "recover") {
		t.Errorf("view missing recovery banner:\n%s", view)
	}
}

func TestBackupsRecoverAction(t *testing.T) {
	host := &fakeBackupsHost{}
	eng := testEngine(t, 7)
	lock := `{"pid":99999,"op":"stopped","phase":"copy","acquired":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(eng.BackupDir, ".backup.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	b := newTestBackups(t, host, eng, nil)

	b.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if len(host.asks) != 1 {
		t.Fatalf("asks = %v, want 1", host.asks)
	}
	if host.lastRun == nil {
		t.Fatal("no confirm command recorded")
	}
	msg := host.lastRun()()
	r, ok := msg.(opResultMsg)
	if !ok {
		t.Fatalf("msg = %#v, want opResultMsg", msg)
	}
	if r.err != "" {
		t.Errorf("recover err = %q", r.err)
	}
	if !strings.Contains(r.detail, "lock cleared") {
		t.Errorf("detail = %q, want 'lock cleared'", r.detail)
	}
	if _, serr := os.Stat(filepath.Join(eng.BackupDir, ".backup.lock")); !os.IsNotExist(serr) {
		t.Errorf("lock should be cleared after recovery")
	}
}

func TestBackupsTimerFormValidation(t *testing.T) {
	host := &fakeBackupsHost{}
	eng := testEngine(t, 7)
	b := newTestBackups(t, host, eng, nil)

	b.enterTimer()
	if !b.timerMode {
		t.Fatal("timer form not entered")
	}
	// Invalid time -> toast, no Ask.
	b.timeBuf = "99:99"
	before := len(host.asks)
	b.confirmTimer()
	if len(host.asks) != before {
		t.Errorf("invalid schedule must not Ask, got %v", host.asks)
	}
	if len(host.toasts) == 0 || !strings.Contains(host.toasts[len(host.toasts)-1], "timer") {
		t.Errorf("expected a validation toast, got %v", host.toasts)
	}
	// Esc leaves the form.
	b.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if b.timerMode {
		t.Error("Esc should leave timer form")
	}
}

func TestBackupsTimerCreate(t *testing.T) {
	host := &fakeBackupsHost{}
	eng := testEngine(t, 7)
	// Timer with a recording exec (same fake pattern as the core tests).
	rec := &recExecT{outputs: map[string][]byte{
		"is-loaded":   []byte("loaded\n"),
		"is-enabled":  []byte("enabled\n"),
		"is-active":   []byte("active\n"),
		"list-timers": []byte("Mon 2026-01-05 02:00:00 UTC  1h  Mon 2026-01-01 00:00:00 UTC  test-backup.timer  -  -  active\n"),
	}}
	tmr := &compass.Timer{Unit: "test-backup", Config: "/nonexistent/cfg", BinaryPath: "/bin/true", Exec: rec.run, ConfigDir: t.TempDir()}

	b := newTestBackups(t, host, eng, tmr)
	b.enterTimer()
	b.cadence = "daily"
	b.timeBuf = "02:00"
	b.mode = compass.ModeLive

	b.confirmTimer()
	if len(host.asks) != 1 {
		t.Fatalf("asks = %v, want 1", host.asks)
	}
	if host.lastRun == nil {
		t.Fatal("no confirm command")
	}
	msg := host.lastRun()()
	r, ok := msg.(opResultMsg)
	if !ok {
		t.Fatalf("msg = %#v, want opResultMsg", msg)
	}
	if r.err != "" {
		t.Fatalf("create err = %q (calls: %v)", r.err, rec.calls)
	}
	if !strings.Contains(r.detail, "timer on") {
		t.Errorf("detail = %q, want 'timer on'", r.detail)
	}
	// The service unit was written.
	if len(rec.calls) < 3 {
		t.Errorf("expected enable + is-active + next-elapse, got %v", rec.calls)
	}
}

func TestBackupsWorkingBlocksKeys(t *testing.T) {
	host := &fakeBackupsHost{}
	eng := testEngine(t, 7)
	b := newTestBackups(t, host, eng, nil)
	b.working = true
	before := len(host.asks)
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	if len(host.asks) != before {
		t.Error("keys must be ignored while an operation is in flight")
	}
	// Resolve the operation.
	b.Update(opResultMsg{label: "test", detail: "ok"})
	if b.working {
		t.Error("working should clear after result")
	}
}

func TestBackupsNilStack(t *testing.T) {
	host := &fakeBackupsHost{}
	b := NewBackups(host, nil)
	b.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	if len(host.toasts) == 0 || !strings.Contains(host.toasts[0], "unavailable") {
		t.Errorf("nil stack: expected 'unavailable' toast, got %v", host.toasts)
	}
	view := b.View()
	if !strings.Contains(view, "Backups") {
		t.Errorf("degraded view missing title:\n%s", view)
	}
}

// recExecT mirrors the core's recording exec fake (separate type so the TUI
// test package stays self-contained).
type recExecT struct {
	calls   [][]string
	outputs map[string][]byte
}

func (r *recExecT) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	key := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			key = a
			break
		}
	}
	if out, ok := r.outputs[key]; ok {
		return out, nil
	}
	return []byte{}, nil
}
