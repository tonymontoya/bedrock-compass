package compass

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func manifestJSON(id, mode string) string {
	return `{"id":"` + id + `","created":"2026-08-16T10:00:00Z","server-version":"1.26.43","mode":"` + mode + `","world":"TestWorld","config":["server.properties","allowlist.json"],"files":[{"path":"worlds/TestWorld/level.dat","size":100},{"path":"server.properties","size":50}],"total-size":150}`
}

func writeFixtureBackup(t *testing.T, backupDir, id, manifest string) {
	t.Helper()
	dir := filepath.Join(backupDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEngine_List_NewestFirst_CompleteOnly(t *testing.T) {
	backupDir := t.TempDir()
	e := &Engine{BackupDir: backupDir, Retention: 7}

	writeFixtureBackup(t, backupDir, "20260815-100000", manifestJSON("20260815-100000", ModeLive))
	writeFixtureBackup(t, backupDir, "20260816-100000", manifestJSON("20260816-100000", ModeStopped))
	// a manifest-less dir is an incomplete backup: excluded from List
	if err := os.MkdirAll(filepath.Join(backupDir, "20260816-110000"), 0o755); err != nil {
		t.Fatal(err)
	}

	list, err := e.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List len = %d, want 2 (manifest-less dir excluded)", len(list))
	}
	if list[0].ID != "20260816-100000" || list[1].ID != "20260815-100000" {
		t.Fatalf("order = [%s %s], want newest first", list[0].ID, list[1].ID)
	}
}

func TestEngine_Inspect_ParsesManifest(t *testing.T) {
	backupDir := t.TempDir()
	e := &Engine{BackupDir: backupDir, Retention: 7}
	writeFixtureBackup(t, backupDir, "20260816-100000", manifestJSON("20260816-100000", ModeStopped))

	b, err := e.Inspect("20260816-100000")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if b.ID != "20260816-100000" || b.ServerVersion != "1.26.43" || b.Mode != ModeStopped {
		t.Errorf("identity = %+v", b)
	}
	if b.World != "TestWorld" {
		t.Errorf("World = %q, want TestWorld", b.World)
	}
	if len(b.Config) != 2 || b.Config[0] != "server.properties" {
		t.Errorf("Config = %v", b.Config)
	}
	if len(b.Files) != 2 || b.Files[0].Path != "worlds/TestWorld/level.dat" || b.Files[0].Size != 100 {
		t.Errorf("Files = %+v", b.Files)
	}
	if b.TotalSize != 150 {
		t.Errorf("TotalSize = %d, want 150", b.TotalSize)
	}
	want, perr := time.Parse(time.RFC3339, "2026-08-16T10:00:00Z")
	if perr != nil {
		t.Fatal(perr)
	}
	if !b.Created.Equal(want) {
		t.Errorf("Created = %v", b.Created)
	}
}

func TestEngine_Inspect_UnknownID_Errors(t *testing.T) {
	e := &Engine{BackupDir: t.TempDir(), Retention: 7}
	if _, err := e.Inspect("20260801-000000"); err == nil {
		t.Fatal("Inspect of a missing id must error")
	}
}

func TestEngine_Inspect_RejectsPathEscape(t *testing.T) {
	e := &Engine{BackupDir: t.TempDir(), Retention: 7}
	for _, id := range []string{"..", "a/b", "../x", "a/../b", "/etc"} {
		if _, err := e.Inspect(id); err == nil {
			t.Errorf("Inspect(%q) must reject path escapes", id)
		}
	}
}

func TestEngine_List_MissingBackupDir_IsEmpty(t *testing.T) {
	e := &Engine{BackupDir: filepath.Join(t.TempDir(), "nope"), Retention: 7}
	list, err := e.List()
	if err != nil {
		t.Fatalf("List on a missing dir must not error: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List = %v, want empty", list)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	propsContent  = "level-name=TestWorld\nmax-players=5\n"
	allowContent  = "[]"
	levelDatBytes = "LEVEL"
	dbFileBytes   = "DBDB"
)

func newFixtureServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "server.properties"), propsContent)
	mustWrite(t, filepath.Join(dir, "allowlist.json"), allowContent)
	mustWrite(t, filepath.Join(dir, "worlds/TestWorld/level.dat"), levelDatBytes)
	mustWrite(t, filepath.Join(dir, "worlds/TestWorld/db/000001.ldb"), dbFileBytes)
	return dir
}

