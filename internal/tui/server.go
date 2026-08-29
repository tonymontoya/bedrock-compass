// Package tui: the Server screen (ticket #13). It is a thin renderer over
// the compass Lifecycle service (D15): it calls Start/Stop/Restart/Status
// and shows their results, reusing the shell's confirmation tiers and toasts.
// The four mutating actions each confirm at the tier the ticket's acceptance
// criteria name (start = TierEnter; stop / restart / adopt = TierYes, type
// "YES"), run in a goroutine with an indeterminate spinner, and toast the
// result. Status is the supervisor's truth (state, main pid, uptime) plus the
// read-only pong probe, refreshed on demand and on the status poll.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// join renders a list of lines as a single view string.
func join(lines []string) string { return strings.Join(lines, "\n") }

// serverHost is the shell the Server screen talks back to: confirmations
// (Ask) and transient notifications (Toast). *App implements it; a fake in
// tests keeps the screen decoupled from the concrete shell.
type serverHost interface {
	Ask(title, consequence string, tier Tier, target string, onConfirm func() tea.Cmd)
	Toast(msg string, kind ToastKind)
}

// Server is the ticket #13 screen: the supervisor's truth (state, source,
// unit, main pid, uptime), the read-only pong probe, and the start / stop /
// restart / adopt actions.
type Server struct {
	host  serverHost
	stack *compass.Stack
	// configPath is the config file the tool loaded (the --config path, or
	// the standard path). Adopt persists the claimed deployment to *this*
	// file — not always the standard path — so a non-standard config keeps
	// working after an adoption. Empty falls back to compass.StandardPath().
	configPath string

	status    compass.LifecycleStatus
	statusErr string

	// In-flight operation (a blocking lifecycle call running in a
	// goroutine) — same shape as the Backups screen.
	working   bool
	workLabel string
	spin      int
}

// NewServer builds the Server screen over a resolved Stack. A nil stack (or
// one whose wiring did not resolve a lifecycle) renders a degraded state.
func NewServer(host serverHost, stack *compass.Stack) *Server {
	s := &Server{host: host, stack: stack}
	s.refresh()
	return s
}

// SetConfigPath points adopt's persist at the config file the tool loaded
// (wired by cmd/bedrock-compass, which knows the --config path). Without it
// the screen falls back to the standard path.
func (s *Server) SetConfigPath(path string) { s.configPath = path }

// adoptPath returns where an adoption should be persisted: the loaded
// config's path when known, else the standard path.
func (s *Server) adoptPath() string {
	if s.configPath != "" {
		return s.configPath
	}
	return compass.StandardPath()
}

func (s *Server) Tab() string { return "Server" }

func (s *Server) Init() tea.Cmd { return s.ticks() }

// lifecycleOK reports whether the screen can drive a lifecycle action.
func (s *Server) lifecycleOK() bool {
	return s.stack != nil && s.stack.Lifecycle != nil
}

// refresh pulls the supervisor's truth + pong probe. It is read-only and
// cheap; a nil stack/lifecycle leaves the state empty, not an error.
func (s *Server) refresh() {
	if !s.lifecycleOK() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.status = s.stack.Lifecycle.Status(ctx)
	s.statusErr = ""
}

func (s *Server) ticks() tea.Cmd {
	if s.working {
		return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg {
			return serverSpinnerMsg{frame: s.spin}
		})
	}
	return nil
}

// serverSpinnerMsg advances the indeterminate spinner while an operation is
// in flight.
type serverSpinnerMsg struct{ frame int }

// Update handles the spinner tick, the operation-result message, and keys.
func (s *Server) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case serverSpinnerMsg:
		s.spin = (s.spin + 1) % len(backupSpinnerFrames)
		return s, s.ticks()
	case opResultMsg:
		s.working = false
		s.workLabel = ""
		if m.err != "" {
			s.host.Toast(m.label+": "+m.err, ToastError)
		} else {
			kind := ToastSuccess
			if m.detail != "" {
				s.host.Toast(m.label+": "+m.detail, kind)
			} else {
				s.host.Toast(m.label+": ok", kind)
			}
		}
		s.refresh()
		return s, s.ticks()
	case statusMsg:
		// The shell polls the status bar; piggyback it to keep the
		// Server's pong probe current while this tab is active.
		s.refresh()
		return s, s.ticks()
	case tea.KeyMsg:
		if s.working {
			return s, s.ticks() // an op owns the interaction
		}
		return s.updateKey(m)
	}
	return s, s.ticks()
}

func (s *Server) updateKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case isRune(key, 's'):
		s.askStart()
	case isRune(key, 'o'):
		s.askStop()
	case isRune(key, 'r'):
		s.askRestart()
	case isRune(key, 'a'):
		s.askAdopt()
	case isRune(key, 'u'):
		s.refresh()
	}
	return s, s.ticks()
}

