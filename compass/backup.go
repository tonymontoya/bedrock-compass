package compass

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Modes of a backup (manifest "mode" field).
const (
	ModeLive      = "live"       // taken through the save protocol while running
	ModeStopped   = "stopped"    // taken with the server stopped
	ModeSafetyNet = "safety-net" // taken automatically before a restore
)

// Backup describes one complete backup: exactly what manifest.json holds.
// The manifest is written last and atomically, so a manifest marks a valid
// backup; dirs without one are incomplete (ADR-0006).
type Backup struct {
	ID            string    `json:"id"`
	Created       time.Time `json:"created"`
	ServerVersion string    `json:"server-version"`
	Mode          string    `json:"mode"`
	World         string    `json:"world"`
	Config        []string  `json:"config,omitempty"`
	Files         []File    `json:"files"`
	TotalSize     int64     `json:"total-size"`
}

// File is one file in a backup. Path is relative to the server dir
// (worlds/<world>/..., server.properties, ...).
type File struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Engine is the backup/restore engine (ticket #9). It is the shared
// operations surface for the Backups screen (#10) and the headless verbs
// (#15); nothing here touches the TUI (D15).
type Engine struct {
	ServerDir    string
	BackupDir    string
	Retention    int
	IncludePacks bool
	// Version is the server version recorded in manifests; "" becomes
	// "unknown".
	Version         string
	Console         *Console
	Sup             *Supervisor
	Trail           *Trail
	processIdentity func(pid int) string
}

// NewEngine builds the engine from the resolved configuration. version is
// the server version for manifests ("" is stored as "unknown").
func NewEngine(cfg *Config, console *Console, sup *Supervisor, trail *Trail, version string) *Engine {
	ret := cfg.Retention
	if ret <= 0 {
		ret = 7
	}
	return &Engine{
		ServerDir:    cfg.ServerDir,
		BackupDir:    cfg.BackupDir,
		Retention:    ret,
		IncludePacks: cfg.IncludePacks,
		Version:      version,
		Console:      console,
		Sup:          sup,
		Trail:        trail,
	}
}

// List returns the complete backups, newest first. A backup is complete
// when its manifest exists (ADR-0006).
func (e *Engine) List() ([]Backup, error) {
	entries, err := os.ReadDir(e.BackupDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list backups: %w", err)
	}
	var out []Backup
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		b, err := e.Inspect(ent.Name())
		if err != nil {
			continue // manifest-less or unreadable: incomplete
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// Inspect reads one backup's manifest.
func (e *Engine) Inspect(id string) (*Backup, error) {
	if err := validateBackupID(id); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(e.BackupDir, id, "manifest.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("backup %s not found", id)
		}
		return nil, fmt.Errorf("read manifest %s: %w", id, err)
	}
	var b Backup
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", id, err)
	}
	return &b, nil
}

// validateBackupID rejects anything that is not a plain directory name, so
// a crafted id cannot escape the backup dir.
func validateBackupID(id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") || id != filepath.Clean(id) {
		return fmt.Errorf("invalid backup id %q", id)
	}
	return nil
}

// Save hold/query/resume protocol strings (BDS 1.26.x; rig-verified in the
// integration tests).
const (
	saveHoldOK         = "Saving..."
	prevSaveIncomplete = "A previous save has not been completed."
	readyToCopy        = "Data saved. Files are now ready to be copied."
	resumeOK           = "are resumed."
)

// coreConfigFiles are the server config files every backup carries (D7).
// Resource and behavior packs are the separate include-packs step.
var coreConfigFiles = []string{
	"server.properties",
	"allowlist.json",
	"permissions.json",
	"packetlimitconfig.json",
}