func TestEngine_LiveBackup_HappyPath(t *testing.T) {
	serverDir := newFixtureServer(t)
	backupDir := t.TempDir()
	c, sent := newScriptedConsole(t, map[string]string{
		"save hold": "Saving...",
		"save query": "Data saved. Files are now ready to be copied. " +
			"TestWorld/level.dat: " + itoa(len(levelDatBytes)) + ", TestWorld/db/000001.ldb: " + itoa(len(dbFileBytes)),
		"save resume": "Changes to the level are resumed.",
	})
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{
		ServerDir: serverDir,
		BackupDir: backupDir,
		Retention: 7,
		Version:   "1.26.43",
		Console:   c,
		Trail:     trail,
	}

	b, err := e.LiveBackup(context.Background())
	if err != nil {
		t.Fatalf("LiveBackup: %v", err)
	}

	want := []string{"save hold", "save query", "save resume"}
	if len(*sent) != len(want) {
		t.Fatalf("console commands = %v, want %v", *sent, want)
	}
	for i := range want {
		if (*sent)[i] != want[i] {
			t.Fatalf("console commands = %v, want %v", *sent, want)
		}
	}

	if b.Mode != ModeLive || b.World != "TestWorld" || b.ServerVersion != "1.26.43" {
		t.Errorf("identity = %+v", b)
	}
	if !regexp.MustCompile(`^\d{8}-\d{6}$`).MatchString(b.ID) {
		t.Errorf("ID = %q, want YYYYMMDD-HHMMSS", b.ID)
	}

	got := map[string]int64{}
	for _, f := range b.Files {
		got[f.Path] = f.Size
	}
	wantFiles := map[string]int64{
		"worlds/TestWorld/level.dat":     int64(len(levelDatBytes)),
		"worlds/TestWorld/db/000001.ldb": int64(len(dbFileBytes)),
		"server.properties":              int64(len(propsContent)),
		"allowlist.json":                 int64(len(allowContent)),
	}
	if len(got) != len(wantFiles) {
		t.Fatalf("files = %v, want %v", got, wantFiles)
	}
	for k, v := range wantFiles {
		if got[k] != v {
			t.Errorf("file %s size = %d, want %d", k, got[k], v)
		}
	}
	if b.TotalSize != int64(len(levelDatBytes)+len(dbFileBytes)+len(propsContent)+len(allowContent)) {
		t.Errorf("TotalSize = %d", b.TotalSize)
	}

	data, err := os.ReadFile(filepath.Join(backupDir, b.ID, "worlds/TestWorld/level.dat"))
	if err != nil || string(data) != levelDatBytes {
		t.Fatalf("copied level.dat = %q, %v", data, err)
	}
	if _, err := e.Inspect(b.ID); err != nil {
		t.Errorf("Inspect after backup: %v", err)
	}
	list, err := e.List()
	if err != nil || len(list) != 1 || list[0].ID != b.ID {
		t.Errorf("List = %+v, %v; want 1 entry %s", list, err, b.ID)
	}

	events, err := trail.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Verb != "backup" || ev.Result != "ok" {
		t.Errorf("audit = %+v", ev)
	}
	if ev.Params["mode"] != ModeLive || ev.Params["id"] != b.ID {
		t.Errorf("audit params = %v", ev.Params)
	}
	if ev.Params["hold-window"] == "" {
		t.Error("audit missing hold-window")
	}

	if data, _ := os.ReadFile(filepath.Join(serverDir, "worlds/TestWorld/level.dat")); string(data) != levelDatBytes {
		t.Error("source file modified by backup")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestEngine_LiveBackup_CopyFailure_StillResumes(t *testing.T) {
	serverDir := newFixtureServer(t)
	// level.dat exists as an unreadable file: the copy must fail.
	if err := os.Chmod(filepath.Join(serverDir, "worlds/TestWorld/level.dat"), 0); err != nil {
		t.Fatal(err)
	}
	backupDir := t.TempDir()
	c, sent := newScriptedConsole(t, map[string]string{
		"save hold":   "Saving...",
		"save query":  "Data saved. Files are now ready to be copied. TestWorld/level.dat: 5",
		"save resume": "Changes to the level are resumed.",
	})
	e := &Engine{ServerDir: serverDir, BackupDir: backupDir, Retention: 7, Console: c}

	if _, err := e.LiveBackup(context.Background()); err == nil {
		t.Fatal("LiveBackup with an unreadable world file must fail")
	}
	// The always-resume invariant: hold succeeded, so resume must have run.
	last := (*sent)[len(*sent)-1]
	if last != "save resume" {
		t.Errorf("last console command = %q, want save resume (sent %v)", last, *sent)
	}
}

func TestEngine_LiveBackup_PreviousSaveIncomplete_HardError(t *testing.T) {
	c, _ := newScriptedConsole(t, map[string]string{
		"save hold": "A previous save has not been completed.",
	})
	e := &Engine{
		ServerDir: newFixtureServer(t),
		BackupDir: t.TempDir(),
		Retention: 7,
		Console:   c,
	}
	_, err := e.LiveBackup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "previous save") {
		t.Fatalf("err = %v, want a hard previous-save error", err)
	}
}

func TestEngine_LiveBackup_PathEscape_Rejected(t *testing.T) {
	serverDir := newFixtureServer(t)
	backupDir := t.TempDir()
	c, sent := newScriptedConsole(t, map[string]string{
		"save hold":   "Saving...",
		"save query":  "Data saved. Files are now ready to be copied. ../evil: 5",
		"save resume": "Changes to the level are resumed.",
	})
	e := &Engine{ServerDir: serverDir, BackupDir: backupDir, Retention: 7, Console: c}

	_, err := e.LiveBackup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "outside the server dir") {
		t.Fatalf("err = %v, want path-escape rejection", err)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "..", "evil")); err == nil {
		t.Error("escape path must not be copied out")
	}
	if last := (*sent)[len(*sent)-1]; last != "save resume" {
		t.Errorf("last command = %q, want save resume", last)
	}
}

