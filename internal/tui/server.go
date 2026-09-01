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
	"strconv"
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

// Server is the supervisor's screen: the supervisor's truth (state, source,
// unit, main pid, uptime), the read-only pong probe, the start / stop /
// restart / adopt actions, and the #14 Provision wizard (install a fresh
// deployment: plan shown verbatim, applied at TierYes, never writing the
// compass config — the config snippet is shown and the operator adopts).
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

	// Provision wizard (#14). A sequential single-buffer form: the operator
	// types each field and presses Enter to commit it, advancing to the next.
	// When the last field commits, the plan is computed and rendered verbatim;
	// Enter on the completed plan applies it at TierYes. Esc cancels the
	// wizard at any point. While provActive, CapturingKeys reports true so the
	// shell routes every key here (digits, letters) instead of jumping tabs.
	provActive  bool
	provIdx     provField // current field
	provBuf     string
	provErr     string
	provName    string
	provDir     string
	provSup     string
	provSrc     compass.SourceMode
	provDetail  string
	provUser    string
	provPort    int
	provPlan    *compass.ProvisionPlan
	provPlanErr string
	provApplied *compass.ProvisionResult
}

// provField orders the wizard's sequential prompts.
type provField int

const (
	pfName provField = iota
	pfDir
	pfSupervisor
	pfSource
	pfDetail
	pfUser
	pfPort
	pfComplete // all fields committed; the plan is shown, Enter applies
)

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

// adoptPortHint returns the port the adopt probe should use. After a fresh
// provision the config does not yet carry the port the server came up on, so
// the probe must use the provisioned port (K-01) — otherwise it probes the
// stale configured port. With no fresh provision (the general adopt case) it
// is 0: the lifecycle falls back to the configured/default port.
func (s *Server) adoptPortHint() int {
	if s.provPlan != nil {
		return s.provPlan.Input.Port
	}
	return 0
}

func (s *Server) Tab() string { return "Server" }

func (s *Server) Init() tea.Cmd { return s.ticks() }

// lifecycleOK reports whether the screen can drive a lifecycle action.
func (s *Server) lifecycleOK() bool {
	return s.stack != nil && s.stack.Lifecycle != nil
}

// provisionOK reports whether the screen can drive a provision action. A fresh
// host with no running server has no lifecycle, so this is independent of it.
func (s *Server) provisionOK() bool {
	return s.stack != nil && s.stack.Provision != nil
}

// CapturingKeys reports whether the Server screen owns raw keyboard input —
// i.e. the provision wizard's free-text form is active. While true, the shell
// routes every key here (letters, digits) so they reach the field instead of
// jumping tabs or quitting. See KeyCaptor.
func (s *Server) CapturingKeys() bool { return s.provActive }

// provBegin opens the provision wizard at the first field with all values
// cleared. The plan and any prior result are dropped.
func (s *Server) provBegin() {
	if !s.provisionOK() {
		s.host.Toast("provision unavailable (config did not resolve)", ToastError)
		return
	}
	s.provActive = true
	s.provIdx = pfName
	s.provBuf = ""
	s.provErr = ""
	s.provName = ""
	s.provDir = ""
	s.provSup = ""
	s.provSrc = ""
	s.provDetail = ""
	s.provUser = ""
	s.provPort = 0
	s.provPlan = nil
	s.provPlanErr = ""
	s.provApplied = nil
}

// provFieldLabel returns the prompt label for the field at idx.
func (s *Server) provFieldLabel(idx provField) string {
	switch idx {
	case pfName:
		return "name"
	case pfDir:
		return "server dir"
	case pfSupervisor:
		return "supervisor (s=systemd, t=tmux)"
	case pfSource:
		return "source (z=local zip, v=download by version)"
	case pfDetail:
		if s.provSrc == compass.SourceVersion {
			return "version"
		}
		return "zip path"
	case pfUser:
		return "user (empty = current user)"
	case pfPort:
		return "port (empty = default 19132)"
	}
	return ""
}

// provInput assembles the compass.ProvisionInput from the wizard's committed
// fields.
func (s *Server) provInput() compass.ProvisionInput {
	in := compass.ProvisionInput{
		Name:       s.provName,
		ServerDir:  s.provDir,
		Supervisor: s.provSup,
		Source:     s.provSrc,
		User:       s.provUser,
		Port:       s.provPort,
	}
	if s.provSrc == compass.SourceVersion {
		in.Version = s.provDetail
	} else {
		in.ZipPath = s.provDetail
	}
	return in
}

// provComputePlan runs the (read-only) plan for the committed fields and stores
// it for verbatim rendering. It touches no world state.
func (s *Server) provComputePlan() {
	plan, err := s.stack.Provision.Plan(s.provInput())
	if err != nil {
		s.provPlan = nil
		s.provPlanErr = err.Error()
		return
	}
	s.provPlanErr = ""
	s.provPlan = &plan
}