// LiveBackup takes a consistent backup of a running server: hold saves,
// snapshot the listed world files plus the core config, resume saves.
// The always-resume invariant holds on every exit path after a successful
// hold (Q4/ADR-0006).
func (e *Engine) LiveBackup(ctx context.Context) (*Backup, error) {
	if err := e.check(); err != nil {
		return nil, err
	}
	if e.Console == nil {
		return nil, errors.New("Engine.Console is required for live backup")
	}
	if err := e.acquireLock(ModeLive); err != nil {
		return nil, err
	}
	defer e.releaseLock()
	start := time.Now()

	world, err := worldName(e.ServerDir)
	if err != nil {
		return nil, e.failBackup(ModeLive, "", start, err)
	}
	id, dir, err := e.newBackupDir()
	if err != nil {
		return nil, e.failBackup(ModeLive, "", start, err)
	}

	e.setLockPhase("hold")
	out, err := e.Console.Send(ctx, "save hold")
	if err != nil {
		return nil, e.failBackup(ModeLive, "", start, fmt.Errorf("save hold: %w", err))
	}
	if strings.Contains(out, prevSaveIncomplete) {
		return nil, e.failBackup(ModeLive, "", start, errors.New(
			"a previous save has not been completed; recover it first (CheckRecovery/FinishRecovery)"))
	}
	if !strings.Contains(out, saveHoldOK) {
		return nil, e.failBackup(ModeLive, "", start, fmt.Errorf("save hold: unexpected response %q", oneLine(out)))
	}
	holdStart := time.Now()
	resumed := false
	defer func() {
		if !resumed {
			e.bestEffortResume()
		}
	}()

	e.setLockPhase("query")
	out, err = e.Console.Send(ctx, "save query")
	if err != nil {
		return nil, e.failBackup(ModeLive, id, start, fmt.Errorf("save query: %w", err))
	}
	listed, err := parseSaveQuery(out)
	if err != nil {
		return nil, e.failBackup(ModeLive, id, start, err)
	}

	e.setLockPhase("copy")
	files, configs, total, missing, err := e.copyLiveFiles(dir, listed)
	if err != nil {
		return nil, e.failBackup(ModeLive, id, start, err)
	}

	e.setLockPhase("resume")
	out, err = e.Console.Send(ctx, "save resume")
	if err == nil && !strings.Contains(out, resumeOK) {
		err = fmt.Errorf("save resume: unexpected response %q", oneLine(out))
	}
	if err != nil {
		return nil, e.failBackup(ModeLive, id, start, fmt.Errorf("save resume: %w", err))
	}
	resumed = true
	holdWindow := time.Since(holdStart)

	b := &Backup{
		ID:            id,
		Created:       time.Now().UTC(),
		ServerVersion: e.version(),
		Mode:          ModeLive,
		World:         world,
		Config:        configs,
		Files:         files,
		TotalSize:     total,
	}

	if len(missing) > 0 {
		err := fmt.Errorf("missing world file(s): %s", strings.Join(missing, ", "))
		e.audit("backup", map[string]string{"mode": ModeLive, "id": id}, "error: "+err.Error(), time.Since(start))
		return b, err
	}

	e.setLockPhase("manifest")
	if err := writeManifest(dir, b); err != nil {
		return nil, e.failBackup(ModeLive, id, start, err)
	}
	pruned, pruneErr := e.Prune()
	result := "ok"
	if pruneErr != nil {
		result = "ok (prune failed: " + pruneErr.Error() + ")"
	}
	e.audit("backup", map[string]string{
		"mode":        ModeLive,
		"id":          id,
		"total-size":  strconv.FormatInt(total, 10),
		"hold-window": holdWindow.String(),
		"pruned":      strconv.Itoa(pruned),
	}, result, time.Since(start))
	return b, nil
}

// check validates the engine configuration shared by all operations.
func (e *Engine) check() error {
	if e.ServerDir == "" {
		return errors.New("Engine.ServerDir is required")
	}
	if e.BackupDir == "" {
		return errors.New("Engine.BackupDir is required")
	}
	if e.ServerDir == e.BackupDir || strings.HasPrefix(e.BackupDir, e.ServerDir+string(os.PathSeparator)) {
		return errors.New("backup dir must be outside the server dir (D7)")
	}
	return nil
}

