package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// configFakeHost is a recording configHost that also captures the tier of
// each confirmation request (the fakeServerHost records only titles).
type configFakeHost struct {
	asks   []configAsk
	toasts []string
}

type configAsk struct {
	title     string
	conseq    string
	tier      Tier
	onConfirm func() tea.Cmd
}

func (h *configFakeHost) Ask(title, consequence string, tier Tier, target string, onConfirm func() tea.Cmd) {
	h.asks = append(h.asks, configAsk{title: title, conseq: consequence, tier: tier, onConfirm: onConfirm})
}

func (h *configFakeHost) Toast(msg string, kind ToastKind) {
	h.toasts = append(h.toasts, msg)
}

// newConfigRig builds a Stack whose Props service runs over a real temp
// server dir and an offline fake console (accessFakeConsole). When
// restartable, a Lifecycle over a recording Supervisor is included (its
// Stop/Start are recorded in *calls).
func newConfigRig(t *testing.T, props string, restartable bool) (*compass.Stack, *accessFakeConsole, string, *[]string) {
	t.Helper()
	dir := t.TempDir()
	if props != "" {
		if err := os.WriteFile(filepath.Join(dir, "server.properties"), []byte(props), 0o644); err != nil {
			t.Fatalf("write server.properties: %v", err)
		}
	}
	trail, err := compass.NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("trail: %v", err)
	}
	console := &accessFakeConsole{}
	cfg := &compass.Config{ServerDir: dir, Port: 19142}
	var lifecycle *compass.Lifecycle
	var calls *[]string
	if restartable {
		c := []string{}
		calls = &c
		sup := &compass.Supervisor{
			Stop:  func(ctx context.Context) error { *calls = append(*calls, "stop"); return nil },
			Start: func(ctx context.Context) error { *calls = append(*calls, "start"); return nil },
		}
		lifecycle = compass.NewLifecycle(cfg, sup, nil, trail)
	}
	return &compass.Stack{Cfg: cfg, Lifecycle: lifecycle, Props: compass.NewProps(cfg, console.wire(), trail, lifecycle)}, console, dir, calls
}

func readPropsValue(t *testing.T, dir, key string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "server.properties"))
	if err != nil {
		t.Fatalf("read server.properties: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func TestConfig_View_RendersRowsMarkersAndHelp(t *testing.T) {
	host := &configFakeHost{}
	st, _, _, _ := newConfigRig(t, "max-players=15\ndifficulty=normal\nonline-mode=true\nmotd=My World\n", false)
	s := NewConfig(host, st)
	view := s.View()

	for _, want := range []string{
		"Config — server.properties",
		"max-players=", "15", "hot",
		"difficulty=", "normal",
		"online-mode=", "true", "cold",
		"motd=", "My World",
		"allowed: 0-65535", // selected row (max-players) help
		"allowlist.json", "Access screen",
		"permissions.json", "Players screen",
		"↑/↓ or j/k select", "Enter edit", "r restart", "u refresh",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("View is missing %q\nview:\n%s", want, view)
		}
	}
}

func TestConfig_EditHotKey_AppliesLiveAndPersists(t *testing.T) {
	host := &configFakeHost{}
	st, console, dir, _ := newConfigRig(t, "max-players=15\nmotd=World\n", false)
	s := NewConfig(host, st)

	// Row 0 is max-players (hot). Open the field, type the new value, commit.
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !s.CapturingKeys() {
		t.Fatal("Enter should open the value field (CapturingKeys)")
	}
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3', '2'}})
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if len(host.asks) != 1 {
		t.Fatalf("asks = %+v, want exactly one", host.asks)
	}
	ask := host.asks[0]
	if ask.tier != TierEnter {
		t.Errorf("tier = %v, want TierEnter (hot key applies live)", ask.tier)
	}
	if !strings.Contains(ask.title, "max-players") || !strings.Contains(ask.title, "32") {
		t.Errorf("title = %q, want the key and new value", ask.title)
	}
	if ask.onConfirm == nil {
		t.Fatal("onConfirm is nil")
	}
	cmd := ask.onConfirm()
	if cmd == nil {
		t.Fatal("onConfirm returned nil cmd")
	}
	res := cmd().(opResultMsg)
	if res.err != "" {
		t.Fatalf("op result = %+v, want no error", res)
	}
	if !strings.Contains(res.detail, "applied live") {
		t.Errorf("detail = %q, want a live-apply note", res.detail)
	}

	if v := readPropsValue(t, dir, "max-players"); v != "32" {
		t.Errorf("max-players = %q, want 32 (persisted)", v)
	}
	if v := readPropsValue(t, dir, "motd"); v != "World" {
		t.Errorf("motd = %q, want World (other keys untouched)", v)
	}
	if len(console.calls) != 1 || console.calls[0] != "setmaxplayers 32" {
		t.Errorf("console calls = %q, want [\"setmaxplayers 32\"]", console.calls)
	}

	// The result message toasts and re-loads the rows.
	s.Update(res)
	if !strings.Contains(host.toasts[len(host.toasts)-1], "applied live") {
		t.Errorf("toasts = %q, want the live-apply note", host.toasts)
	}
	if v := readPropsValue(t, dir, "max-players"); v != "32" {
		t.Errorf("after refresh max-players = %q, want 32", v)
	}
}

