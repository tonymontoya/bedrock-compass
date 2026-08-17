package compass

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeRestoreFixture builds a live server (level.dat = "LEVEL") and a
// backup whose level.dat is "BACKUPDATA".
func writeRestoreFixture(t *testing.T) (serverDir, backupDir, id string) {
	t.Helper()
	serverDir = newFixtureServer(t)
	backupDir = t.TempDir()
	id = "20260810-000000"
	bdir := filepath.Join(backupDir, id)
	mustWrite(t, filepath.Join(bdir, "worlds/TestWorld/level.dat"), "BACKUPDATA")
	mustWrite(t, filepath.Join(bdir, "server.properties"), propsContent)
	mustWrite(t, filepath.Join(bdir, "allowlist.json"), allowContent)
	manifest := `{"id":"` + id + `","created":"2026-08-10T00:00:00Z","server-version":"1.26.43","mode":"` + ModeLive +
		`","world":"TestWorld","config":["server.properties","allowlist.json"],"files":[` +
		`{"path":"worlds/TestWorld/level.dat","size":10},` +
		`{"path":"server.properties","size":` + itoa(len(propsContent)) + `},` +
		`{"path":"allowlist.json","size":2}` +
		`],"total-size":` + itoa(10+len(propsContent)+2) + `}`
	if err := os.WriteFile(filepath.Join(bdir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return serverDir, backupDir, id
}

func TestEngine_Restore_HappyPath(t *testing.T) {
	serverDir, backupDir, id := writeRestoreFixture(t)
	mustWrite(t, filepath.Join(serverDir, "worlds/TestWorld/level.dat_old"), "OLD")
	sup, calls := newFakeSup(nil, nil, false)
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{ServerDir: serverDir, BackupDir: backupDir, Retention: 7, Sup: sup, Trail: trail}

	res, err := e.Restore(context.Background(), id)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.SafetyNet == "" {
		t.Fatal("must report the safety-net id")
	}
	data, err := os.ReadFile(filepath.Join(serverDir, "worlds/TestWorld/level.dat"))
	if err != nil || string(data) != "BACKUPDATA" {
		t.Fatalf("restored level.dat = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(serverDir, "worlds/TestWorld/level.dat_old")); !os.IsNotExist(err) {
		t.Error("level.dat_old must be deleted")
	}
	snet, err := e.Inspect(res.SafetyNet)
	if err != nil {
		t.Fatalf("safety-net Inspect: %v", err)
	}
	if snet.Mode != ModeSafetyNet {
		t.Errorf("safety-net mode = %q, want safety-net", snet.Mode)
	}
	sdata, err := os.ReadFile(filepath.Join(backupDir, res.SafetyNet, "worlds/TestWorld/level.dat"))
	if err != nil || string(sdata) != "LEVEL" {
		t.Errorf("safety-net captured level.dat = %q, %v (want the pre-restore data)", sdata, err)
	}
	if !reflect.DeepEqual(*calls, []string{"stop", "start"}) {
		t.Errorf("sup calls = %v, want [stop start]", *calls)
	}
	events, err := trail.Recent(10)
	if err != nil || len(events) != 1 {
		t.Fatalf("audit = %+v, %v", events, err)
	}
	if events[0].Verb != "restore" {
		t.Errorf("verb = %q, want restore", events[0].Verb)
	}
	if events[0].Params["id"] != id || events[0].Params["safety-net-id"] != res.SafetyNet {
		t.Errorf("audit params = %v", events[0].Params)
	}
}

func TestEngine_Restore_StartFails_SafetyNetOffered(t *testing.T) {
	serverDir, backupDir, id := writeRestoreFixture(t)
	sup, calls := newFakeSup(nil, errors.New("systemctl start: unit broken"), false)
	e := &Engine{ServerDir: serverDir, BackupDir: backupDir, Retention: 7, Sup: sup}

	res, err := e.Restore(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), "start failed") {
		t.Fatalf("err = %v, want a loud start failure", err)
	}
	if res == nil || res.SafetyNet == "" {
		t.Fatal("must report the safety net so the operator can undo")
	}
	if last := (*calls)[len(*calls)-1]; last != "start" {
		t.Errorf("sup calls = %v, want the start attempt last", *calls)
	}
}

func TestEngine_Restore_UndoRoundTrip(t *testing.T) {
	serverDir, backupDir, id := writeRestoreFixture(t)
	sup, _ := newFakeSup(nil, nil, false)
	e := &Engine{ServerDir: serverDir, BackupDir: backupDir, Retention: 7, Sup: sup}

	res, err := e.Restore(context.Background(), id)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := e.Restore(context.Background(), res.SafetyNet); err != nil {
		t.Fatalf("undo Restore(safety-net): %v", err)
	}
	data, err := os.ReadFile(filepath.Join(serverDir, "worlds/TestWorld/level.dat"))
	if err != nil || string(data) != "LEVEL" {
		t.Fatalf("after undo level.dat = %q, %v (want the original data back)", data, err)
	}
}

func TestEngine_Restore_UnknownID_NeverTouchesServer(t *testing.T) {
	sup, calls := newFakeSup(nil, nil, false)
	e := &Engine{ServerDir: newFixtureServer(t), BackupDir: t.TempDir(), Retention: 7, Sup: sup}

	if _, err := e.Restore(context.Background(), "nope"); err == nil {
		t.Fatal("unknown id must fail")
	}
	if len(*calls) != 0 {
		t.Errorf("sup calls = %v, want none (verify the manifest before touching the server)", *calls)
	}
}

func TestEngine_Restore_CopyFailure_RestartsServer(t *testing.T) {
	serverDir, backupDir, id := writeRestoreFixture(t)
	if err := os.Chmod(filepath.Join(backupDir, id, "worlds/TestWorld/level.dat"), 0); err != nil {
		t.Fatal(err)
	}
	sup, calls := newFakeSup(nil, nil, false)
	e := &Engine{ServerDir: serverDir, BackupDir: backupDir, Retention: 7, Sup: sup}

	if _, err := e.Restore(context.Background(), id); err == nil {
		t.Fatal("unreadable backup file must fail the restore")
	}
	if last := (*calls)[len(*calls)-1]; last != "start" {
		t.Errorf("sup calls = %v, want the server restarted", *calls)
	}
}
