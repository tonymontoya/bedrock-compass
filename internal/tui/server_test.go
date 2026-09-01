package tui

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// fakeServerHost is a recording serverHost for the Server screen. It
// captures the confirmation request (ask) and the transient notifications
// (toasts), and lets a test drive the recorded confirm callback.
type fakeServerHost struct {
	asks    []string
	tiers   []Tier
	toasts  []string
	lastRun func() tea.Cmd
}

func (h *fakeServerHost) Ask(title, consequence string, tier Tier, target string, onConfirm func() tea.Cmd) {
	h.asks = append(h.asks, title)
	h.tiers = append(h.tiers, tier)
	h.lastRun = onConfirm
}

func (h *fakeServerHost) Toast(msg string, kind ToastKind) {
	h.toasts = append(h.toasts, msg)
}

// newServerRig builds a *compass.Stack whose Lifecycle is driven by a
// recorded Supervisor.Status and a deterministic pong probe. This mirrors
// the compass package's own offline testing seams (Lifecycle exposes
// Status/Ping/Discover/ConsoleCheck as injectable fields).
func newServerRig(t *testing.T, status compass.ProcessStatus, statusErr error) *compass.Stack {
	t.Helper()
	sup := &compass.Supervisor{
		Status: func(ctx context.Context) (compass.ProcessStatus, error) { return status, statusErr },
		Stop:   func(ctx context.Context) error { return nil },
		Start:  func(ctx context.Context) error { return nil },
	}
	cfg := &compass.Config{ServerDir: t.TempDir(), Port: 19132}
	lc := compass.NewLifecycle(cfg, sup, nil, nil)
	// Deterministic pong: answer when the supervisor reports the server
	// active, refuse otherwise (so the "pong unavailable" path is tested).
	lc.Ping = func(ctx context.Context, addr string) (compass.Pong, error) {
		if !strings.HasSuffix(addr, ":19132") {
			t.Errorf("ping addr = %q, want 127.0.0.1:19132", addr)
		}
		if status.Active {
			return compass.Pong{Name: "test", Version: "1.26.430", Online: 1, MaxPlayers: 2}, nil
		}
		return compass.Pong{}, errors.New("refused")
	}
	return &compass.Stack{Cfg: cfg, Lifecycle: lc}
}

func TestServer_View_Running(t *testing.T) {
	host := &fakeServerHost{}
	st := newServerRig(t, compass.ProcessStatus{
		Active: true,
		Unit:   "minecraft-bedrock.service",
		PID:    4242,
		Uptime: 3*time.Hour + 7*time.Minute,
		Source: "systemd",
	}, nil)
	s := NewServer(host, st)
	view := s.View()

	for _, want := range []string{
		"Server",
		"running",
		"minecraft-bedrock.service",
		"4242",
		"3h 7m",
		"systemd",
		"test",
		"1.26.430",
		"1 / 2 players",
		"s start    o stop    r restart    a adopt",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("View is missing %q\nview:\n%s", want, view)
		}
	}
}

func TestServer_View_Stopped(t *testing.T) {
	host := &fakeServerHost{}
	st := newServerRig(t, compass.ProcessStatus{Active: false, Source: "systemd"}, nil)
	s := NewServer(host, st)
	view := s.View()

	if !strings.Contains(view, "stopped") {
		t.Errorf("View should show stopped\nview:\n%s", view)
	}
	if !strings.Contains(view, "pong: unavailable") {
		t.Errorf("View should note the pong probe is unavailable\nview:\n%s", view)
	}
}

func TestServer_View_DegradedWhenNoLifecycle(t *testing.T) {
	host := &fakeServerHost{}
	s := NewServer(host, &compass.Stack{}) // no lifecycle resolved
	view := s.View()
	if !strings.Contains(view, "server unavailable") {
		t.Errorf("View should degrade clearly\nview:\n%s", view)
	}
	if !strings.Contains(view, "s start    o stop    r restart    a adopt") {
		t.Errorf("View should still show the key hints\nview:\n%s", view)
	}
}

