package compass

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSup is a recording Supervisor for offline Lifecycle tests. It records
// the action sequence and can be told to fail a specific action.
type fakeSup struct {
	mu        sync.Mutex
	calls     []string
	failStart error
	failStop  error
	status    ProcessStatus
	statusErr error
}

func (f *fakeSup) record(c string) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
}

func (f *fakeSup) new() *Supervisor {
	return &Supervisor{
		Stop: func(ctx context.Context) error {
			f.record("stop")
			return f.failStop
		},
		Start: func(ctx context.Context) error {
			f.record("start")
			return f.failStart
		},
		Running: func(ctx context.Context) (bool, error) { return f.status.Active, nil },
		Status: func(ctx context.Context) (ProcessStatus, error) {
			return f.status, f.statusErr
		},
	}
}

func (f *fakeSup) seq() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func newTestLifecycle(t *testing.T, sup *Supervisor, console *Console) (*Lifecycle, *Trail) {
	t.Helper()
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	cfg := &Config{ServerDir: t.TempDir(), Port: 19132}
	return &Lifecycle{Cfg: cfg, Sup: sup, Console: console, Trail: trail}, trail
}

func trailEvents(t *testing.T, tr *Trail) []Event {
	t.Helper()
	evs, err := tr.Recent(50)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	// Recent returns newest first; flip to chronological.
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	return evs
}

func lastLifecycleEvent(t *testing.T, tr *Trail) Event {
	t.Helper()
	evs := trailEvents(t, tr)
	if len(evs) == 0 {
		t.Fatal("audit trail is empty, want one lifecycle event")
	}
	return evs[len(evs)-1]
}

func TestLifecycle_Start_AuditsOk(t *testing.T) {
	sup := (&fakeSup{}).new()
	lc, tr := newTestLifecycle(t, sup, nil)

	if err := lc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ev := lastLifecycleEvent(t, tr)
	if ev.Verb != "lifecycle" {
		t.Errorf("Verb = %q, want lifecycle", ev.Verb)
	}
	if ev.Params["action"] != "start" {
		t.Errorf("Params[action] = %q, want start", ev.Params["action"])
	}
	if ev.Result != "ok" {
		t.Errorf("Result = %q, want ok", ev.Result)
	}
}

func TestLifecycle_Start_ErrorIsAudited(t *testing.T) {
	fs := &fakeSup{failStart: errors.New("systemctl start: access denied")}
	lc, tr := newTestLifecycle(t, fs.new(), nil)

	if err := lc.Start(context.Background()); err == nil {
		t.Fatal("Start must surface the supervisor failure")
	}
	ev := lastLifecycleEvent(t, tr)
	if !strings.Contains(ev.Result, "error:") || !strings.Contains(ev.Result, "access denied") {
		t.Errorf("Result = %q, want error:… access denied", ev.Result)
	}
}

func TestLifecycle_Stop_AuditsOk(t *testing.T) {
	sup := (&fakeSup{}).new()
	lc, tr := newTestLifecycle(t, sup, nil)

	if err := lc.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	ev := lastLifecycleEvent(t, tr)
	if ev.Params["action"] != "stop" || ev.Result != "ok" {
		t.Errorf("event = %+v, want action=stop result=ok", ev)
	}
}

func TestLifecycle_Restart_StopsThenStarts(t *testing.T) {
	fs := &fakeSup{}
	lc, _ := newTestLifecycle(t, fs.new(), nil)

	if err := lc.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	seq := fs.seq()
	if len(seq) != 2 || seq[0] != "stop" || seq[1] != "start" {
		t.Fatalf("call sequence = %v, want [stop start]", seq)
	}
}

func TestLifecycle_Restart_StopFails_StartNotCalled(t *testing.T) {
	fs := &fakeSup{failStop: errors.New("systemctl stop: failed")}
	lc, tr := newTestLifecycle(t, fs.new(), nil)

	if err := lc.Restart(context.Background()); err == nil {
		t.Fatal("Restart must fail when stop fails")
	}
	if seq := fs.seq(); len(seq) != 1 || seq[0] != "stop" {
		t.Errorf("call sequence = %v, want exactly [stop] (start must not run)", seq)
	}
	ev := lastLifecycleEvent(t, tr)
	if !strings.Contains(ev.Result, "stop") {
		t.Errorf("audit Result = %q, must name the failing step", ev.Result)
	}
}

