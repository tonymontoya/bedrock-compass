// Package compass: Timer owns the scheduled-backup mechanism (ticket #10).
//
// It is TUI-free (D15) — the TUI's Backups screen calls these methods and
// renders their results. The systemd units live under the user's systemd
// config (no sudo, D11): a service that runs the headless `backup-now` verb
// and a timer that triggers it on the configured cadence.
package compass

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Schedule describes a timer's cadence + mode (the operator's choice).
//
// Cadence is "daily" or "weekly" (anchored calendar time, not an interval).
// Weekday is 0-6 (Monday=0) when Cadence=="weekly"; ignored otherwise.
// Mode is "live" or "stopped".
type Schedule struct {
	Cadence string // "daily" | "weekly"
	Time    string // "HH:MM"
	Weekday int    // 0=Mon .. 6=Sun; valid only when Cadence=="weekly"
	Mode    string // "live" | "stopped"
}

func (s Schedule) Validate() error {
	switch s.Cadence {
	case "daily":
	case "weekly":
		if s.Weekday < 0 || s.Weekday > 6 {
			return fmt.Errorf("schedule: weekday must be 0-6 for weekly, got %d", s.Weekday)
		}
	default:
		return fmt.Errorf("schedule: cadence must be 'daily' or 'weekly', got %q", s.Cadence)
	}
	if len(s.Time) != 5 || s.Time[2] != ':' {
		return fmt.Errorf("schedule: time must be HH:MM, got %q", s.Time)
	}
	hh, err := strconv.Atoi(s.Time[:2])
	if err != nil || hh < 0 || hh > 23 {
		return fmt.Errorf("schedule: hour must be 00-23, got %q", s.Time[:2])
	}
	mm, err := strconv.Atoi(s.Time[3:])
	if err != nil || mm < 0 || mm > 59 {
		return fmt.Errorf("schedule: minute must be 00-59, got %q", s.Time[3:])
	}
	switch s.Mode {
	case ModeLive, ModeStopped:
	default:
		return fmt.Errorf("schedule: mode must be 'live' or 'stopped', got %q", s.Mode)
	}
	return nil
}

// TimerStatus is the read-only report from Timer.Status.
type TimerStatus struct {
	Loaded  bool   // unit files present and loaded into systemd
	Enabled bool   // timer will start at boot / next matching cadence
	Active  bool   // timer is currently running
	NextRun string // human-readable next run time ("", when unknown / not loaded)
}

// Timer owns the scheduled-backup systemd units. All methods are safe to
// call from the TUI (no blocking reads) and from the headless path.
type Timer struct {
	Unit       string // unit name base; default "bedrock-compass-backup"
	Config     string // path to the config file the backup-now service will read
	Exec       ExecFunc
	ConfigDir  string // where the unit files live; default ~/.config/systemd/user
	BinaryPath string // absolute path to the bedrock-compass binary
}

// DefaultTimerName is the default unit base name.
const DefaultTimerName = "bedrock-compass-backup"

func (t *Timer) unit() string {
	if t.Unit == "" {
		return DefaultTimerName
	}
	return t.Unit
}

func (t *Timer) configDir() string {
	if t.ConfigDir != "" {
		return t.ConfigDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user")
}

func (t *Timer) binaryPath() string {
	if t.BinaryPath != "" {
		return t.BinaryPath
	}
	return "/usr/local/bin/bedrock-compass"
}

func (t *Timer) exec() ExecFunc {
	if t.Exec != nil {
		return t.Exec
	}
	return Exec
}

// Create writes the service + timer unit files, daemon-reloads the user
// manager, and enables + starts the timer. Returns the timer's status after
// the change. It is idempotent: re-running overwrites the unit files and
// re-enables (a no-op if already enabled).
func (t *Timer) Create(ctx context.Context, sched Schedule) (*TimerStatus, error) {
	if err := sched.Validate(); err != nil {
		return nil, err
	}
	dir := t.configDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create timer config dir %s: %w", dir, err)
	}
	svc := filepath.Join(dir, t.unit()+".service")
	tm := filepath.Join(dir, t.unit()+".timer")

	svcBody := renderServiceUnit(t.binaryPath(), t.Config, sched.Mode)
	tmBody := renderTimerUnit(sched)
	if err := os.WriteFile(svc, []byte(svcBody), 0o644); err != nil {
		return nil, fmt.Errorf("write service unit %s: %w", svc, err)
	}
	if err := os.WriteFile(tm, []byte(tmBody), 0o644); err != nil {
		return nil, fmt.Errorf("write timer unit %s: %w", tm, err)
	}

	exec := t.exec()
	if _, err := exec(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return nil, fmt.Errorf("systemctl --user daemon-reload: %w (run it yourself if the tool lacks permission)", err)
	}
	if _, err := exec(ctx, "systemctl", "--user", "enable", t.unit()+".timer"); err != nil {
		return nil, fmt.Errorf("systemctl --user enable %s.timer: %w (run it yourself if the tool lacks permission)", t.unit(), err)
	}
	if _, err := exec(ctx, "systemctl", "--user", "start", t.unit()+".timer"); err != nil {
		return nil, fmt.Errorf("systemctl --user start %s.timer: %w (run it yourself if the tool lacks permission)", t.unit(), err)
	}
	return t.Status(ctx)
}

