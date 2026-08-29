package compass

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ProcessStatus is the supervisor's truth about the server process: is it
// active, what is its main pid, and how long has it been running. It is the
// authoritative source for the Server screen's State/Uptime/PID rows
// (ticket #13) — distinct from the telemetry Status (a display snapshot).
// Zero values mean "unknown".
type ProcessStatus struct {
	Active bool
	Unit   string
	PID    int
	Uptime time.Duration
	Source string // "systemd" or "tmux" (or "" if neither)
}

// Supervisor stops and starts the server process. The backup engine uses
// it for stop-and-copy backups and for restore; the TUI's Server screen
// (ticket #13) and the headless start/stop/restart verbs use the same seam.
//
// The function fields keep the seam testable offline: production wiring
// goes through ExecFunc and the console channel, tests inject fakes.
type Supervisor struct {
	// Poll tunes the process-exit wait loop in tmux Stop. Zero takes the
	// default (250 ms).
	Poll time.Duration

	Stop    func(ctx context.Context) error
	Start   func(ctx context.Context) error
	Running func(ctx context.Context) (bool, error)
	Status  func(ctx context.Context) (ProcessStatus, error)
}

// Restart stops then starts the server. The two steps are sequential
// because a server cannot be restarted while it is still saving its world
// to disk. If Stop succeeds but Start fails, the caller is responsible
// for surfacing the failure (the operator can then Start manually).
func (s *Supervisor) Restart(ctx context.Context) error {
	if err := s.Stop(ctx); err != nil {
		return fmt.Errorf("restart: stop: %w", err)
	}
	if err := s.Start(ctx); err != nil {
		return fmt.Errorf("restart: start: %w", err)
	}
	return nil
}

// statusUnknown is returned when the supervisor's status source is not
// wired (e.g. a tmux deployment where the process is not running).
var statusUnknown = ProcessStatus{}

func (s *Supervisor) poll() time.Duration {
	if s.Poll > 0 {
		return s.Poll
	}
	return 250 * time.Millisecond
}

// NewSystemdSupervisor wires a server under systemd. Stop/Start use
// systemctl as the current user (no sudo, D11 pattern: a permission
// failure surfaces the exact command to the operator).
func NewSystemdSupervisor(unit string, exec ExecFunc) *Supervisor {
	if exec == nil {
		exec = Exec
	}
	return &Supervisor{
		Stop: func(ctx context.Context) error {
			if _, err := exec(ctx, "systemctl", "stop", unit); err != nil {
				return fmt.Errorf("systemctl stop %s: %w (run it yourself if the tool lacks permission)", unit, err)
			}
			return nil
		},
		Start: func(ctx context.Context) error {
			if _, err := exec(ctx, "systemctl", "start", unit); err != nil {
				return fmt.Errorf("systemctl start %s: %w (run it yourself if the tool lacks permission)", unit, err)
			}
			return nil
		},
		Running: func(ctx context.Context) (bool, error) {
			out, _ := exec(ctx, "systemctl", "is-active", unit)
			// is-active exits non-zero when the unit is not active; an
			// inactive unit is a normal state, not an error.
			return strings.TrimSpace(string(out)) == "active", nil
		},
		Status: func(ctx context.Context) (ProcessStatus, error) {
			return systemdStatus(ctx, unit, exec)
		},
	}
}

// systemdStatus reads the unit's active state, main pid, and uptime from
// systemctl. An inactive unit is a valid (not-active) result, not an error.
// The ActiveEnterTimestampMonotonic field is the kernel monotonic clock at
// the moment the unit entered "active", which gives an accurate uptime
// independent of wall-clock adjustments. Note: systemd reports this value in
// MICROSECONDS (like every *TimestampMonotonic property), not nanoseconds —
// treating it as ns inflates the displayed uptime by 1000x.
func systemdStatus(ctx context.Context, unit string, exec ExecFunc) (ProcessStatus, error) {
	out, err := exec(ctx, "systemctl", "show", unit,
		"-p", "ActiveState",
		"-p", "MainPID",
		"-p", "ActiveEnterTimestampMonotonic",
	)
	if err != nil {
		return ProcessStatus{}, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	var st ProcessStatus
	st.Unit = unit
	st.Source = "systemd"
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "ActiveState":
			st.Active = strings.TrimSpace(v) == "active"
		case "MainPID":
			st.PID, _ = strconv.Atoi(strings.TrimSpace(v))
		case "ActiveEnterTimestampMonotonic":
			// systemd reports this in microseconds (CLOCK_MONOTONIC).
			monotonicUS, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if monotonicUS > 0 {
				// The host's monotonic clock at the moment of the call.
				if nowNS, ok := monotonicNowNS(); ok {
					st.Uptime = time.Duration(nowNS-monotonicUS*1000) * time.Nanosecond
					if st.Uptime < 0 {
						st.Uptime = 0
					}
				}
			}
		}
	}
	return st, nil
}

