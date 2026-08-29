package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// fakeHost records the dashboard's confirm and routing calls so tests can
// assert the quick-action tiers and destinations without a live shell.
type fakeHost struct {
	asks  []askedAction
	gosTo []string
}

type askedAction struct {
	title       string
	consequence string
	tier        Tier
	target      string
	onConfirm   func() tea.Cmd
}

func (f *fakeHost) Ask(title, consequence string, tier Tier, target string, onConfirm func() tea.Cmd) {
	f.asks = append(f.asks, askedAction{title, consequence, tier, target, onConfirm})
}

func (f *fakeHost) GoTo(label string) tea.Cmd {
	f.gosTo = append(f.gosTo, label)
	return nil
}

func statProvider(s compass.Status) func() compass.Status {
	return func() compass.Status { return s }
}

func evProvider(evs []compass.Event) func() []compass.Event {
	return func() []compass.Event { return evs }
}

func sampleStatus() compass.Status {
	return compass.Status{
		State:     "running",
		Name:      "MontoyaWorld",
		Version:   "1.26.43",
		MOTD:      "Bedrock level",
		Uptime:    time.Hour,
		Online:    3,
		Max:       15,
		CPUPct:    12,
		RAMUsed:   1 << 30,
		RAMTotal:  8 << 30,
		RSS:       320 << 20,
		DiskUsed:  45 << 30,
		DiskTotal: 200 << 30,
		NetUp:     1024,
		NetDown:   2048,
	}
}

// Renders all telemetry tiles (D8) for a running server.
func TestDashboard_RendersTiles(t *testing.T) {
	host := &fakeHost{}
	st := sampleStatus()
	d := NewDashboard(host, nil)
	d.Update(statusMsg(st))
	v := d.View()
	for _, want := range []string{"MontoyaWorld", "1.26.43", "Bedrock level", "3 / 15", "running", "12%"} {
		if !strings.Contains(v, want) {
			t.Errorf("View missing %q in:\n%s", want, v)
		}
	}
}

// Refresh: a new status broadcast (players joined) updates the tiles.
func TestDashboard_RefreshOnBroadcast(t *testing.T) {
	host := &fakeHost{}
	d := NewDashboard(host, nil)
	d.Update(statusMsg(compass.Status{Online: 0, Max: 15}))
	if !strings.Contains(d.View(), "0 / 15") {
		t.Fatalf("after first broadcast should show 0 players:\n%s", d.View())
	}
	d.Update(statusMsg(compass.Status{Online: 5, Max: 15}))
	if !strings.Contains(d.View(), "5 / 15") {
		t.Errorf("tiles did not refresh after broadcast:\n%s", d.View())
	}
}

// Stopped server degrades gracefully (no panics, sensible dashes).
func TestDashboard_StoppedState(t *testing.T) {
	host := &fakeHost{}
	d := NewDashboard(host, nil)
	d.Update(statusMsg(compass.Status{State: "stopped"}))
	v := d.View()
	if !strings.Contains(v, "stopped") {
		t.Errorf("View should show stopped state:\n%s", v)
	}
}

// Recent operations panel shows the last N audit records (D11).
func TestDashboard_RecentOperations(t *testing.T) {
	host := &fakeHost{}
	evs := []compass.Event{
		{Time: time.Now(), Verb: "backup", Params: map[string]string{"scope": "world"},
			Result: "ok", Duration: 2*time.Minute + 3*time.Second},
		{Time: time.Now(), Verb: "restart", Result: "error: server not running"},
	}
	d := NewDashboard(host, evProvider(evs))
	d.Update(statusMsg(compass.Status{}))
	v := d.View()
	for _, want := range []string{"backup", "restart", "ok", "error", "scope=world"} {
		if !strings.Contains(v, want) {
			t.Errorf("recent operations missing %q in:\n%s", want, v)
		}
	}
}

// Recent operations is empty until a status broadcast delivers events.
func TestDashboard_RecentOperations_Empty(t *testing.T) {
	host := &fakeHost{}
	d := NewDashboard(host, evProvider(nil))
	d.Update(statusMsg(compass.Status{}))
	if !strings.Contains(d.View(), "no operations yet") {
		t.Errorf("empty trail should show a placeholder:\n%s", d.View())
	}
}

// Quick action "backup now": confirms at the Enter tier and routes to Backups.
func TestDashboard_QuickAction_Backup(t *testing.T) {
	host := &fakeHost{}
	d := NewDashboard(host, nil)
	d.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	if len(host.asks) != 1 {
		t.Fatalf("expected 1 ask, got %d", len(host.asks))
	}
	ask := host.asks[0]
	if ask.tier != TierEnter {
		t.Errorf("backup tier = %v, want TierEnter", ask.tier)
	}
	ask.onConfirm()
	if len(host.gosTo) != 1 || host.gosTo[0] != "Backups" {
		t.Errorf("backup confirm routed to %v, want [Backups]", host.gosTo)
	}
}

// Quick action "restart": confirms at the YES tier and routes to Server.
func TestDashboard_QuickAction_Restart(t *testing.T) {
	host := &fakeHost{}
	d := NewDashboard(host, nil)
	d.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if len(host.asks) != 1 {
		t.Fatalf("expected 1 ask, got %d", len(host.asks))
	}
	ask := host.asks[0]
	if ask.tier != TierYes {
		t.Errorf("restart tier = %v, want TierYes", ask.tier)
	}
	ask.onConfirm()
	if len(host.gosTo) != 1 || host.gosTo[0] != "Server" {
		t.Errorf("restart confirm routed to %v, want [Server]", host.gosTo)
	}
}

// Unknown keys do nothing (no accidental confirmations).
func TestDashboard_IgnoresUnknownKeys(t *testing.T) {
	host := &fakeHost{}
	d := NewDashboard(host, nil)
	d.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if len(host.asks) != 0 {
		t.Errorf("unknown key should not ask, got %d", len(host.asks))
	}
}

// The App broadcasts a fresh status to the active dashboard, and the dashboard
// tiles agree with the status bar for the same snapshot.
func TestApp_BroadcastsStatusToDashboard(t *testing.T) {
	app := NewApp()
	st := sampleStatus()
	app.SetStatusProvider(statProvider(st))
	app.Screens[0] = NewDashboard(app, nil)

	app.Update(statusTickMsg{})

	dash, ok := app.Screens[0].(*Dashboard)
	if !ok {
		t.Fatal("Screens[0] is not a *Dashboard")
	}
	if dash.status != st {
		t.Errorf("dashboard status = %+v, want %+v", dash.status, st)
	}
	if !strings.Contains(dash.View(), "MontoyaWorld") {
		t.Error("dashboard tiles missing the server name")
	}
	// status bar agrees with the tiles for the same snapshot
	if !strings.Contains(renderStatus(st, 80), "MontoyaWorld") {
		t.Error("status bar missing the server name")
	}
}
