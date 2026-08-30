package compass

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Stack is the resolved wiring of a bedrock-compass deployment (ticket #10):
// the config, the console channel, the supervisor, the audit trail, the
// backup engine, and the scheduled-backup timer. Both consumers build one
// with NewStack so the TUI's Backups screen and the headless `backup-now`
// verb share exactly one wiring scheme (no forked logic):
//
//   - the TUI mounts a Backups screen over Stack.Engine + Stack.Timer.
//   - `bedrock-compass backup-now --mode live` runs Stack.BackupNow.
//
// Partial failures are tolerated: a deployment that cannot resolve a console
// or supervisor (e.g. a stopped server, a tmux setup before #13) still gets a
// Stack whose Engine supports the operations its available channels allow.
type Stack struct {
	Cfg     *Config
	Console *Console
	Sup     *Supervisor
	Trail   *Trail
	Engine  *Engine
	Timer   *Timer
	// Journal is the unit's journal (systemd deployments only; nil for tmux).
	// The Players service mines it for recently-connected players (D3b) and
	// the Logs screen (#8, D9) will read from it.
	Journal *Journal
	// Lifecycle is the server lifecycle + adopt surface (#13): start /
	// stop / restart / status / adopt over the supervisor, audited. Both
	// the TUI's Server screen and the headless lifecycle verbs build one
	// with NewStack so they share exactly one code path (no forked logic).
	Lifecycle *Lifecycle
	// Players is the roster + per-player action surface (#6): list / kick /
	// op / deop / allow over the console channel, audited. The TUI's
	// Players screen renders it; nothing else knows about the other.
	Players *Players
	// Access is the deny-mechanism surface (#7): gate read/toggle (the
	// two-layer allow-list property + live on/off), allowlist add / remove /
	// reload by gamertag, and per-IP firewall block/unblock with graceful
	// degradation. The TUI's Access screen renders it; nothing else knows
	// about the other.
	Access *Access
	// Logs is the journal read/filter surface (#8): snapshot, live-tail poll,
	// and time-range queries over the unit's journal (nil for tmux setups,
	// which have no unit). The TUI's Logs screen renders it; nothing else
	// knows about the other. Reader-only — never writes or rotates logs.
	Logs *Logs
	// RawConsole is the verbatim console surface (#12): one command at a time,
	// sent to the server exactly as typed and audit-logged with verb "console".
	// The TUI's Console screen renders it; nothing else knows about the other.
	// Built even when no channel resolved — Send then degrades to a clean error
	// (still audited) rather than panicking.
	RawConsole *RawConsole
	// Props is the server.properties surface (#11): read the keys with
	// hot/cold classification and per-key allowed-values help, validate
	// values, write atomically with before/after audit, apply hot keys live
	// through the console (setmaxplayers / changesetting), and restart the
	// server so queued cold changes take effect. The TUI's Config screen
	// renders it; nothing else knows about the other.
	Props *Props
}

// StackOpts are the options for NewStack.
type StackOpts struct {
	// ConfigPath is the config file path the timer's backup-now service
	// will read. Empty = the timer service will run without --config
	// (it then reads the standard path).
	ConfigPath string
	// BinaryPath is the absolute path to the bedrock-compass binary the
	// timer's service will exec. Empty = the current executable's path
	// (os.Executable), or a default when that fails.
	BinaryPath string
	// TimerUnit pins the timer's unit base name. Empty =
	// DefaultTimerName.
	TimerUnit string
}

// NewStack resolves the config into a full Stack. version is the server
// version recorded in manifests ("" becomes "unknown" by the engine).
// A nil cfg uses the documented defaults.
func NewStack(ctx context.Context, cfg *Config, version string, opts StackOpts) (*Stack, error) {
	if cfg == nil {
		cfg = defaults()
	}

	// Server dir: explicit wins, else autodiscover from the running process.
	if cfg.ServerDir == "" {
		if d, derr := Discover(ctx); derr == nil {
			cfg.ServerDir = d.Dir
		}
	}

	// Port: explicit config wins; otherwise read server-port= from the
	// resolved server dir (the documented "0 = autodiscover" behavior). A
	// missing/unreadable file leaves 0, and consumers fall back to the BDS
	// default port.
	if cfg.Port <= 0 && cfg.ServerDir != "" {
		cfg.Port = portFromProperties(cfg.ServerDir)
	}

	sup, console, unit := resolveChannels(ctx, cfg)

	var trail *Trail
	if p := cfg.AuditFile; p != "" {
		if t, terr := NewTrail(p); terr == nil {
			trail = t
		}
	}

	engine := NewEngine(cfg, console, sup, trail, version)

	// The lifecycle service shares the supervisor + console + trail. Its
	// adopt dependencies default to production wiring (NewLifecycle);
	// tests that need offline adopt build a Lifecycle directly.
	lifecycle := NewLifecycle(cfg, sup, console, trail)

	// The journal is only meaningful for systemd deployments (the unit's
	// log). tmux setups have no unit: Players.Recent degrades to an error
	// the screen surfaces as "unavailable" rather than a broken read.
	var journal *Journal
	if unit != "" {
		journal = &Journal{Unit: unit}
	}

	// The players service shares the console + trail (+ journal when one
	// exists). The TUI's Players screen renders it; nothing else knows
	// about the other.
	players := NewPlayers(console, trail, journal)

	// The access service (#7) needs the server dir (allowlist.json +
	// server.properties), the console channel (live gate flip + reload), and
	// the audit trail. It is built even when the console is nil: the file
	// operations still work, and live-apply degrades to a note.
	access := NewAccess(cfg, console, trail)

	// The logs service (#8) reads the unit's journal (systemd deployments
	// only; tmux has no unit). It is a pure reader — it shares nothing
	// mutable with the other services.
	var logs *Logs
	if journal != nil {
		logs = &Logs{Journal: journal}
	}

	// The raw console service (#12) shares the console channel + trail. It is
	// built even when no channel resolved (console == nil): Send then degrades
	// to a clean, audited error instead of panicking — a misconfigured surface
	// still leaves a trace in the audit trail.
	rawConsole := NewRawConsole(console, trail)

	// The props service (#11) needs the server dir (server.properties), the
	// console channel (hot apply), the audit trail, and the lifecycle (the
	// restart that makes queued cold changes take effect). It is built even
	// when the console is nil: reads, validation, and cold writes still work.
	props := NewProps(cfg, console, trail, lifecycle)

	bin := opts.BinaryPath
	if bin == "" {
		if p, err := os.Executable(); err == nil {
			if abs, aerr := filepath.Abs(p); aerr == nil {
				bin = abs
			} else {
				bin = p
			}
		}
		if bin == "" {
			bin = "/usr/local/bin/bedrock-compass"
		}
	}

	timerUnit := opts.TimerUnit
	if timerUnit == "" {
		timerUnit = DefaultTimerName
	}

	timer := &Timer{
		Unit:       timerUnit,
		Config:     opts.ConfigPath,
		BinaryPath: bin,
	}

	return &Stack{
		Cfg:        cfg,
		Console:    console,
		Sup:        sup,
		Trail:      trail,
		Engine:     engine,
		Timer:      timer,
		Journal:    journal,
		Lifecycle:  lifecycle,
		Players:    players,
		Access:     access,
		Logs:       logs,
		RawConsole: rawConsole,
		Props:      props,
	}, nil
}

// resolveChannels resolves the supervisor, the console channel, and the
// unit name for the configured supervisor kind.
//
//   - systemd/auto: the supervisor is `systemctl start/stop/restart` on the
//     unit (cfg.Service, or discovered from the running process); the console
//     is the unit's console FIFO (ADR-0001).
//   - tmux: the supervisor is the console `stop`/`start` commands (ticket
//     #13); the console is the pane. No unit is involved.
//
// A nil supervisor (no unit, no pane) still yields a usable Stack: status
// degrades to process-table truth and lifecycle actions surface a clear
// "no supervisor resolved" error rather than a broken one.
func resolveChannels(ctx context.Context, cfg *Config) (sup *Supervisor, console *Console, unit string) {
	unit = cfg.Service
	switch cfg.Supervisor {
	case "tmux":
		if cfg.Pane == "" {
			return
		}
		console = NewTmuxConsole(cfg.Pane, ConsolePolicy{})
		sup = NewTmuxSupervisor(console, nil)
		return
	case "systemd", "auto":
	default:
		return
	}
	if unit == "" {
		if d, err := Discover(ctx); err == nil {
			unit = d.Unit
		}
	}
	if unit != "" {
		sup = NewSystemdSupervisor(unit, nil)
		fif := cfg.ConsoleFif
		if fif == "" {
			fif = "/run/bedrock-console.fifo"
		}
		console = NewSystemdConsole(fif, unit, ConsolePolicy{})
	}
	return
}

// BackupNow runs one backup in the given mode and returns the backup id.
// It is the single execution path for both the interactive screen and the
// headless / scheduled `backup-now` verb.
func (s *Stack) BackupNow(ctx context.Context, mode string) (string, error) {
	if s.Engine == nil {
		return "", errors.New("no engine (config did not resolve)")
	}
	var (
		b   *Backup
		err error
	)
	switch mode {
	case ModeLive:
		b, err = s.Engine.LiveBackup(ctx)
	case ModeStopped:
		b, err = s.Engine.StoppedBackup(ctx)
	default:
		return "", fmt.Errorf("backup-now: mode must be 'live' or 'stopped', got %q", mode)
	}
	if err != nil {
		return "", err
	}
	return b.ID, nil
}

// portFromProperties reads server-port= from dir/server.properties and returns
// it (0 when the file or key is absent/unparseable). This implements the
// documented "Port 0 = autodiscover" behavior at wiring time so every consumer
// (telemetry, lifecycle, access firewall commands) targets the port the server
// actually listens on instead of silently falling back to the BDS default.
func portFromProperties(dir string) int {
	data, err := os.ReadFile(filepath.Join(dir, "server.properties"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		v, ok := cutKey(t, "server-port=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return 0
		}
		return n
	}
	return 0
}