func TestLifecycle_Restart_StartFails_SaysServerStopped(t *testing.T) {
	fs := &fakeSup{failStart: errors.New("boom")}
	lc, _ := newTestLifecycle(t, fs.new(), nil)

	err := lc.Restart(context.Background())
	if err == nil {
		t.Fatal("Restart must fail when start fails")
	}
	if !strings.Contains(err.Error(), "stopped") {
		t.Errorf("error should tell the operator the server is left stopped, got: %v", err)
	}
}

// TestLifecycle_Restart_ErrorHasNoDoublePrefix pins ticket #13's review fix:
// both callers (the TUI toast and the headless verb) prepend "restart:", so
// the service error must not carry its own — otherwise it renders as
// "restart: restart: …". The failing step is still named.
func TestLifecycle_Restart_ErrorHasNoDoublePrefix(t *testing.T) {
	fs := &fakeSup{failStop: errors.New("denied")}
	lc, _ := newTestLifecycle(t, fs.new(), nil)

	err := lc.Restart(context.Background())
	if err == nil {
		t.Fatal("Restart must fail when stop fails")
	}
	msg := err.Error()
	if strings.Contains(msg, "restart:") {
		t.Errorf("error must not self-prefix with \"restart:\", got: %v", msg)
	}
	if !strings.Contains(msg, "stop failed") || !strings.Contains(msg, "denied") {
		t.Errorf("error must name the failing step and cause, got: %v", msg)
	}
}

// TestLifecycle_Adopt_ErrorHasNoDoublePrefix pins the same contract for
// adopt (ticket #13 review): the TUI toast renders "adopt: <err>", so the
// service error must not start with its own "adopt:" prefix. The failing
// step is still named.
func TestLifecycle_Adopt_ErrorHasNoDoublePrefix(t *testing.T) {
	lc, _ := newTestLifecycle(t, nil, nil)
	lc.Cfg = adoptCfg()
	lc.Discover = func(ctx context.Context) (*Discovered, error) {
		return nil, errors.New("two bedrock_server processes on this host")
	}

	_, err := lc.Adopt(context.Background(), 0)
	if err == nil {
		t.Fatal("Adopt must fail when discovery fails")
	}
	msg := err.Error()
	if strings.Contains(msg, "adopt:") {
		t.Errorf("error must not self-prefix with \"adopt:\", got: %v", msg)
	}
	if !strings.Contains(msg, "discover failed") || !strings.Contains(msg, "two bedrock_server") {
		t.Errorf("error must name the failing step and cause, got: %v", msg)
	}
}

func TestLifecycle_NoSupervisor_ClearError(t *testing.T) {
	lc, _ := newTestLifecycle(t, nil, nil)
	for name, fn := range map[string]func(context.Context) error{
		"start":   lc.Start,
		"stop":    lc.Stop,
		"restart": lc.Restart,
	} {
		if err := fn(context.Background()); err == nil {
			t.Errorf("%s without a supervisor must error", name)
		} else if !strings.Contains(err.Error(), "no supervisor") {
			t.Errorf("%s error should name the missing supervisor, got: %v", name, err)
		}
	}
}

func TestLifecycle_Status_FromSupervisor(t *testing.T) {
	sup := (&fakeSup{status: ProcessStatus{
		Active: true,
		Unit:   "minecraft-bedrock.service",
		PID:    4242,
		Uptime: 3*time.Hour + 7*time.Minute,
		Source: "systemd",
	}}).new()
	lc, _ := newTestLifecycle(t, sup, nil)
	// A pong probe that answers, to verify the Status carries it.
	lc.Ping = func(ctx context.Context, addr string) (Pong, error) {
		if !strings.HasSuffix(addr, ":19132") {
			t.Errorf("ping addr = %q, want 127.0.0.1:19132", addr)
		}
		return Pong{Name: "test", Version: "1.26.430", Online: 1, MaxPlayers: 2}, nil
	}

	st := lc.Status(context.Background())
	if st.State != "running" {
		t.Errorf("State = %q, want running", st.State)
	}
	if st.Source != "systemd" || st.Unit != "minecraft-bedrock.service" {
		t.Errorf("Source/Unit = %q/%q, want systemd/minecraft-bedrock.service", st.Source, st.Unit)
	}
	if st.PID != 4242 {
		t.Errorf("PID = %d, want 4242", st.PID)
	}
	if st.Uptime < 3*time.Hour || st.Uptime > 3*time.Hour+30*time.Minute {
		t.Errorf("Uptime = %v, want ~3h7m", st.Uptime)
	}
	if st.Pong == nil || st.Pong.Name != "test" {
		t.Errorf("Pong = %+v, want the ping answer", st.Pong)
	}
}