func TestEngine_LiveBackup_HoldUnexpected_NoResume(t *testing.T) {
	c, sent := newScriptedConsole(t, map[string]string{
		"save hold": "I am not a save hold response.",
	})
	e := &Engine{
		ServerDir: newFixtureServer(t),
		BackupDir: t.TempDir(),
		Retention: 7,
		Console:   c,
	}
	_, err := e.LiveBackup(context.Background())
	if err == nil {
		t.Fatal("unexpected hold response must fail")
	}
	if containsLine(sent, "save resume") {
		t.Error("must not resume when our hold never succeeded")
	}
}

func TestEngine_LiveBackup_QueryNotReady_StillResumes(t *testing.T) {
	c, sent := newScriptedConsole(t, map[string]string{
		"save hold":   "Saving...",
		"save query":  "Still saving, wait.",
		"save resume": "Changes to the level are resumed.",
	})
	e := &Engine{
		ServerDir: newFixtureServer(t),
		BackupDir: t.TempDir(),
		Retention: 7,
		Console:   c,
	}
	if _, err := e.LiveBackup(context.Background()); err == nil {
		t.Fatal("not-ready save query must fail")
	}
	if last := (*sent)[len(*sent)-1]; last != "save resume" {
		t.Errorf("last command = %q, want save resume", last)
	}
}