// worldName reads the level name from server.properties; it names the
// world directory under worlds/.
func worldName(serverDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(serverDir, "server.properties"))
	if err != nil {
		return "", fmt.Errorf("read server.properties: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "level-name="); ok {
			name = strings.TrimSpace(name)
			if name == "" {
				return "", errors.New("level-name is empty in server.properties")
			}
			return name, nil
		}
	}
	return "", errors.New("level-name not set in server.properties")
}

// parseSaveQuery verifies the ready response and returns the listed world
// files (paths relative to worlds/).
func parseSaveQuery(out string) ([]string, error) {
	i := strings.Index(out, readyToCopy)
	if i < 0 {
		return nil, fmt.Errorf("save query: expected %q, got %q", readyToCopy, oneLine(out))
	}
	var listed []string
	for _, part := range strings.Split(out[i+len(readyToCopy):], ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		path, size, ok := strings.Cut(p, ":")
		if !ok || size == "" {
			return nil, fmt.Errorf("save query: malformed entry %q (want path:size)", p)
		}
		listed = append(listed, strings.TrimSpace(path))
	}
	if len(listed) == 0 {
		return nil, errors.New("save query: no world files listed")
	}
	return listed, nil
}

// copyLiveFiles copies the listed world files (under worlds/) and the
// existing core config files into the backup dir, mirroring the server
// dir layout. Manifest paths are server-dir-relative. Missing listed files
// are reported, not fatal at this stage: the caller resumes first.
func (e *Engine) copyLiveFiles(dir string, listed []string) (files []File, configs []string, total int64, missing []string, err error) {
	for _, p := range listed {
		p = filepath.Clean(p)
		if filepath.IsAbs(p) || p == ".." || strings.HasPrefix(p, ".."+string(os.PathSeparator)) {
			return nil, nil, 0, nil, fmt.Errorf("save query listed a path outside the server dir: %q", p)
		}
		manifestPath := filepath.Join("worlds", p)
		src := filepath.Join(e.ServerDir, manifestPath)
		if _, statErr := os.Stat(src); statErr != nil {
			if os.IsNotExist(statErr) {
				missing = append(missing, manifestPath)
				continue
			}
			return nil, nil, 0, nil, fmt.Errorf("stat %s: %w", manifestPath, statErr)
		}
		size, copyErr := copyFile(src, filepath.Join(dir, manifestPath))
		if copyErr != nil {
			return nil, nil, 0, nil, copyErr
		}
		files = append(files, File{Path: manifestPath, Size: size})
		total += size
	}
	for _, name := range coreConfigFiles {
		src := filepath.Join(e.ServerDir, name)
		if _, statErr := os.Stat(src); statErr != nil {
			continue
		}
		size, copyErr := copyFile(src, filepath.Join(dir, name))
		if copyErr != nil {
			return nil, nil, 0, nil, copyErr
		}
		configs = append(configs, name)
		files = append(files, File{Path: name, Size: size})
		total += size
	}

	packFiles, packTotal, packErr := e.copyPackDirs(dir)
	if packErr != nil {
		return nil, nil, 0, nil, packErr
	}
	files = append(files, packFiles...)
	total += packTotal
	if err := mirrorTreeModes(e.ServerDir, dir); err != nil {
		return nil, nil, 0, nil, fmt.Errorf("mirror modes: %w", err)
	}
	return files, configs, total, missing, nil
}

// copyPackDirs recursively copies behavior_packs/ and resource_packs/ when
// include-packs is enabled (default off). Pack files land in the manifest
// like any other file, so restore treats them uniformly.
func (e *Engine) copyPackDirs(dir string) ([]File, int64, error) {
	if !e.IncludePacks {
		return nil, 0, nil
	}
	var files []File
	var total int64
	for _, name := range []string{"behavior_packs", "resource_packs"} {
		root := filepath.Join(e.ServerDir, name)
		info, err := os.Stat(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, 0, fmt.Errorf("stat %s: %w", name, err)
		}
		if !info.IsDir() {
			continue
		}
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(e.ServerDir, p)
			if err != nil {
				return err
			}
			size, err := copyFile(p, filepath.Join(dir, rel))
			if err != nil {
				return err
			}
			files = append(files, File{Path: rel, Size: size})
			total += size
			return nil
		})
		if err != nil {
			return nil, 0, fmt.Errorf("copy %s: %w", name, err)
		}
	}
	return files, total, nil
}