func TestLifecycle_Status_StoppedSupervisor(t *testing.T) {
	sup := (&fakeSup{status: ProcessStatus{Active: false, Source: "systemd"}}).new()
	lc, _ := newTestLifecycle(t, sup, nil)
	lc.Ping = func(ctx context.Context, addr string) (Pong, error) {
		return Pong{}, errors.New("connection refused")
	}

	st := lc.Status(context.Background())
	if st.State != "stopped" {
		t.Errorf("State = %q, want stopped", st.State)
	}
	// A failed pong probe must degrade to no pong, not an error.
	if st.Pong != nil {
		t.Errorf("Pong = %+v, want nil when the probe fails", st.Pong)
	}
}

func TestLifecycle_Status_DegradesToProcessTruth(t *testing.T) {
	// No supervisor wired at all (a tmux deployment before #13, or a
	// config that resolved no unit): Status must fall back to the
	// process table. This host may or may not be running a server, so
	// the assertion is conditional but the shape is always checked.
	lc, _ := newTestLifecycle(t, nil, nil)
	lc.Ping = func(ctx context.Context, addr string) (Pong, error) {
		return Pong{}, errors.New("refused")
	}

	st := lc.Status(context.Background())
	pid, err := ServerProcessPID(context.Background())
	if err != nil {
		t.Fatalf("ServerProcessPID: %v", err)
	}
	if pid > 0 {
		if st.State != "running" || st.Source != "process" || st.PID != pid {
			t.Errorf("process-truth status = %+v, want running/process/%d", st, pid)
		}
	} else if st.State != "unknown" {
		t.Errorf("status with no server running = %+v, want unknown", st)
	}
}

// --- adopt -----------------------------------------------------------------

func adoptCfg() *Config {
	return &Config{Port: 19132}
}

func TestLifecycle_Adopt_HappyPath(t *testing.T) {
	lc, tr := newTestLifecycle(t, nil, nil)
	lc.Cfg = adoptCfg()
	lc.Discover = func(ctx context.Context) (*Discovered, error) {
		return &Discovered{PID: 1234, Dir: "/srv/mc", Unit: "minecraft-bedrock.service"}, nil
	}
	lc.Ping = func(ctx context.Context, addr string) (Pong, error) {
		return Pong{Name: "prod", Online: 0, MaxPlayers: 2}, nil
	}
	lc.ConsoleCheck = func(ctx context.Context, c *Console) (string, error) {
		return "Bedrock server 1.26.43.1 ... 0/2 players online", nil
	}
	// defaultFIFOFor shells out to systemctl on the host; the result is
	// either the real FIFO or the documented default — both acceptable.
	_ = tr

	a, err := lc.Adopt(context.Background(), 0)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if a.Supervisor != "systemd" {
		t.Errorf("Supervisor = %q, want systemd", a.Supervisor)
	}
	if a.Service != "minecraft-bedrock.service" {
		t.Errorf("Service = %q, want minecraft-bedrock.service", a.Service)
	}
	if a.ServerDir != "/srv/mc" {
		t.Errorf("ServerDir = %q, want /srv/mc", a.ServerDir)
	}
	if a.ConsoleFif == "" {
		t.Error("ConsoleFif is empty, want a FIFO path")
	}
	if !strings.Contains(a.Console, "players online") {
		t.Errorf("Console = %q, want the roster line", a.Console)
	}
}

// K-01: after provisioning a deployment on a non-default port, the one-click
// adopt must probe THAT port, not the (stale) configured port. The port hint
// is what the TUI passes when a provision was just applied.
func TestLifecycle_Adopt_PortHintOverridesConfiguredPort(t *testing.T) {
	lc, _ := newTestLifecycle(t, nil, nil)
	lc.Cfg = &Config{Port: 19142} // a stale configured port from an old deployment
	lc.Discover = func(ctx context.Context) (*Discovered, error) {
		return &Discovered{PID: 1234, Dir: "/srv/mc", Unit: "minecraft-bedrock.service"}, nil
	}
	var probed string
	lc.Ping = func(ctx context.Context, addr string) (Pong, error) {
		probed = addr
		return Pong{Name: "fresh", Online: 0, MaxPlayers: 2}, nil
	}
	lc.ConsoleCheck = func(ctx context.Context, c *Console) (string, error) {
		return "0/2 players online", nil
	}

	a, err := lc.Adopt(context.Background(), 19200)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if probed != "127.0.0.1:19200" {
		t.Errorf("probed %q, want the port hint 127.0.0.1:19200 (not the stale configured 127.0.0.1:19142)", probed)
	}
	if a.Port != 19200 {
		t.Errorf("Adopted.Port = %d, want the port hint 19200", a.Port)
	}
}