// monotonicNowNS returns the current host monotonic clock in nanoseconds
// (Linux: CLOCK_MONOTONIC via /proc/uptime is not precise; we use the
// syscall clock_gettime wrapper). Fallback: /proc/uptime.
func monotonicNowNS() (int64, bool) {
	// /proc/uptime gives seconds since boot with 2-decimal precision —
	// sufficient for uptime display (±10 ms).
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, false
	}
	sec, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return int64(sec * 1e9), true
}

// ErrTmuxStartDeferred is returned by the tmux supervisor's Start: starting
// a tmux-hosted server requires the operator's exact launch command, which
// the tool does not know. The operator starts the server in the pane
// manually; the tool manages it via the console channel.
var ErrTmuxStartDeferred = errors.New("starting a tmux-hosted server is not supported: start it in the tmux pane manually, then the tool will manage it via the console")

// NewTmuxSupervisor wires a server running in a tmux pane. Stop types the
// console "stop" command and waits for the process to exit; Status reads
// the process table for the bedrock_server pid and uptime. Start is
// deferred (the operator must launch the server in the pane manually).
func NewTmuxSupervisor(console *Console, processAlive func(ctx context.Context) (bool, error)) *Supervisor {
	if processAlive == nil {
		processAlive = ServerProcessAlive
	}
	s := &Supervisor{}
	s.Stop = func(ctx context.Context) error {
		if _, err := console.Send(ctx, "stop"); err != nil {
			return fmt.Errorf("console stop: %w", err)
		}
		for {
			alive, err := processAlive(ctx)
			if err != nil {
				return fmt.Errorf("wait for server exit: %w", err)
			}
			if !alive {
				return nil
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("server did not exit after the stop command: %w", ctx.Err())
			case <-time.After(s.poll()):
			}
		}
	}
	s.Start = func(ctx context.Context) error { return ErrTmuxStartDeferred }
	s.Running = processAlive
	s.Status = func(ctx context.Context) (ProcessStatus, error) {
		return tmuxStatus(ctx)
	}
	return s
}

// tmuxStatus reads the process table for the bedrock_server pid and
// computes uptime from /proc/<pid>/stat (field 22: start time in clock
// ticks since boot).
func tmuxStatus(ctx context.Context) (ProcessStatus, error) {
	procs, err := procsFromProc(ctx)
	if err != nil {
		return ProcessStatus{}, fmt.Errorf("read process table: %w", err)
	}
	var pid int
	for _, p := range procs {
		if p.Name == serverProcName {
			pid = p.PID
			break
		}
	}
	if pid == 0 {
		return ProcessStatus{Source: "tmux"}, nil
	}
	st := ProcessStatus{Active: true, PID: pid, Source: "tmux"}
	if up, ok := processUptime(pid); ok {
		st.Uptime = up
	}
	return st, nil
}

// processUptime returns the uptime of a pid, computed from /proc/<pid>/stat
// field 22 (start time in clock ticks since boot). Returns false when the
// field is unavailable.
func processUptime(pid int) (time.Duration, bool) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	// The comm field (field 2) may contain spaces or parens; skip past
	// the closing paren to find field 3 onward.
	s := string(b)
	idx := strings.LastIndex(s, ")")
	if idx < 0 {
		return 0, false
	}
	// After the closing paren, fields are space-separated starting at
	// field 3 (state). Field 22 (starttime) is the 20th field after
	// the paren (field 3 = index 0 → field 22 = index 19).
	fields := strings.Fields(s[idx+1:])
	if len(fields) < 20 {
		return 0, false
	}
	startTicks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, false
	}
	// clk_tck is typically 100 on Linux; read it from the sysconf
	// equivalent. /proc/uptime gives seconds since boot.
	clkTck := uint64(100) // Linux default; accurate enough for display
	startSecs := time.Duration(startTicks/clkTck) * time.Second
	nowNS, ok := monotonicNowNS()
	if !ok {
		return 0, false
	}
	up := time.Duration(nowNS)*time.Nanosecond - startSecs
	if up < 0 {
		return 0, false
	}
	return up, true
}

// ServerProcessAlive reports whether a bedrock_server process is running on
// this host.
func ServerProcessAlive(ctx context.Context) (bool, error) {
	procs, err := procsFromProc(ctx)
	if err != nil {
		return false, fmt.Errorf("read process table: %w", err)
	}
	for _, p := range procs {
		if p.Name == serverProcName {
			return true, nil
		}
	}
	return false, nil
}

// ServerProcessPID returns the pid of the first bedrock_server process
// found, or 0 when none is running.
func ServerProcessPID(ctx context.Context) (int, error) {
	procs, err := procsFromProc(ctx)
	if err != nil {
		return 0, fmt.Errorf("read process table: %w", err)
	}
	for _, p := range procs {
		if p.Name == serverProcName {
			return p.PID, nil
		}
	}
	return 0, nil
}