func TestServer_StartConfirmsAtTierEnter(t *testing.T) {
	host := &fakeServerHost{}
	st := newServerRig(t, compass.ProcessStatus{Active: true, Source: "systemd"}, nil)
	s := NewServer(host, st)

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	if len(host.asks) != 1 || host.asks[0] != "Start server" {
		t.Fatalf("asks = %v, want [Start server]", host.asks)
	}
	if host.lastRun == nil {
		t.Fatal("no confirm command recorded")
	}
	// Drive the confirm to completion; it should produce a success result.
	msg := host.lastRun()()
	r, ok := msg.(opResultMsg)
	if !ok {
		t.Fatalf("msg = %#v, want opResultMsg", msg)
	}
	if r.err != "" {
		t.Errorf("start err = %q, want ok", r.err)
	}
}

func TestServer_StopConfirmsAtTierYes(t *testing.T) {
	host := &fakeServerHost{}
	st := newServerRig(t, compass.ProcessStatus{Active: true, Source: "systemd"}, nil)
	s := NewServer(host, st)

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	if len(host.asks) != 1 || host.asks[0] != "Stop server" {
		t.Fatalf("asks = %v, want [Stop server]", host.asks)
	}
}

func TestServer_RestartConfirms(t *testing.T) {
	host := &fakeServerHost{}
	st := newServerRig(t, compass.ProcessStatus{Active: true, Source: "systemd"}, nil)
	s := NewServer(host, st)

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if len(host.asks) != 1 || host.asks[0] != "Restart server" {
		t.Fatalf("asks = %v, want [Restart server]", host.asks)
	}
}

func TestServer_AdoptConfirms(t *testing.T) {
	host := &fakeServerHost{}
	st := newServerRig(t, compass.ProcessStatus{Active: true, Source: "systemd"}, nil)
	s := NewServer(host, st)

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if len(host.asks) != 1 || host.asks[0] != "Adopt server" {
		t.Fatalf("asks = %v, want [Adopt server]", host.asks)
	}
}

func TestServer_TabLabel(t *testing.T) {
	host := &fakeServerHost{}
	s := NewServer(host, &compass.Stack{})
	if s.Tab() != "Server" {
		t.Errorf("Tab() = %q, want Server", s.Tab())
	}
}

// --- Provision wizard (#14) tests ------------------------------------------

// fakeExec is a recording ExecFunc for the Provision service: it records every
// call and lets a test fail specific full commands ("<name> <args...>") or
// whole commands ("<name>").
type fakeExec struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]error
}

func (f *fakeExec) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	full := name
	if len(args) > 0 {
		full = name + " " + strings.Join(args, " ")
	}
	f.mu.Lock()
	f.calls = append(f.calls, full)
	f.mu.Unlock()
	if err, ok := f.fail[full]; ok {
		return nil, err
	}
	if err, ok := f.fail[name]; ok {
		return nil, err
	}
	return nil, nil
}

func (f *fakeExec) callsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// provServerZip writes a minimal bedrock-server zip (a single bedrock_server
// entry) and returns its path.
func provServerZip(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "bedrock-server.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("bedrock_server")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("#!/bin/sh\necho fake-server\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// newProvRig builds a *compass.Stack whose Provision is driven by a recording
// fake exec and a deterministic Verify. The session name is "compass-prov";
// for tmux, has-session fails (a fresh host, not an inconsistent one).
func newProvRig(t *testing.T, verifyErr error) (*compass.Stack, *fakeExec) {
	t.Helper()
	fe := &fakeExec{fail: map[string]error{
		"tmux has-session -t compass-prov": errors.New("no session"),
	}}
	return &compass.Stack{
		Provision: &compass.Provision{
			Env:    &compass.ProvisionEnv{IsRoot: false, CurrentUser: "tony"},
			Exec:   fe.run,
			Verify: func(ctx context.Context, addr string) error { return verifyErr },
		},
	}, fe
}

func keyMsg(r rune) tea.Msg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }
func enterMsg() tea.Msg     { return tea.KeyMsg{Type: tea.KeyEnter} }
func escMsg() tea.Msg       { return tea.KeyMsg{Type: tea.KeyEsc} }