// provCommit validates the buffer against the current field, stores it, and
// advances. When the last field (port) commits, the plan is computed.
func (s *Server) provCommit() {
	val := strings.TrimSpace(s.provBuf)
	switch s.provIdx {
	case pfName:
		if val == "" {
			s.provErr = "name is required"
			return
		}
		s.provName = val
	case pfDir:
		if val == "" {
			s.provErr = "server dir is required"
			return
		}
		s.provDir = val
	case pfSupervisor:
		switch val {
		case "s", "systemd":
			s.provSup = "systemd"
		case "t", "tmux":
			s.provSup = "tmux"
		default:
			s.provErr = "press s (systemd) or t (tmux)"
			return
		}
	case pfSource:
		switch val {
		case "z", "zip":
			s.provSrc = compass.SourceZip
		case "v", "version":
			s.provSrc = compass.SourceVersion
		default:
			s.provErr = "press z (local zip) or v (download by version)"
			return
		}
	case pfDetail:
		if val == "" {
			s.provErr = "the " + s.provFieldLabel(s.provIdx) + " is required"
			return
		}
		s.provDetail = val
	case pfUser:
		s.provUser = val // optional
	case pfPort:
		if val != "" {
			n, err := strconv.Atoi(val)
			if err != nil || n <= 0 || n > 65535 {
				s.provErr = "port must be a number between 1 and 65535"
				return
			}
			s.provPort = n
		}
	}
	s.provBuf = ""
	s.provErr = ""
	if s.provIdx == pfPort {
		s.provIdx = pfComplete
		s.provComputePlan()
		return
	}
	s.provIdx++
}

// provUpdate handles keys while the wizard is active. Printable runes append to
// the buffer, backspace deletes, Esc cancels, and Enter commits the current
// field (or applies the completed plan at TierYes).
func (s *Server) provUpdate(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if s.provIdx == pfComplete {
		switch key.Type {
		case tea.KeyEsc:
			s.provActive = false
			s.provPlan = nil
			s.provApplied = nil
			return s, s.ticks()
		case tea.KeyEnter:
			s.provAskApply()
			return s, s.ticks()
		}
		return s, s.ticks()
	}
	switch key.Type {
	case tea.KeyEsc:
		s.provActive = false
		s.provBuf = ""
		s.provErr = ""
		return s, s.ticks()
	case tea.KeyBackspace:
		if len(s.provBuf) > 0 {
			s.provBuf = s.provBuf[:len(s.provBuf)-1]
		}
		s.provErr = ""
		return s, s.ticks()
	case tea.KeyEnter:
		s.provCommit()
		return s, s.ticks()
	case tea.KeyRunes:
		if len(key.Runes) > 0 && printable(key.Runes[0]) {
			s.provBuf += string(key.Runes[0])
			s.provErr = ""
		}
		return s, s.ticks()
	}
	return s, s.ticks()
}

// provisionDoneMsg carries the structured Apply result back to Update so all
// state mutations happen on the model goroutine (the command's closure runs in
// a goroutine and must not touch shared fields).
type provisionDoneMsg struct {
	res compass.ProvisionResult
	err string
}