func TestEngine_LiveBackup_ResumeUnexpected_Fails(t *testing.T) {
	c, sent := newScriptedConsole(t, map[string]string{
		"save hold":   "Saving...",
		"save query":  "Data saved. Files are now ready to be copied. TestWorld/level.dat: 5",
		"save resume": "Nope.",
	})
	e := &Engine{
		ServerDir: newFixtureServer(t),
		BackupDir: t.TempDir(),
		Retention: 7,
		Console:   c,
	}
	if _, err := e.LiveBackup(context.Background()); err == nil {
		t.Fatal("unexpected resume response must fail")
	}
	if last := (*sent)[len(*sent)-1]; last != "save resume" {
		t.Errorf("last command = %q, want save resume", last)
	}
}

func TestEngine_LiveBackup_MissingListedFile_NoManifest(t *testing.T) {
	serverDir := newFixtureServer(t)
	backupDir := t.TempDir()
	c, sent := newScriptedConsole(t, map[string]string{
		"save hold":   "Saving...",
		"save query":  "Data saved. Files are now ready to be copied. TestWorld/ghost.dat: 5",
		"save resume": "Changes to the level are resumed.",
	})
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{ServerDir: serverDir, BackupDir: backupDir, Retention: 7, Console: c, Trail: trail}

	b, err := e.LiveBackup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "missing world file(s)") {
		t.Fatalf("err = %v, want missing-file report", err)
	}
	if b == nil {
		t.Fatal("must still return the backup id for cleanup")
	}
	if last := (*sent)[len(*sent)-1]; last != "save resume" {
		t.Errorf("last command = %q, want save resume", last)
	}
	// No manifest: not a valid backup, so List must not show it.
	if list, err := e.List(); err != nil || len(list) != 0 {
		t.Errorf("List = %v, %v; want empty (no manifest)", list, err)
	}
	events, err := trail.Recent(10)
	if err != nil || len(events) != 1 || events[0].Result != "error: missing world file(s): worlds/TestWorld/ghost.dat" {
		t.Errorf("audit = %+v, %v; want a failure record", events, err)
	}
}

func TestEngine_LiveBackup_IncludePacks(t *testing.T) {
	newPackServer := func() string {
		dir := newFixtureServer(t)
		mustWrite(t, filepath.Join(dir, "behavior_packs/pack1/pack_manifest.json"), "{}")
		mustWrite(t, filepath.Join(dir, "resource_packs/pack2/terrain.png"), "PNG")
		return dir
	}
	script := map[string]string{
		"save hold":   "Saving...",
		"save query":  "Data saved. Files are now ready to be copied. TestWorld/level.dat: 5",
		"save resume": "Changes to the level are resumed.",
	}

	c, _ := newScriptedConsole(t, script)
	e := &Engine{ServerDir: newPackServer(), BackupDir: t.TempDir(), Retention: 7, IncludePacks: true, Console: c}
	b, err := e.LiveBackup(context.Background())
	if err != nil {
		t.Fatalf("LiveBackup: %v", err)
	}
	got := map[string]bool{}
	for _, f := range b.Files {
		got[f.Path] = true
	}
	for _, want := range []string{
		"behavior_packs/pack1/pack_manifest.json",
		"resource_packs/pack2/terrain.png",
	} {
		if !got[want] {
			t.Errorf("IncludePacks: files missing %s (have %v)", want, got)
		}
	}
	data, err := os.ReadFile(filepath.Join(e.BackupDir, b.ID, "behavior_packs/pack1/pack_manifest.json"))
	if err != nil || string(data) != "{}" {
		t.Errorf("copied pack manifest = %q, %v", data, err)
	}

	// Off by default: without IncludePacks the packs are not part of the set.
	c2, _ := newScriptedConsole(t, script)
	e2 := &Engine{ServerDir: newPackServer(), BackupDir: t.TempDir(), Retention: 7, Console: c2}
	b2, err := e2.LiveBackup(context.Background())
	if err != nil {
		t.Fatalf("LiveBackup default: %v", err)
	}
	got2 := map[string]bool{}
	for _, f := range b2.Files {
		got2[f.Path] = true
	}
	if got2["behavior_packs/pack1/pack_manifest.json"] {
		t.Error("packs must not be included by default")
	}
}

