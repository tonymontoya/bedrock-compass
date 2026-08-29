package tui

import (
	"context"
	"errors"
	"strings"
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
	toasts  []string
	lastRun func() tea.Cmd
}

func (h *fakeServerHost) Ask(title, consequence string, tier Tier, target string, onConfirm func() tea.Cmd) {
	h.asks = append(h.asks, title)
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