// copyFile copies one file, creating parent dirs, and returns its size.
func copyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir for %s: %w", dst, err)
	}
	out, err := os.Create(dst)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", dst, err)
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	if n != fi.Size() {
		return 0, fmt.Errorf("copy %s: short write", src)
	}
	// Preserve the source file's permission bits (e.g. group-write) so a
	// server running under a different uid can still write it after restore.
	if err := os.Chmod(dst, fi.Mode().Perm()); err != nil {
		return 0, fmt.Errorf("chmod %s: %w", dst, err)
	}
	return fi.Size(), nil
}

// mirrorTreeModes walks the directories under srcRoot and chmods the
// corresponding directories under dstRoot to the source's permission bits
// (including setgid). Only directories present in both trees are touched, so
// it never reaches outside the copied subtree. This is what lets a server
// running under a different uid create and write files in a restored world.
func mirrorTreeModes(srcRoot, dstRoot string) error {
	return filepath.WalkDir(srcRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || p == srcRoot {
			return nil
		}
		rel, err := filepath.Rel(srcRoot, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dstRoot, rel)
		if _, err := os.Stat(dst); err != nil {
			return nil // dest dir not present; skip
		}
		st, err := os.Stat(p)
		if err != nil {
			return err
		}
		mode := st.Mode()
		// os.Chmod applies only .Perm(); carry the setgid bit over explicitly.
		if mode&os.ModeSetgid != 0 {
			mode = mode.Perm() | os.ModeSetgid
		} else {
			mode = mode.Perm()
		}
		if err := os.Chmod(dst, mode); err != nil {
			// In a cross-user deployment the destination dir may be owned by
			// the server user (e.g. the world root). We cannot chmod it, but it
			// already carries the operator's permission model, so skipping is
			// correct; only surface genuinely unexpected errors.
			var pe *os.PathError
			if errors.As(err, &pe) && (pe.Err == syscall.EPERM || pe.Err == syscall.EACCES) {
				return nil
			}
			return fmt.Errorf("chmod %s: %w", dst, err)
		}
		return nil
	})
}

// writeManifest writes manifest.json atomically (temp + rename). It is the
// completion marker (ADR-0006).
func writeManifest(dir string, b *Backup) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	tmp := filepath.Join(dir, "manifest.json.tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "manifest.json")); err != nil {
		return fmt.Errorf("rename manifest: %w", err)
	}
	return nil
}

// bestEffortResume fires save resume on a background context when the
// normal path could not; the invariant is that hold is never left open.
func (e *Engine) bestEffortResume() {
	if e.Console == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = e.Console.Send(ctx, "save resume")
}

// failBackup audits a failed backup attempt and returns the error.
func (e *Engine) failBackup(mode, id string, start time.Time, err error) error {
	e.audit("backup", map[string]string{"mode": mode, "id": id}, "error: "+err.Error(), time.Since(start))
	return err
}

// audit appends one trail event, best effort: an audit failure must not
// mask the operation result.
func (e *Engine) audit(verb string, params map[string]string, result string, dur time.Duration) {
	if e.Trail == nil {
		return
	}
	_ = e.Trail.Append(Event{Verb: verb, Params: params, Result: result, Duration: dur})
}

// oneLine flattens a console response for use inside error messages.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}