func (s *Server) askStart() {
	if !s.lifecycleOK() {
		s.host.Toast("server unavailable (config did not resolve)", ToastError)
		return
	}
	s.host.Ask(
		"Start server",
		"Start the server through its supervisor.",
		TierEnter,
		"",
		func() tea.Cmd {
			s.working = true
			s.workLabel = "starting…"
			return runOp("start", func(ctx context.Context) (string, error) {
				if err := s.stack.Lifecycle.Start(ctx); err != nil {
					return "", err
				}
				return "", nil
			})
		},
	)
}

func (s *Server) askStop() {
	if !s.lifecycleOK() {
		s.host.Toast("server unavailable (config did not resolve)", ToastError)
		return
	}
	s.host.Ask(
		"Stop server",
		"Stop the server through its supervisor. Players are disconnected.",
		TierYes,
		"",
		func() tea.Cmd {
			s.working = true
			s.workLabel = "stopping…"
			return runOp("stop", func(ctx context.Context) (string, error) {
				if err := s.stack.Lifecycle.Stop(ctx); err != nil {
					return "", err
				}
				return "", nil
			})
		},
	)
}

func (s *Server) askRestart() {
	if !s.lifecycleOK() {
		s.host.Toast("server unavailable (config did not resolve)", ToastError)
		return
	}
	s.host.Ask(
		"Restart server",
		"Stop the server, then start it again. Players are disconnected for the duration.",
		TierYes,
		"",
		func() tea.Cmd {
			s.working = true
			s.workLabel = "restarting…"
			return runOp("restart", func(ctx context.Context) (string, error) {
				if err := s.stack.Lifecycle.Restart(ctx); err != nil {
					return "", err
				}
				return "", nil
			})
		},
	)
}

func (s *Server) askAdopt() {
	if !s.lifecycleOK() {
		s.host.Toast("server unavailable (config did not resolve)", ToastError)
		return
	}
	s.host.Ask(
		"Adopt server",
		"Point the tool at the running deployment, verify it (pong + console round-trip, both mandatory), then claim it into the config.",
		TierYes,
		"",
		func() tea.Cmd {
			s.working = true
			s.workLabel = "adopting…"
			return runOp("adopt", func(ctx context.Context) (string, error) {
				a, err := s.stack.Lifecycle.Adopt(ctx)
				if err != nil {
					return "", err
				}
				// Persist the association so the deployment is now the
				// one the tool manages — into the config file the tool
				// loaded (the --config path, or the standard path when
				// none was given), so a non-standard config keeps working.
				if err := s.stack.Lifecycle.PersistAdopt(s.adoptPath(), a); err != nil {
					return "", fmt.Errorf("adopt verified but persist: %w", err)
				}
				return fmt.Sprintf("%s → %s", a.Supervisor, a.Service), nil
			})
		},
	)
}

// View renders the supervisor's truth (state, source, unit, main pid,
// uptime), the pong probe, the key hints, and the working spinner.
func (s *Server) View() string {
	lines := []string{titleStyle.Render("Server"), ""}

	if s.working {
		f := string(backupSpinnerFrames[s.spin%len(backupSpinnerFrames)])
		lines = append(lines, promptStyle.Render("  "+f+" "+s.workLabel)+"…", "")
	}

	if !s.lifecycleOK() {
		lines = append(lines, toastErr.Render("  server unavailable (config did not resolve)"), "")
		lines = append(lines, hintStyle.Render("  s start    o stop    r restart    a adopt"))
		return join(lines)
	}

	// State line, color-coded.
	state := s.status
	var stateLine string
	switch state.State {
	case "running":
		stateLine = "  state   " + stateRun.Render("running")
	case "stopped":
		stateLine = "  state   " + stateStop.Render("stopped")
	default:
		stateLine = "  state   " + stateUnknown.Render("unknown")
	}
	lines = append(lines, stateLine)

	if state.Source != "" {
		lines = append(lines, "  source  "+state.Source)
	}
	if state.Unit != "" {
		lines = append(lines, "  unit    "+state.Unit)
	}
	if state.PID > 0 {
		lines = append(lines, fmt.Sprintf("  main pid %d", state.PID))
	}
	if state.State == "running" && state.Uptime > 0 {
		lines = append(lines, "  uptime  "+humanDur(state.Uptime))
	}
	lines = append(lines, "")

	// Pong probe (read-only).
	if p := state.Pong; p != nil {
		lines = append(lines, tileSection.Render("Server (pong)"))
		if p.Name != "" {
			lines = append(lines, "  name    "+p.Name)
		}
		if p.Version != "" {
			lines = append(lines, "  version "+p.Version)
		}
		if p.GUID != "" {
			lines = append(lines, "  guid    "+p.GUID)
		}
		if p.MOTD != "" {
			lines = append(lines, "  motd    "+p.MOTD)
		}
		lines = append(lines, fmt.Sprintf("  online  %d / %d players", p.Online, p.MaxPlayers))
		lines = append(lines, "")
	} else {
		lines = append(lines, hintStyle.Render("  pong: unavailable (server not advertising)"), "")
	}

	lines = append(lines, hintStyle.Render("  s start    o stop    r restart    a adopt    u refresh"))
	return join(lines)
}

// humanDur renders a duration as a compact human string (e.g. "3h 12m").
func humanDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm %ds", m, s)
}