// Disable stops and disables the timer (the unit files stay in place;
// Remove is how you also delete them).
func (t *Timer) Disable(ctx context.Context) error {
	exec := t.exec()
	if _, err := exec(ctx, "systemctl", "--user", "stop", t.unit()+".timer"); err != nil {
		return fmt.Errorf("systemctl --user stop %s.timer: %w (run it yourself if the tool lacks permission)", t.unit(), err)
	}
	if _, err := exec(ctx, "systemctl", "--user", "disable", t.unit()+".timer"); err != nil {
		return fmt.Errorf("systemctl --user disable %s.timer: %w (run it yourself if the tool lacks permission)", t.unit(), err)
	}
	return nil
}

// Remove disables the timer and deletes the unit files.
func (t *Timer) Remove(ctx context.Context) error {
	// Disable first (ignore error: a fresh host may have no timer yet).
	_ = t.Disable(ctx)
	exec := t.exec()
	if _, err := exec(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w (run it yourself if the tool lacks permission)", err)
	}
	dir := t.configDir()
	for _, p := range []string{filepath.Join(dir, t.unit()+".service"), filepath.Join(dir, t.unit()+".timer")} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove unit file %s: %w", p, err)
		}
	}
	return nil
}

// Status reports whether the timer is loaded / enabled / active, and the
// next scheduled run. A host without the units yet returns a zero
// TimerStatus with no error.
func (t *Timer) Status(ctx context.Context) (*TimerStatus, error) {
	st := &TimerStatus{}
	exec := t.exec()
	loaded, _ := exec(ctx, "systemctl", "--user", "is-loaded", t.unit()+".timer")
	if strings.TrimSpace(string(loaded)) != "loaded" {
		return st, nil
	}
	st.Loaded = true
	enabled, _ := exec(ctx, "systemctl", "--user", "is-enabled", t.unit()+".timer")
	st.Enabled = strings.TrimSpace(string(enabled)) == "enabled"
	active, _ := exec(ctx, "systemctl", "--user", "is-active", t.unit()+".timer")
	st.Active = strings.TrimSpace(string(active)) == "active"
	if out, err := exec(ctx, "systemctl", "--user", "list-timers", "--no-legend", "--no-pager", t.unit()+".timer"); err == nil {
		// Columns: NEXT LEFT LAST UNIT ACTIVATES DROP-IN STATE
		fields := strings.Fields(string(out))
		if len(fields) >= 2 {
			st.NextRun = fields[0]
		}
	}
	return st, nil
}

// renderServiceUnit is pure: it produces the service unit body.
func renderServiceUnit(binary, config, mode string) string {
	// Runs as the current user's systemd manager, so it inherits HOME,
	// PATH, and XDG_RUNTIME_DIR from the login session (the user manager
	// forwards the ambient environment to oneshot services). We do NOT set
	// Environment= here: a stale value would clobber the real HOME.
	return `[Unit]
Description=bedrock-compass scheduled backup (mode=` + mode + `)

[Service]
Type=oneshot
ExecStart=` + binary + ` backup-now --mode ` + mode + ` --config ` + config + `
`
}

// renderTimerUnit is pure: it produces the timer unit body.
func renderTimerUnit(sched Schedule) string {
	var on string
	if sched.Cadence == "weekly" {
		day := "Mon"
		switch sched.Weekday {
		case 0:
			day = "Mon"
		case 1:
			day = "Tue"
		case 2:
			day = "Wed"
		case 3:
			day = "Thu"
		case 4:
			day = "Fri"
		case 5:
			day = "Sat"
		case 6:
			day = "Sun"
		}
		on = day + ` *-*-* ` + sched.Time + `:00`
	} else {
		on = `*-*-* ` + sched.Time + `:00`
	}
	return `[Unit]
Description=bedrock-compass scheduled backup (cadence=` + sched.Cadence + `, mode=` + sched.Mode + `)

[Timer]
OnCalendar=` + on + `
Persistent=true

[Install]
WantedBy=timers.target
`
}