func newFakeSup(stopErr, startErr error, running bool) (*Supervisor, *[]string) {
	var calls []string
	s := &Supervisor{
		Stop:    func(ctx context.Context) error { calls = append(calls, "stop"); return stopErr },
		Start:   func(ctx context.Context) error { calls = append(calls, "start"); return startErr },
		Running: func(ctx context.Context) (bool, error) { return running, nil },
	}
	return s, &calls
}

func TestEngine_StoppedBackup_HappyPath(t *testing.T) {
	serverDir := newFixtureServer(t)
	mustWrite(t, filepath.Join(serverDir, "worlds/TestWorld/level.dat_old"), "OLD")
	backupDir := t.TempDir()
	sup, calls := newFakeSup(nil, nil, false)
	e := &Engine{ServerDir: serverDir, BackupDir: backupDir, Retention: 7, Sup: sup}

	b, err := e.StoppedBackup(context.Background())
	if err != nil {
		t.Fatalf("StoppedBackup: %v", err)
	}
	if b.Mode != ModeStopped {
		t.Errorf("Mode = %q, want stopped", b.Mode)
	}
	if !reflect.DeepEqual(*calls, []string{"stop", "start"}) {
		t.Errorf("sup calls = %v, want [stop start]", *calls)
	}
	got := map[string]bool{}
	for _, f := range b.Files {
		got[f.Path] = true
	}
	for _, want := range []string{
		"worlds/TestWorld/level.dat",
		"worlds/TestWorld/db/000001.ldb",
		"server.properties",
		"allowlist.json",
	} {
		if !got[want] {
			t.Errorf("files missing %s (have %v)", want, got)
		}
	}
	if got["worlds/TestWorld/level.dat_old"] {
		t.Error("level.dat_old must be excluded")
	}
	if _, err := os.Stat(filepath.Join(backupDir, b.ID, "worlds/TestWorld/level.dat_old")); !os.IsNotExist(err) {
		t.Error("level.dat_old must not be copied")
	}
	if _, err := e.Inspect(b.ID); err != nil {
		t.Errorf("Inspect after backup: %v", err)
	}
}

func TestEngine_StoppedBackup_StartFails_BackupStillComplete(t *testing.T) {
	sup, calls := newFakeSup(nil, errors.New("systemctl start: unit not found"), false)
	e := &Engine{ServerDir: newFixtureServer(t), BackupDir: t.TempDir(), Retention: 7, Sup: sup}

	b, err := e.StoppedBackup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "start failed") {
		t.Fatalf("err = %v, want a loud start failure", err)
	}
	if b == nil {
		t.Fatal("the completed backup must still be returned")
	}
	if _, err := e.Inspect(b.ID); err != nil {
		t.Errorf("backup must be complete despite start failure: %v", err)
	}
	if list, err := e.List(); err != nil || len(list) != 1 {
		t.Errorf("List = %v, %v; want the completed backup", list, err)
	}
	if !reflect.DeepEqual(*calls, []string{"stop", "start"}) {
		t.Errorf("sup calls = %v, want exactly [stop start] (no double retry)", *calls)
	}
}

func TestEngine_StoppedBackup_CopyFailure_RestartsServer(t *testing.T) {
	serverDir := newFixtureServer(t)
	if err := os.Chmod(filepath.Join(serverDir, "worlds/TestWorld/level.dat"), 0); err != nil {
		t.Fatal(err)
	}
	sup, calls := newFakeSup(nil, nil, false)
	e := &Engine{ServerDir: serverDir, BackupDir: t.TempDir(), Retention: 7, Sup: sup}

	if _, err := e.StoppedBackup(context.Background()); err == nil {
		t.Fatal("unreadable world file must fail the backup")
	}
	if last := (*calls)[len(*calls)-1]; last != "start" {
		t.Errorf("sup calls = %v, server must be restarted after a failed backup", *calls)
	}
}

func TestEngine_StoppedBackup_StillRunningAfterStop_Fails(t *testing.T) {
	sup, calls := newFakeSup(nil, nil, true) // Running stays true: stop did not take effect
	e := &Engine{ServerDir: newFixtureServer(t), BackupDir: t.TempDir(), Retention: 7, Sup: sup}

	if _, err := e.StoppedBackup(context.Background()); err == nil {
		t.Fatal("a still-running server must fail the backup")
	}
	if last := (*calls)[len(*calls)-1]; last != "start" {
		t.Errorf("sup calls = %v, server must be restarted", *calls)
	}
}

