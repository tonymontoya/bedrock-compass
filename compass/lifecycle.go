package compass

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Lifecycle is the server lifecycle + adopt service (ticket #13). It is the
// shared operations surface for the TUI's Server screen (#13) and the
// headless start/stop/restart/status/adopt verbs (#15): one code path,
// nothing TUI-aware (D15).
//
// It is a thin service over the existing seams (Supervisor, Console,
// Pinger, Discover) and mirrors the Engine's pattern: the function fields
// keep the adopt path testable offline (tests inject fakes; production
// wiring is the real Discover/Pinger/Console), and every mutating action
// is audit-logged through the Trail (D11).
type Lifecycle struct {
	Cfg     *Config
	Sup     *Supervisor
	Console *Console
	Trail   *Trail

	// Adopt dependencies. Nil fields take the production wiring.
	Discover     func(ctx context.Context) (*Discovered, error)
	Ping         func(ctx context.Context, addr string) (Pong, error)
	ConsoleCheck func(ctx context.Context, c *Console) (string, error)
}

// NewLifecycle builds the service over an already-resolved supervisor and
// console (from NewStack), with the production adopt dependencies.
func NewLifecycle(cfg *Config, sup *Supervisor, console *Console, trail *Trail) *Lifecycle {
	return &Lifecycle{
		Cfg:          cfg,
		Sup:          sup,
		Console:      console,
		Trail:        trail,
		Discover:     Discover,
		Ping:         defaultPing,
		ConsoleCheck: defaultConsoleCheck,
	}
}

// audit appends one trail event, best effort (a missing or unwritable trail
// must not break a lifecycle action — the action's own result is what
// matters). Mirrors Engine.audit.
func (l *Lifecycle) audit(action string, result string, dur time.Duration) {
	if l.Trail == nil {
		return
	}
	_ = l.Trail.Append(Event{Verb: "lifecycle", Params: map[string]string{"action": action}, Result: result, Duration: dur})
}

// Start begins the server through the supervisor. Audited.
func (l *Lifecycle) Start(ctx context.Context) error {
	if l.Sup == nil {
		return fmt.Errorf("no supervisor resolved (the deployment's supervisor is unknown)")
	}
	start := time.Now()
	err := l.Sup.Start(ctx)
	if err != nil {
		l.audit("start", "error: "+err.Error(), time.Since(start))
		return err
	}
	l.audit("start", "ok", time.Since(start))
	return nil
}

// Stop halts the server through the supervisor. Audited.
func (l *Lifecycle) Stop(ctx context.Context) error {
	if l.Sup == nil {
		return fmt.Errorf("no supervisor resolved (the deployment's supervisor is unknown)")
	}
	start := time.Now()
	err := l.Sup.Stop(ctx)
	if err != nil {
		l.audit("stop", "error: "+err.Error(), time.Since(start))
		return err
	}
	l.audit("stop", "ok", time.Since(start))
	return nil
}

// Restart stops then starts the server. Audited as a single "restart"
// action. If stop succeeds but start fails, the operator is left with a
// stopped server and a clear error (they can then start it manually).
// Error strings name the failing step ("stop failed:" / "start failed:")
// but carry no "restart:" prefix of their own: both callers (the TUI's
// Server screen toast and the headless verb) prepend the action label, so
// a self-prefix would render as "restart: restart: …".
func (l *Lifecycle) Restart(ctx context.Context) error {
	if l.Sup == nil {
		return fmt.Errorf("no supervisor resolved (the deployment's supervisor is unknown)")
	}
	start := time.Now()
	if err := l.Sup.Stop(ctx); err != nil {
		l.audit("restart", "error: stop: "+err.Error(), time.Since(start))
		return fmt.Errorf("stop failed: %w", err)
	}
	if err := l.Sup.Start(ctx); err != nil {
		l.audit("restart", "error: start: "+err.Error(), time.Since(start))
		return fmt.Errorf("start failed: %w (the server is stopped; start it manually)", err)
	}
	l.audit("restart", "ok", time.Since(start))
	return nil
}

// LifecycleStatus is the server's lifecycle state as the Server screen
// displays it: the supervisor's truth (active, main PID, uptime) plus the
// read-only pong probe. Zero/empty fields mean "unknown / not wired".
type LifecycleStatus struct {
	State  string // "running" | "stopped" | "unknown"
	Source string // "systemd" | "tmux" | "process" | ""
	Unit   string // systemd unit ("" when not systemd)
	PID    int    // main pid (0 = not running / unknown)
	Uptime time.Duration
	Pong   *Pong // nil when the probe is unavailable or failed
}

// Status reports the supervisor's truth and probes the server read-only.
// It degrades gracefully: no supervisor -> process-table truth; no pong ->
// the supervisor truth alone.
func (l *Lifecycle) Status(ctx context.Context) LifecycleStatus {
	st := LifecycleStatus{State: "unknown"}

	if l.Sup != nil && l.Sup.Status != nil {
		ps, err := l.Sup.Status(ctx)
		if err == nil {
			st.Source = ps.Source
			st.Unit = ps.Unit
			st.PID = ps.PID
			st.Uptime = ps.Uptime
			if ps.Active {
				st.State = "running"
			} else {
				st.State = "stopped"
			}
		} else {
			// The supervisor is wired but its status source failed (e.g.
			// systemctl is unavailable). Degrade to process truth.
			if pid, perr := ServerProcessPID(ctx); perr == nil && pid > 0 {
				st.State = "running"
				st.PID = pid
				st.Source = "process"
				if up, ok := processUptime(pid); ok {
					st.Uptime = up
				}
			}
		}
	} else if pid, err := ServerProcessPID(ctx); err == nil && pid > 0 {
		// No supervisor (e.g. a tmux deployment): process-table truth.
		st.State = "running"
		st.PID = pid
		st.Source = "process"
		if up, ok := processUptime(pid); ok {
			st.Uptime = up
		}
	}

	if l.Ping != nil {
		addr := pingAddr(l.Cfg)
		if pong, err := l.Ping(ctx, addr); err == nil {
			st.Pong = &pong
		}
	}
	return st
}

// Adopt points the tool at a running deployment it does not yet know,
// verifies it, and returns the result to persist (the operator's own case,
// CONTEXT "adopt"). It never writes the config itself — the caller decides
// where to persist via PersistAdopt — so the tool can verify without
// committing.
//
// Verification is mandatory (ticket #13 acceptance): both the pong probe
// and a console round-trip must succeed against the *discovered*
// deployment's own channel before the association is returned. A host
// that blocks either endpoint gets a clear, loud failure — not a silent
// degrade — because a half-verified adoption would leave the operator
// claiming a deployment the tool may not be able to drive.
//
// Error strings name the failing step ("discover failed:", "pong …",
// "console …") but carry no "adopt:" prefix of their own: both callers
// (the TUI's Server screen toast and the headless verb) prepend the action
// label, so a self-prefix would render as "adopt: adopt: …".
func (l *Lifecycle) Adopt(ctx context.Context) (*Adopted, error) {
	disc, err := l.Discover(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover failed: %w", err)
	}

	port := l.Cfg.Port
	if port <= 0 {
		port = 19132
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	pong, err := l.Ping(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("pong %s: %w", addr, err)
	}

	console, err := adoptConsole(ctx, disc, l.Cfg)
	if err != nil {
		return nil, fmt.Errorf("console: %w", err)
	}
	out, err := l.ConsoleCheck(ctx, console)
	if err != nil {
		return nil, fmt.Errorf("console round-trip: %w", err)
	}
	if !containsPlayersLine(out) {
		return nil, fmt.Errorf("console round-trip did not return a roster line (got %q)", truncate(out, 80))
	}

	return &Adopted{
		Discovery:  *disc,
		Supervisor: "systemd",
		Service:    disc.Unit,
		ServerDir:  disc.Dir,
		Pane:       l.Cfg.Pane,
		Port:       port,
		ConsoleFif: defaultFIFOFor(ctx, disc.Unit),
		Pong:       pong,
		Console:    out,
	}, nil
}

// Adopted is the result of a successful Adopt: the deployment the tool has
// verified (pong + console round-trip) and is ready to claim.
type Adopted struct {
	Discovery  Discovered
	Supervisor string
	Service    string
	ServerDir  string
	Pane       string
	Port       int
	ConsoleFif string
	Pong       Pong // the pong answer (always present: it is a verify gate)
	Console    string
}

// PersistAdopt writes the adopted association into the config at path
// (creating the directory), so the deployment is the one the tool manages
// from now on. The existing config's non-adopted fields (backup dir,
// retention, audit file, packs) are preserved.
func (l *Lifecycle) PersistAdopt(path string, a *Adopted) error {
	cfg := l.Cfg
	if cfg == nil {
		cfg = defaults()
	}
	cfg.ServerDir = a.ServerDir
	cfg.Supervisor = a.Supervisor
	cfg.Service = a.Service
	cfg.Pane = a.Pane
	if a.Port > 0 {
		cfg.Port = a.Port
	}
	if a.ConsoleFif != "" {
		cfg.ConsoleFif = a.ConsoleFif
	}
	return cfg.Save(path)
}

// ---- production adopt dependencies -------------------------------------

func defaultPing(ctx context.Context, addr string) (Pong, error) {
	return NewPinger(addr).Ping(ctx)
}

func defaultConsoleCheck(ctx context.Context, c *Console) (string, error) {
	return c.Send(ctx, "list")
}

// containsPlayersLine reports whether the console output is a roster line
// (Bedrock's "list" reply always ends in "… players online").
func containsPlayersLine(out string) bool {
	return strings.Contains(strings.ToLower(out), "players online")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// pingAddr returns the pong probe address from the config (127.0.0.1:port,
// defaulting port to 19132 when unset).
func pingAddr(cfg *Config) string {
	port := 19132
	if cfg != nil && cfg.Port > 0 {
		port = cfg.Port
	}
	return "127.0.0.1:" + strconv.Itoa(port)
}

// adoptConsole builds the console channel for the discovered deployment:
// the systemd unit's console FIFO (ADR-0001) when a unit was found, else
// the config's tmux pane.
func adoptConsole(ctx context.Context, disc *Discovered, cfg *Config) (*Console, error) {
	fif := defaultFIFOFor(ctx, disc.Unit)
	if disc.Unit != "" {
		return NewSystemdConsole(fif, disc.Unit, ConsolePolicy{Timeout: 10 * time.Second}), nil
	}
	if cfg != nil && cfg.Pane != "" {
		return NewTmuxConsole(cfg.Pane, ConsolePolicy{Timeout: 10 * time.Second}), nil
	}
	return nil, fmt.Errorf("no unit or pane to verify over (the deployment is not under systemd or a known tmux pane)")
}

// defaultFIFOFor returns the console FIFO for a systemd unit, derived from
// its <unit>.socket's ListenFIFO when present, else the documented default.
func defaultFIFOFor(ctx context.Context, unit string) string {
	if unit != "" {
		out, err := Exec(ctx, "systemctl", "cat", unit+".socket")
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ListenFIFO="); ok {
					return v
				}
			}
		}
	}
	return "/run/bedrock-console.fifo"
}