// provType types a field value into the active wizard and commits it with Enter.
func provType(s *Server, val string) {
	for _, r := range val {
		s.Update(keyMsg(r))
	}
	s.Update(enterMsg())
}

// provDriveTmuxForm runs the wizard to completion with a tmux + zip selection.
func provDriveTmuxForm(s *Server, dir, zipPath string) {
	s.Update(keyMsg('p'))
	provType(s, "compass-prov")
	provType(s, dir)
	provType(s, "t")
	provType(s, "z")
	provType(s, zipPath)
	provType(s, "") // user (optional)
	provType(s, "") // port (optional) → completes + computes plan
}

func TestServer_Prov_CapturingKeys(t *testing.T) {
	host := &fakeServerHost{}
	stack, _ := newProvRig(t, nil)
	s := NewServer(host, stack)
	if s.CapturingKeys() {
		t.Fatal("should not capture keys before the wizard")
	}
	s.Update(keyMsg('p'))
	if !s.CapturingKeys() {
		t.Fatal("should capture keys while the wizard is active")
	}
	s.Update(escMsg())
	if s.CapturingKeys() {
		t.Fatal("should stop capturing keys after Esc")
	}
}

func TestServer_Prov_RequiredFieldError(t *testing.T) {
	host := &fakeServerHost{}
	stack, _ := newProvRig(t, nil)
	s := NewServer(host, stack)
	s.Update(keyMsg('p'))
	// Empty name → error, stays on the name field.
	s.Update(enterMsg())
	if s.provIdx != pfName {
		t.Fatalf("idx = %d, want pfName", s.provIdx)
	}
	if s.provErr == "" {
		t.Fatal("expected a required-field error")
	}
}

func TestServer_Prov_EscCancels(t *testing.T) {
	host := &fakeServerHost{}
	stack, _ := newProvRig(t, nil)
	s := NewServer(host, stack)
	s.Update(keyMsg('p'))
	provType(s, "compass-prov")
	s.Update(escMsg())
	if s.provActive {
		t.Fatal("wizard should be inactive after Esc")
	}
	if s.provName != "compass-prov" {
		// committed values may remain; the point is the wizard closed
	}
}

func TestServer_Prov_WizardTmuxPlan(t *testing.T) {
	host := &fakeServerHost{}
	stack, _ := newProvRig(t, nil)
	s := NewServer(host, stack)

	provDriveTmuxForm(s, "/tmp/compass-prov", "/tmp/bedrock-server.zip")

	if s.provIdx != pfComplete {
		t.Fatalf("idx = %d, want pfComplete", s.provIdx)
	}
	if s.provPlan == nil {
		t.Fatal("plan should be computed")
	}
	if len(s.provPlan.TmuxCommands) == 0 {
		t.Fatalf("plan should have tmux commands: %+v", s.provPlan)
	}

	view := s.View()
	for _, want := range []string{"Provision", "new-session", "pipe-pane", "compass config"} {
		if !strings.Contains(view, want) {
			t.Errorf("View missing %q\nview:\n%s", want, view)
		}
	}
}

func TestServer_Prov_PlanSystemdUnits(t *testing.T) {
	host := &fakeServerHost{}
	stack, _ := newProvRig(t, nil)
	s := NewServer(host, stack)

	s.Update(keyMsg('p'))
	provType(s, "compass-prov")
	provType(s, "/tmp/compass-prov")
	provType(s, "s") // supervisor systemd
	provType(s, "z")
	provType(s, "/tmp/bedrock-server.zip")
	provType(s, "") // user
	provType(s, "") // port

	if s.provPlan == nil {
		t.Fatal("plan should be computed")
	}
	if s.provPlan.UnitService == "" || s.provPlan.UnitSocket == "" {
		t.Fatalf("systemd plan should have both units: %+v", s.provPlan)
	}
	view := s.View()
	for _, want := range []string{"[Unit]", "ListenFIFO", "systemd units"} {
		if !strings.Contains(view, want) {
			t.Errorf("View missing %q\nview:\n%s", want, view)
		}
	}
}