func fixtureManifest(id, mode, created string) string {
	return `{"id":"` + id + `","created":"` + created + `","server-version":"1.26.43","mode":"` + mode + `","world":"TestWorld","config":[],"files":[],"total-size":0}`
}

func TestEngine_Prune_KeepsNewestN(t *testing.T) {
	backupDir := t.TempDir()
	e := &Engine{BackupDir: backupDir, Retention: 3}
	old := []struct{ id, created string }{
		{"20260810-000000", "2026-08-10T00:00:00Z"},
		{"20260811-000000", "2026-08-11T00:00:00Z"},
		{"20260812-000000", "2026-08-12T00:00:00Z"},
		{"20260813-000000", "2026-08-13T00:00:00Z"},
		{"20260814-000000", "2026-08-14T00:00:00Z"},
	}
	for _, o := range old {
		writeFixtureBackup(t, backupDir, o.id, fixtureManifest(o.id, ModeLive, o.created))
	}

	n, err := e.Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 2 {
		t.Errorf("pruned = %d, want 2", n)
	}
	list, err := e.List()
	if err != nil || len(list) != 3 {
		t.Fatalf("List = %v, %v; want 3 kept", list, err)
	}
	if list[0].ID != "20260814-000000" {
		t.Errorf("newest kept = %s, want 20260814-000000", list[0].ID)
	}
}

func TestEngine_Prune_RemovesTrash(t *testing.T) {
	backupDir := t.TempDir()
	e := &Engine{BackupDir: backupDir, Retention: 7}
	writeFixtureBackup(t, backupDir, "20260816-100000", manifestJSON("20260816-100000", ModeLive))
	if err := os.MkdirAll(filepath.Join(backupDir, "20260816-110000"), 0o755); err != nil {
		t.Fatal(err)
	}

	n, err := e.Prune()
	if err != nil || n != 1 {
		t.Fatalf("pruned = %d, err = %v; want the trash removed", n, err)
	}
	list, err := e.List()
	if err != nil || len(list) != 1 {
		t.Errorf("List = %v, %v; want the good backup kept", list, err)
	}
}

func TestEngine_Prune_SafetyNetsCount(t *testing.T) {
	backupDir := t.TempDir()
	e := &Engine{BackupDir: backupDir, Retention: 1}
	writeFixtureBackup(t, backupDir, "20260815-000000", fixtureManifest("20260815-000000", ModeLive, "2026-08-15T00:00:00Z"))
	writeFixtureBackup(t, backupDir, "20260816-000000", fixtureManifest("20260816-000000", ModeSafetyNet, "2026-08-16T00:00:00Z"))

	n, err := e.Prune()
	if err != nil || n != 1 {
		t.Fatalf("pruned = %d, err = %v; want 1", n, err)
	}
	list, err := e.List()
	if err != nil || len(list) != 1 || list[0].Mode != ModeSafetyNet {
		t.Errorf("List = %+v, %v; want only the safety-net kept", list, err)
	}
}

func TestEngine_Prune_MissingDir_NoError(t *testing.T) {
	e := &Engine{BackupDir: filepath.Join(t.TempDir(), "nope"), Retention: 7}
	n, err := e.Prune()
	if err != nil || n != 0 {
		t.Fatalf("pruned = %d, err = %v; want 0, nil", n, err)
	}
}