// A port hint of 0 means "use the configured/default port" (the general
// adopt case — headless verb, or an adopt with no fresh provision).
func TestLifecycle_Adopt_ZeroHintFallsBackToConfiguredPort(t *testing.T) {
	lc, _ := newTestLifecycle(t, nil, nil)
	lc.Cfg = &Config{Port: 19142}
	lc.Discover = func(ctx context.Context) (*Discovered, error) {
		return &Discovered{PID: 1234, Dir: "/srv/mc", Unit: "minecraft-bedrock.service"}, nil
	}
	var probed string
	lc.Ping = func(ctx context.Context, addr string) (Pong, error) {
		probed = addr
		return Pong{Name: "prod", Online: 0, MaxPlayers: 2}, nil
	}
	lc.ConsoleCheck = func(ctx context.Context, c *Console) (string, error) {
		return "0/2 players online", nil
	}

	if _, err := lc.Adopt(context.Background(), 0); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if probed != "127.0.0.1:19142" {
		t.Errorf("probed %q, want the configured port 127.0.0.1:19142 when the hint is 0", probed)
	}
}

func TestLifecycle_Adopt_PongFails_NoClaim(t *testing.T) {
	lc, _ := newTestLifecycle(t, nil, nil)
	lc.Cfg = adoptCfg()
	lc.Discover = func(ctx context.Context) (*Discovered, error) {
		return &Discovered{PID: 1, Dir: "/srv/mc", Unit: "u.service"}, nil
	}
	lc.Ping = func(ctx context.Context, addr string) (Pong, error) {
		return Pong{}, errors.New("connection refused")
	}
	consoleCalled := false
	lc.ConsoleCheck = func(ctx context.Context, c *Console) (string, error) {
		consoleCalled = true
		return "players online", nil
	}

	if _, err := lc.Adopt(context.Background(), 0); err == nil {
		t.Fatal("Adopt must fail when the pong probe fails")
	} else if !strings.Contains(err.Error(), "pong") {
		t.Errorf("error = %v, want it to name the failed pong probe", err)
	}
	if consoleCalled {
		t.Error("the console round-trip must not run when pong failed")
	}
}

func TestLifecycle_Adopt_ConsoleNotARoster_Rejected(t *testing.T) {
	lc, _ := newTestLifecycle(t, nil, nil)
	lc.Cfg = adoptCfg()
	lc.Discover = func(ctx context.Context) (*Discovered, error) {
		return &Discovered{PID: 1, Dir: "/srv/mc", Unit: "u.service"}, nil
	}
	lc.Ping = func(ctx context.Context, addr string) (Pong, error) {
		return Pong{}, nil
	}
	lc.ConsoleCheck = func(ctx context.Context, c *Console) (string, error) {
		return "some other output without a roster", nil
	}

	if _, err := lc.Adopt(context.Background(), 0); err == nil {
		t.Fatal("Adopt must reject a console reply that is not a roster line")
	} else if !strings.Contains(err.Error(), "roster") {
		t.Errorf("error = %v, want it to name the missing roster line", err)
	}
}

func TestLifecycle_Adopt_DiscoverFails(t *testing.T) {
	lc, _ := newTestLifecycle(t, nil, nil)
	lc.Cfg = adoptCfg()
	lc.Discover = func(ctx context.Context) (*Discovered, error) {
		return nil, errors.New("no running bedrock_server found")
	}
	lc.Ping = func(ctx context.Context, addr string) (Pong, error) {
		t.Error("pong must not run when discovery failed")
		return Pong{}, nil
	}

	if _, err := lc.Adopt(context.Background(), 0); err == nil {
		t.Fatal("Adopt must fail when discovery fails")
	} else if !strings.Contains(err.Error(), "discover") {
		t.Errorf("error = %v, want it to name the discovery failure", err)
	}
}