func TestConfig_EditColdKey_RestartTierAndRestart(t *testing.T) {
	host := &configFakeHost{}
	st, console, dir, calls := newConfigRig(t, "online-mode=true\n", true)
	s := NewConfig(host, st)

	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f', 'a', 'l', 's', 'e'}})
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if len(host.asks) != 1 {
		t.Fatalf("asks = %+v, want exactly one", host.asks)
	}
	ask := host.asks[0]
	if ask.tier != TierYes {
		t.Errorf("tier = %v, want TierYes (cold key applies at the restart tier)", ask.tier)
	}
	if !strings.Contains(ask.conseq, "restart") {
		t.Errorf("consequence = %q, want a restart warning", ask.conseq)
	}
	if len(console.calls) != 0 {
		t.Errorf("console calls = %q, want none before confirm (cold keys never touch the console)", console.calls)
	}

	res := ask.onConfirm()().(opResultMsg)
	if res.err != "" {
		t.Fatalf("op result = %+v, want no error", res)
	}
	if v := readPropsValue(t, dir, "online-mode"); v != "false" {
		t.Errorf("online-mode = %q, want false (queued)", v)
	}
	if want := []string{"stop", "start"}; len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
		t.Errorf("supervisor calls = %q, want %q (restart applied the change)", *calls, want)
	}
	if !strings.Contains(res.detail, "restarted") {
		t.Errorf("detail = %q, want the restart note", res.detail)
	}
}

func TestConfig_EditInvalidValue_StaysInFieldWithAllowedText(t *testing.T) {
	host := &configFakeHost{}
	st, console, dir, _ := newConfigRig(t, "max-players=15\n", false)
	s := NewConfig(host, st)

	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'-', '1'}})
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if len(host.asks) != 0 {
		t.Fatalf("asks = %+v, want none (invalid value must not confirm)", host.asks)
	}
	if !s.CapturingKeys() {
		t.Error("the field must stay open on an invalid value")
	}
	view := s.View()
	if !strings.Contains(view, "0-65535") {
		t.Errorf("View = %q, want the allowed-values text inline", view)
	}
	if v := readPropsValue(t, dir, "max-players"); v != "15" {
		t.Errorf("max-players = %q, want 15 (unchanged)", v)
	}
	if len(console.calls) != 0 {
		t.Errorf("console calls = %q, want none", console.calls)
	}
}

func TestConfig_EditEsc_Cancels(t *testing.T) {
	host := &configFakeHost{}
	st, _, dir, _ := newConfigRig(t, "max-players=15\n", false)
	s := NewConfig(host, st)

	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'9', '9'}})
	s.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if s.CapturingKeys() {
		t.Error("Esc must close the field")
	}
	if len(host.asks) != 0 {
		t.Errorf("asks = %+v, want none (cancelled)", host.asks)
	}
	if v := readPropsValue(t, dir, "max-players"); v != "15" {
		t.Errorf("max-players = %q, want 15 (cancelled)", v)
	}
}

func TestConfig_RestartKey_OpensRestartTier(t *testing.T) {
	host := &configFakeHost{}
	st, _, _, calls := newConfigRig(t, "online-mode=true\n", true)
	s := NewConfig(host, st)

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if len(host.asks) != 1 {
		t.Fatalf("asks = %+v, want exactly one", host.asks)
	}
	if host.asks[0].tier != TierYes {
		t.Errorf("tier = %v, want TierYes (restart is destructive)", host.asks[0].tier)
	}
	res := host.asks[0].onConfirm()().(opResultMsg)
	if res.err != "" {
		t.Fatalf("op result = %+v, want no error", res)
	}
	if want := []string{"stop", "start"}; len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
		t.Errorf("supervisor calls = %q, want %q", *calls, want)
	}
}

func TestConfig_View_DegradesWithoutService(t *testing.T) {
	host := &configFakeHost{}
	s := NewConfig(host, &compass.Stack{})
	view := s.View()
	if !strings.Contains(view, "config service unavailable") {
		t.Errorf("View = %q, want a clear degraded state", view)
	}
}

func TestConfig_View_MissingPropsFile(t *testing.T) {
	host := &configFakeHost{}
	st, _, _, _ := newConfigRig(t, "", false) // no server.properties
	s := NewConfig(host, st)
	view := s.View()
	if !strings.Contains(view, "server.properties") {
		t.Errorf("View = %q, want the read failure surfaced", view)
	}
}