// StoppedBackup stops the server, snapshots the entire world directory
// (excluding level.dat_old) plus config and included packs, writes the
// manifest, then starts the server again. Any failure after our stop
// attempts a best-effort restart (Q8).
func (e *Engine) StoppedBackup(ctx context.Context) (*Backup, error) {
	if err := e.check(); err != nil {
		return nil, err
	}
	if e.Sup == nil {
		return nil, errors.New("Engine.Sup is required for stopped backup")
	}
	start := time.Now()

	world, err := worldName(e.ServerDir)
	if err != nil {
		return nil, e.failBackup(ModeStopped, "", start, err)
	}
	id, dir, err := e.newBackupDir()
	if err != nil {
		return nil, e.failBackup(ModeStopped, "", start, err)
	}

	if err := e.acquireLock(ModeStopped); err != nil {
		return nil, err
	}
	defer e.releaseLock()

	e.setLockPhase("stop")
	if err := e.Sup.Stop(ctx); err != nil {
		return nil, e.failBackup(ModeStopped, id, start, fmt.Errorf("stop: %w", err))
	}
	reachedStart := false
	defer func() {
		if !reachedStart {
			e.bestEffortStart()
		}
	}()
	running, err := e.Sup.Running(ctx)
	if err != nil {
		return nil, e.failBackup(ModeStopped, id, start, fmt.Errorf("verify stopped: %w", err))
	}
	if running {
		return nil, e.failBackup(ModeStopped, id, start, errors.New("server still running after stop"))
	}

	e.setLockPhase("copy")
	files, configs, total, err := e.copyStoppedFiles(dir, world)
	if err != nil {
		return nil, e.failBackup(ModeStopped, id, start, err)
	}

	b := &Backup{
		ID:            id,
		Created:       time.Now().UTC(),
		ServerVersion: e.version(),
		Mode:          ModeStopped,
		World:         world,
		Config:        configs,
		Files:         files,
		TotalSize:     total,
	}
	e.setLockPhase("manifest")
	if err := writeManifest(dir, b); err != nil {
		return nil, e.failBackup(ModeStopped, id, start, err)
	}
	pruned, pruneErr := e.Prune()
	result := "ok"
	if pruneErr != nil {
		result = "ok (prune failed: " + pruneErr.Error() + ")"
	}

	reachedStart = true
	e.setLockPhase("start")
	if err := e.Sup.Start(ctx); err != nil {
		e.audit("backup", map[string]string{"mode": ModeStopped, "id": id, "pruned": strconv.Itoa(pruned)},
			"error: backup ok, start failed: "+err.Error(), time.Since(start))
		return b, fmt.Errorf("backup completed but server start failed: %w", err)
	}
	e.audit("backup", map[string]string{
		"mode":       ModeStopped,
		"id":         id,
		"total-size": strconv.FormatInt(total, 10),
		"pruned":     strconv.Itoa(pruned),
	}, result, time.Since(start))
	return b, nil
}

// bestEffortStart restarts the server on a fresh context when we stopped
// it and the operation failed before the normal start; a start failure
// here is not reported, the operation error is what matters.
func (e *Engine) bestEffortStart() {
	if e.Sup == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = e.Sup.Start(ctx)
}

// copyStoppedFiles snapshots the entire world directory (excluding
// level.dat_old) plus the core config and included packs.
func (e *Engine) copyStoppedFiles(dir, world string) (files []File, configs []string, total int64, err error) {
	worldRoot := filepath.Join(e.ServerDir, "worlds", world)
	err = filepath.WalkDir(worldRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if filepath.Base(p) == "level.dat_old" {
			return nil
		}
		rel, err := filepath.Rel(e.ServerDir, p)
		if err != nil {
			return err
		}
		size, err := copyFile(p, filepath.Join(dir, rel))
		if err != nil {
			return err
		}
		files = append(files, File{Path: rel, Size: size})
		total += size
		return nil
	})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("copy world: %w", err)
	}
	for _, name := range coreConfigFiles {
		src := filepath.Join(e.ServerDir, name)
		if _, statErr := os.Stat(src); statErr != nil {
			continue
		}
		size, copyErr := copyFile(src, filepath.Join(dir, name))
		if copyErr != nil {
			return nil, nil, 0, copyErr
		}
		configs = append(configs, name)
		files = append(files, File{Path: name, Size: size})
		total += size
	}
	pf, pt, perr := e.copyPackDirs(dir)
	if perr != nil {
		return nil, nil, 0, perr
	}
	files = append(files, pf...)
	total += pt
	if err := mirrorTreeModes(e.ServerDir, dir); err != nil {
		return nil, nil, 0, fmt.Errorf("mirror modes: %w", err)
	}
	return files, configs, total, nil
}