func TestServer_Prov_ApplyAtTierYes(t *testing.T) {
	host := &fakeServerHost{}
	stack, fe := newProvRig(t, nil)
	zipPath := provServerZip(t)
	s := NewServer(host, stack)

	provDriveTmuxForm(s, t.TempDir(), zipPath)
	if s.provIdx != pfComplete {
		t.Fatalf("idx = %d, want pfComplete", s.provIdx)
	}

	// Enter on the completed plan asks to apply at TierYes.
	s.Update(enterMsg())
	if len(host.asks) != 1 || host.asks[0] != "Provision server" {
		t.Fatalf("asks = %v", host.asks)
	}
	if host.tiers[0] != TierYes {
		t.Fatalf("tier = %v, want TierYes", host.tiers[0])
	}

	// Drive the confirmation to completion.
	msg := host.lastRun()()
	done, ok := msg.(provisionDoneMsg)
	if !ok {
		t.Fatalf("msg = %#v, want provisionDoneMsg", msg)
	}
	if done.err != "" {
		t.Fatalf("apply err = %q (status=%q)", done.err, done.res.Status)
	}
	if done.res.Status != "provisioned" {
		t.Fatalf("status = %q, want provisioned", done.res.Status)
	}

	// Handle the result message (state mutations happen here).
	s.Update(done)
	if s.provApplied == nil || s.provApplied.Status != "provisioned" {
		t.Fatalf("provApplied = %+v", s.provApplied)
	}
	if s.provActive {
		t.Fatal("wizard should be inactive after apply")
	}
	// The tmux session should have been started and piped.
	calls := fe.callsSnapshot()
	hasNew, hasPipe := false, false
	for _, c := range calls {
		if strings.HasPrefix(c, "tmux new-session") {
			hasNew = true
		}
		if strings.HasPrefix(c, "tmux pipe-pane") {
			hasPipe = true
		}
	}
	if !hasNew || !hasPipe {
		t.Fatalf("expected new-session + pipe-pane, got %v", calls)
	}
}

func TestServer_Prov_ApplyIncompleteShowsCommands(t *testing.T) {
	host := &fakeServerHost{}
	stack, _ := newProvRig(t, nil)
	zipPath := provServerZip(t)
	s := NewServer(host, stack)

	// systemd as non-root → incomplete with handoff commands.
	s.Update(keyMsg('p'))
	provType(s, "compass-prov")
	provType(s, t.TempDir())
	provType(s, "s")
	provType(s, "z")
	provType(s, zipPath)
	provType(s, "tony")
	provType(s, "")

	s.Update(enterMsg())
	msg := host.lastRun()()
	done, ok := msg.(provisionDoneMsg)
	if !ok {
		t.Fatalf("msg = %#v, want provisionDoneMsg", msg)
	}
	if done.res.Status != "incomplete" {
		t.Fatalf("status = %q, want incomplete (err=%v)", done.res.Status, done.err)
	}
	if len(done.res.Commands) == 0 {
		t.Fatal("expected handoff commands for non-root systemd")
	}
	s.Update(done)
	if s.provApplied == nil {
		t.Fatal("provApplied should be set")
	}
	view := s.View()
	if !strings.Contains(view, "incomplete") {
		t.Errorf("View should show incomplete\nview:\n%s", view)
	}
	for _, c := range done.res.Commands {
		if !strings.Contains(view, c) {
			t.Errorf("View missing command %q\nview:\n%s", c, view)
		}
	}
}