func TestLifecycle_Adopt_NoUnitNoPane_NoChannel(t *testing.T) {
	lc, _ := newTestLifecycle(t, nil, nil)
	lc.Cfg = adoptCfg() // no pane
	lc.Discover = func(ctx context.Context) (*Discovered, error) {
		return &Discovered{PID: 1, Dir: "/srv/mc", Unit: ""}, nil // no unit
	}
	lc.Ping = func(ctx context.Context, addr string) (Pong, error) {
		return Pong{}, nil
	}

	_, err := lc.Adopt(context.Background(), 0)
	if err == nil {
		t.Fatal("Adopt must fail when there is no unit and no pane to verify over")
	}
	if !strings.Contains(err.Error(), "console") {
		t.Errorf("error = %v, want it to name the missing console channel", err)
	}
}

func TestLifecycle_PersistAdopt_WritesConfig(t *testing.T) {
	lc, _ := newTestLifecycle(t, nil, nil)
	lc.Cfg = &Config{BackupDir: "/existing/backups", Retention: 7}

	a := &Adopted{
		Discovery:  Discovered{PID: 1, Dir: "/srv/mc", Unit: "minecraft-bedrock.service"},
		Supervisor: "systemd",
		Service:    "minecraft-bedrock.service",
		ServerDir:  "/srv/mc",
		Port:       19132,
		ConsoleFif: "/run/bedrock-console.fifo",
	}
	path := filepath.Join(t.TempDir(), "cfg", "config.yaml")
	if err := lc.PersistAdopt(path, a); err != nil {
		t.Fatalf("PersistAdopt: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after persist: %v", err)
	}
	if loaded.ServerDir != "/srv/mc" {
		t.Errorf("ServerDir = %q, want /srv/mc", loaded.ServerDir)
	}
	if loaded.Supervisor != "systemd" || loaded.Service != "minecraft-bedrock.service" {
		t.Errorf("Supervisor/Service = %q/%q, want systemd/minecraft-bedrock.service", loaded.Supervisor, loaded.Service)
	}
	if loaded.Port != 19132 {
		t.Errorf("Port = %d, want 19132", loaded.Port)
	}
	if loaded.ConsoleFif != "/run/bedrock-console.fifo" {
		t.Errorf("ConsoleFif = %q, want /run/bedrock-console.fifo", loaded.ConsoleFif)
	}
	// Non-adopted fields are preserved.
	if loaded.BackupDir != "/existing/backups" || loaded.Retention != 7 {
		t.Errorf("existing config fields lost: BackupDir=%q Retention=%d", loaded.BackupDir, loaded.Retention)
	}
}

// Config.Save round-trip and atomicity (the temp+rename path).
func TestConfig_SaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.yaml")
	c := &Config{
		ServerDir:  "/srv/mc",
		Supervisor: "systemd",
		Service:    "minecraft-bedrock.service",
		BackupDir:  "/backups",
		Retention:  5,
		Port:       19132,
		ConsoleFif: "/run/bedrock-console.fifo",
	}
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file missing after Save: %v", err)
	}
	// No temp file should be left behind.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind after Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ServerDir != c.ServerDir || loaded.Service != c.Service || loaded.Retention != c.Retention {
		t.Errorf("round-trip mismatch: got %+v, want %+v", loaded, c)
	}
}

func TestConfig_SavePreservesUnknownRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// Save then load must not lose data through the YAML tags.
	c := &Config{ServerDir: "/a", Supervisor: "tmux", Pane: "mc:0.0", Retention: 3}
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Supervisor != "tmux" || loaded.Pane != "mc:0.0" {
		t.Errorf("tmux/pane round-trip: got %+v", loaded)
	}
}

// The audit trail must not be required to exist for a lifecycle action to
// succeed — a missing/unwritable trail degrades to no audit, not a failure.
func TestLifecycle_AuditFailureDoesNotBreakAction(t *testing.T) {
	sup := (&fakeSup{}).new()
	lc, _ := newTestLifecycle(t, sup, nil)
	// Point the trail at a path that cannot be created (a file, not a dir).
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr, err := NewTrail(filepath.Join(blocker, "audit.jsonl"))
	if err == nil {
		t.Fatal("NewTrail under a file should fail")
	}
	lc.Trail = tr // nil in practice; the action must not panic or fail
	if err := lc.Start(context.Background()); err != nil {
		t.Errorf("Start with an unusable trail must still succeed, got: %v", err)
	}
}