// Prune keeps the newest Retention complete backups and removes the rest.
// Manifest-less dirs are incomplete trash and are always removed
// (ADR-0006). Safety-nets count like any other backup. Returns the number
// of entries removed.
func (e *Engine) Prune() (int, error) {
	if e.BackupDir == "" {
		return 0, errors.New("Engine.BackupDir is required")
	}
	entries, err := os.ReadDir(e.BackupDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read backup dir: %w", err)
	}
	keep := e.Retention
	if keep <= 0 {
		keep = 7
	}
	type candidate struct {
		name    string
		created time.Time
		valid   bool
	}
	var all []candidate
	for _, en := range entries {
		if !en.IsDir() {
			continue
		}
		bk, err := e.Inspect(en.Name())
		if err != nil {
			all = append(all, candidate{name: en.Name()})
			continue
		}
		all = append(all, candidate{name: en.Name(), created: bk.Created, valid: true})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].created.Equal(all[j].created) {
			return all[i].name > all[j].name
		}
		return all[i].created.After(all[j].created)
	})
	pruned := 0
	kept := 0
	for _, c := range all {
		if c.valid && kept < keep {
			kept++
			continue
		}
		if err := os.RemoveAll(filepath.Join(e.BackupDir, c.name)); err != nil {
			return pruned, fmt.Errorf("remove %s: %w", c.name, err)
		}
		pruned++
	}
	return pruned, nil
}

// RestoreResult reports what Restore did.
type RestoreResult struct {
	// SafetyNet is the id of the pre-restore snapshot; undo = Restore(it).
	SafetyNet string
}

// Restore returns the server to the state of backup id: verify manifest,
// stop, safety-net the current world, wipe the current world and config,
// copy the backup's set, remove level.dat_old, start. Every abort after
// our own stop attempts a restart (Q8). Undo = Restore(safety-net id).
func (e *Engine) Restore(ctx context.Context, id string) (*RestoreResult, error) {
	if err := e.check(); err != nil {
		return nil, err
	}
	if e.Sup == nil {
		return nil, errors.New("Engine.Sup is required for restore")
	}
	start := time.Now()

	src, err := e.Inspect(id)
	if err != nil {
		return nil, e.failRestore(id, "", start, fmt.Errorf("verify backup: %w", err))
	}

	if err := e.acquireLock("restore"); err != nil {
		return nil, err
	}
	defer e.releaseLock()

	world, err := worldName(e.ServerDir)
	if err != nil {
		return nil, e.failRestore(id, "", start, err)
	}

	e.setLockPhase("stop")
	if err := e.Sup.Stop(ctx); err != nil {
		return nil, e.failRestore(id, "", start, fmt.Errorf("stop: %w", err))
	}
	reachedStart := false
	defer func() {
		if !reachedStart {
			e.bestEffortStart()
		}
	}()
	running, err := e.Sup.Running(ctx)
	if err != nil {
		return nil, e.failRestore(id, "", start, fmt.Errorf("verify stopped: %w", err))
	}
	if running {
		return nil, e.failRestore(id, "", start, errors.New("server still running after stop"))
	}

	// Safety-net the current world before anything is overwritten.
	snetID, snetDir, err := e.newBackupDir()
	if err != nil {
		return nil, e.failRestore(id, snetID, start, err)
	}
	e.setLockPhase("safety-net")
	sfiles, sconfigs, stotal, err := e.copyStoppedFiles(snetDir, world)
	if err != nil {
		return nil, e.failRestore(id, snetID, start, err)
	}
	snet := &Backup{
		ID:            snetID,
		Created:       time.Now().UTC(),
		ServerVersion: e.version(),
		Mode:          ModeSafetyNet,
		World:         world,
		Config:        sconfigs,
		Files:         sfiles,
		TotalSize:     stotal,
	}
	if err := writeManifest(snetDir, snet); err != nil {
		return nil, e.failRestore(id, snetID, start, err)
	}

	// Wipe the world dirs being replaced (current and, if different, the
	// backup's) plus the core config, then copy the backup's set.
	e.setLockPhase("copy")
	if err := e.wipeForRestore(world, src.World); err != nil {
		return nil, e.failRestore(id, snetID, start, err)
	}
	if err := e.copyRestoreSet(src); err != nil {
		return nil, e.failRestore(id, snetID, start, err)
	}
	// level.dat_old is BDS's own pre-save snapshot; no restore set carries
	// it (Q8).
	os.Remove(filepath.Join(e.ServerDir, "worlds", src.World, "level.dat_old"))

	e.setLockPhase("start")
	reachedStart = true
	if err := e.Sup.Start(ctx); err != nil {
		e.audit("restore", map[string]string{"id": id, "safety-net-id": snetID},
			"error: restore ok, start failed: "+err.Error(), time.Since(start))
		return &RestoreResult{SafetyNet: snetID},
			fmt.Errorf("restore completed but server start failed: %w (undo: Restore(%s))", err, snetID)
	}

	_, _ = e.Prune()
	e.audit("restore", map[string]string{"id": id, "safety-net-id": snetID}, "ok", time.Since(start))
	return &RestoreResult{SafetyNet: snetID}, nil
}