// provAskApply confirms applying the computed plan at TierYes (it installs the
// server and starts the supervisor). The confirmation consequence names the
// target deployment.
func (s *Server) provAskApply() {
	in := s.provInput()
	s.host.Ask(
		"Provision server",
		fmt.Sprintf("Install %q into %s under %s, accept the EULA, wire the supervisor, and start it.", in.Name, in.ServerDir, in.Supervisor),
		TierYes,
		in.Name,
		func() tea.Cmd {
			s.working = true
			s.workLabel = "provisioning"
			return func() tea.Msg {
				res, err := s.stack.Provision.Apply(context.Background(), in)
				m := provisionDoneMsg{res: res}
				if err != nil {
					m.err = err.Error()
				}
				return m
			}
		},
	)
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
	case provisionDoneMsg:
		s.working = false
		s.workLabel = ""
		s.provActive = false
		s.provApplied = &m.res
		if m.err != "" {
			s.host.Toast("provision: "+m.err, ToastError)
		} else {
			s.host.Toast("provision: "+m.res.Status, ToastSuccess)
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
		if s.provActive {
			return s.provUpdate(m)
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
	case isRune(key, 'p'):
		s.provBegin()
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
				a, err := s.stack.Lifecycle.Adopt(ctx, s.adoptPortHint())
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

// provisionSection renders the #14 Provision surface: the active wizard (field
// prompt, the verbatim plan, or the apply result). It returns no lines when
// the wizard is idle and there is no result to show.
func (s *Server) provisionSection() []string {
	var out []string
	if s.provActive {
		out = append(out, tileSection.Render("Provision"))
		if s.provIdx < pfComplete {
			out = append(out, promptStyle.Render("  "+s.provFieldLabel(s.provIdx)+": "+s.provBuf+"█"))
			if s.provErr != "" {
				out = append(out, toastErr.Render("  "+s.provErr))
			} else {
				out = append(out, hintStyle.Render("  Enter to commit · Esc to cancel"))
			}
			return out
		}
		// Completed: render the plan verbatim (or its error), then the apply hint.
		if s.provPlanErr != "" {
			out = append(out, toastErr.Render("  plan: "+s.provPlanErr))
		}
		if s.provPlan != nil {
			out = append(out, s.renderPlan(s.provPlan)...)
		}
		out = append(out, hintStyle.Render("  Enter to apply · Esc to cancel"))
		return out
	}
	if s.provApplied != nil {
		out = append(out, tileSection.Render("Provision (result)"))
		out = append(out, "  status  "+s.provApplied.Status)
		if s.provApplied.Detail != "" {
			out = append(out, "  "+s.provApplied.Detail)
		}
		for _, c := range s.provApplied.Commands {
			out = append(out, "  $ "+c)
		}
		for _, c := range s.provApplied.Cleanup {
			out = append(out, "  cleanup $ "+c)
		}
		if s.provApplied.Status == "provisioned" && s.provPlan != nil && s.provPlan.ConfigSnippet != "" {
			out = append(out, "", tileSection.Render("Adopt into the compass config"))
			for _, ln := range strings.Split(s.provPlan.ConfigSnippet, "\n") {
				out = append(out, "  "+ln)
			}
			out = append(out, hintStyle.Render("  a adopt   (persists into the compass config)"))
		}
		return out
	}
	return out
}

// renderPlan renders a provision plan verbatim: the action, steps, prerequisites,
// the supervisor artifacts (systemd units or tmux commands), and the config
// snippet.
func (s *Server) renderPlan(plan *compass.ProvisionPlan) []string {
	var out []string
	out = append(out, "  action  "+plan.Action)
	if plan.EffectiveUser != "" {
		out = append(out, "  user    "+plan.EffectiveUser)
	}
	if len(plan.Prerequisites) > 0 {
		out = append(out, "  prerequisites")
		for _, p := range plan.Prerequisites {
			out = append(out, "    - "+p)
		}
	}
	if len(plan.Steps) > 0 {
		out = append(out, "  steps")
		for i, st := range plan.Steps {
			out = append(out, fmt.Sprintf("    %d. %s", i+1, st))
		}
	}
	if plan.UnitService != "" || plan.UnitSocket != "" {
		out = append(out, "  systemd units")
		if plan.UnitService != "" {
			out = append(out, unitBlock(plan.UnitService)...)
		}
		if plan.UnitSocket != "" {
			out = append(out, unitBlock(plan.UnitSocket)...)
		}
	}
	if len(plan.TmuxCommands) > 0 {
		out = append(out, "  tmux commands")
		for _, c := range plan.TmuxCommands {
			out = append(out, "    $ "+c)
		}
	}
	if plan.ConfigSnippet != "" {
		out = append(out, "  compass config (copy or press a to adopt)")
		for _, ln := range strings.Split(plan.ConfigSnippet, "\n") {
			out = append(out, "    "+ln)
		}
	}
	return out
}

// unitBlock indents a unit file's lines for display.
func unitBlock(unit string) []string {
	var out []string
	for _, ln := range strings.Split(unit, "\n") {
		out = append(out, "    "+ln)
	}
	return out
}

// View renders the supervisor's truth (state, source, unit, main pid,
// uptime), the pong probe, the Provision section (#14), the key hints, and the
// working spinner.
func (s *Server) View() string {
	lines := []string{titleStyle.Render("Server"), ""}

	if s.working {
		f := string(backupSpinnerFrames[s.spin%len(backupSpinnerFrames)])
		lines = append(lines, promptStyle.Render("  "+f+" "+s.workLabel)+"…", "")
	}

	if s.lifecycleOK() {
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
	} else {
		lines = append(lines, toastErr.Render("  server unavailable (config did not resolve)"), "")
	}

	// Provision section (#14): the wizard, the verbatim plan, or the result.
	if prov := s.provisionSection(); len(prov) > 0 {
		lines = append(lines, prov...)
		lines = append(lines, "")
	}

	// While the wizard owns the keyboard it renders its own hint, so the
	// key-hints row is suppressed then.
	if s.provActive {
		return join(lines)
	}
	lines = append(lines, hintStyle.Render("  s start    o stop    r restart    a adopt    p provision    u refresh"))
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