func TestEngine_LiveBackup_PrunesAfterSuccess(t *testing.T) {
	serverDir := newFixtureServer(t)
	backupDir := t.TempDir()
	for _, o := range []struct{ id, created string }{
		{"20260810-000000", "2026-08-10T00:00:00Z"},
		{"20260811-000000", "2026-08-11T00:00:00Z"},
		{"20260812-000000", "2026-08-12T00:00:00Z"},
		{"20260813-000000", "2026-08-13T00:00:00Z"},
		{"20260814-000000", "2026-08-14T00:00:00Z"},
		{"20260815-000000", "2026-08-15T00:00:00Z"},
		{"20260816-000000", "2026-08-16T00:00:00Z"},
	} {
		writeFixtureBackup(t, backupDir, o.id, fixtureManifest(o.id, ModeLive, o.created))
	}
	c, _ := newScriptedConsole(t, map[string]string{
		"save hold":   "Saving...",
		"save query":  "Data saved. Files are now ready to be copied. TestWorld/level.dat: 5",
		"save resume": "Changes to the level are resumed.",
	})
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{ServerDir: serverDir, BackupDir: backupDir, Retention: 7, Console: c, Trail: trail}

	b, err := e.LiveBackup(context.Background())
	if err != nil {
		t.Fatalf("LiveBackup: %v", err)
	}
	list, err := e.List()
	if err != nil || len(list) != 7 {
		t.Fatalf("List = %v, %v; want 7 (newest kept, oldest pruned)", list, err)
	}
	if list[0].ID != b.ID {
		t.Errorf("newest = %s, want %s", list[0].ID, b.ID)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "20260810-000000")); !os.IsNotExist(err) {
		t.Error("oldest backup must be pruned")
	}
	events, err := trail.Recent(10)
	if err != nil || len(events) != 1 {
		t.Fatalf("audit = %+v, %v", events, err)
	}
	if events[0].Params["pruned"] != "1" {
		t.Errorf("audit pruned = %q, want 1", events[0].Params["pruned"])
	}
}

// TestCopyFile_PreservesFileMode pins the cross-user restore requirement:
// a copied file must keep its permission bits (e.g. group-write) so a server
// running under a different uid can still write it after a restore.
func TestCopyFile_PreservesFileMode(t *testing.T) {
	srcRoot := t.TempDir()
	srcFile := filepath.Join(srcRoot, "worlds", "TestWorld", "db", "000001.ldb")
	if err := os.MkdirAll(filepath.Dir(srcFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcFile, []byte("DBDB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(srcFile, 0o664); err != nil { // group rw
		t.Fatal(err)
	}

	dstFile := filepath.Join(t.TempDir(), "worlds", "TestWorld", "db", "000001.ldb")
	if _, err := copyFile(srcFile, dstFile); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	fi, err := os.Stat(dstFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o664 {
		t.Errorf("copied file mode = %o, want 0664 (group-write preserved)", got)
	}
}

// TestMirrorTreeModes pins that a copied directory tree keeps its permission
// bits (group-write and setgid), which is what lets a server under a
// different uid create and write files in the restored world.
func TestMirrorTreeModes(t *testing.T) {
	srcRoot := t.TempDir()
	world := filepath.Join(srcRoot, "worlds", "TestWorld", "db")
	if err := os.MkdirAll(world, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{
		filepath.Join(srcRoot, "worlds"),
		filepath.Join(srcRoot, "worlds", "TestWorld"),
		world,
	} {
		// 0o0775|os.ModeSetgid is the form Go's os.Chmod honors (a raw
		// 0o2775 drops the setgid bit).
		if err := os.Chmod(d, 0o0775|os.ModeSetgid); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(world, "000001.ldb"), []byte("DB"), 0o644); err != nil {
		t.Fatal(err)
	}

	dstRoot := t.TempDir()
	dstWorld := filepath.Join(dstRoot, "worlds", "TestWorld", "db")
	if err := os.MkdirAll(dstWorld, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dstWorld, "000001.ldb"), []byte("DB"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := mirrorTreeModes(srcRoot, dstRoot); err != nil {
		t.Fatalf("mirrorTreeModes: %v", err)
	}
	di, err := os.Stat(dstWorld)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode()&os.ModeSetgid == 0 {
		t.Errorf("copied dir missing setgid: %v", di.Mode())
	}
	if di.Mode().Perm()&0o020 == 0 {
		t.Errorf("copied dir group lacks write: %o", di.Mode().Perm())
	}
}