// failRestore audits a failed restore and returns the error.
func (e *Engine) failRestore(id, snet string, start time.Time, err error) error {
	e.audit("restore", map[string]string{"id": id, "safety-net-id": snet}, "error: "+err.Error(), time.Since(start))
	return err
}

// wipeForRestore removes the world dirs being replaced plus the core
// config files. Missing entries are not errors.
func (e *Engine) wipeForRestore(worlds ...string) error {
	seen := map[string]bool{}
	for _, w := range worlds {
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		if err := os.RemoveAll(filepath.Join(e.ServerDir, "worlds", w)); err != nil {
			return fmt.Errorf("wipe world %s: %w", w, err)
		}
	}
	for _, name := range coreConfigFiles {
		os.Remove(filepath.Join(e.ServerDir, name))
	}
	return nil
}

// copyRestoreSet copies every file in the backup manifest into the server
// dir; manifest paths are server-dir-relative.
func (e *Engine) copyRestoreSet(src *Backup) error {
	for _, f := range src.Files {
		from := filepath.Join(e.BackupDir, src.ID, f.Path)
		to := filepath.Join(e.ServerDir, f.Path)
		if _, err := copyFile(from, to); err != nil {
			return fmt.Errorf("restore %s: %w", f.Path, err)
		}
	}
	if err := mirrorTreeModes(filepath.Join(e.BackupDir, src.ID), e.ServerDir); err != nil {
		return fmt.Errorf("mirror modes: %w", err)
	}
	return nil
}

// newBackupDir allocates a unique <YYYYMMDD-HHMMSS> directory under the
// backup dir, retrying across second boundaries on collision.
func (e *Engine) newBackupDir() (string, string, error) {
	for i := 0; i < 20; i++ {
		id := time.Now().UTC().Format("20060102-150405")
		dir := filepath.Join(e.BackupDir, id)
		if _, err := os.Stat(dir); err != nil {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", "", fmt.Errorf("create backup dir: %w", err)
			}
			return id, dir, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return "", "", errors.New("could not allocate a unique backup id")
}

// version returns the engine's server version, or a placeholder.
func (e *Engine) version() string {
	if e.Version != "" {
		return e.Version
	}
	return "unknown"
}
