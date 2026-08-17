package compass

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

	sup, unit := resolveSupervisor(ctx, cfg)

	fif := cfg.ConsoleFif
	if fif == "" {
		fif = "/run/bedrock-console.fifo"
	}
	console := NewSystemdConsole(fif, unit, ConsolePolicy{})

	var trail *Trail
	if p := cfg.AuditFile; p != "" {
		if t, terr := NewTrail(p); terr == nil {
			trail = t
		}
	}

	engine := NewEngine(cfg, console, sup, trail, version)

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
		Cfg:     cfg,
		Console: console,
		Sup:     sup,
		Trail:   trail,
		Engine:  engine,
		Timer:   timer,
	}, nil
}

// resolveSupervisor returns the supervisor + its unit name. For "systemd"
// the unit is cfg.Service (or discovered). For "tmux" the supervisor is nil
// in v1: the tmux supervisor's stop/start (the console send-keys channel) is
// ticket #13's scope, so a tmux deployment has no supervisor for
// stopped-backup/restore in this ticket — those actions surface a clear
// "not available for tmux deployments yet" error rather than a broken one.
func resolveSupervisor(ctx context.Context, cfg *Config) (sup *Supervisor, unit string) {
	unit = cfg.Service
	switch cfg.Supervisor {
	case "systemd", "auto":
		if unit == "" {
			if d, err := Discover(ctx); err == nil {
				unit = d.Unit
			}
		}
		if unit != "" {
			sup = NewSystemdSupervisor(unit, nil)
		}
		return
	case "tmux":
		// No supervisor in v1 (ticket #13). unit is left empty so the
		// console FIFO path is not attempted for a tmux deployment.
		return
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
