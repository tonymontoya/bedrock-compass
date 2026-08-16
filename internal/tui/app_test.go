package tui

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

func key(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

func typeStr(t *testing.T, a *App, s string) {
	t.Helper()
	for _, r := range s {
		a.Update(key(r))
	}
}

func TestTabs_AllPresent(t *testing.T) {
	a := NewApp()
	view := a.View()
	for _, name := range []string{"Dashboard", "Players", "Access", "Backups", "Logs", "Config", "Console", "Server"} {
		if !strings.Contains(view, name) {
			t.Errorf("tab row missing %q", name)
		}
	}
}

func TestTabs_ArrowsSwitch(t *testing.T) {
	a := NewApp()
	if a.active != 0 {
		t.Fatalf("start active = %d, want 0", a.active)
	}
	if !strings.Contains(a.View(), "ticket #5") {
		t.Fatalf("dashboard body not visible")
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRight})
	if a.active != 1 {
		t.Fatalf("after right active = %d, want 1", a.active)
	}
	if !strings.Contains(a.View(), "ticket #6") {
		t.Fatalf("players body not visible after right")
	}
	a.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if a.active != 0 {
		t.Fatalf("after left active = %d, want 0", a.active)
	}
}

func TestTabs_NumberJump(t *testing.T) {
	a := NewApp()
	a.Update(key('6')) // 1-indexed: 6 -> index 5 -> Config
	if a.active != 5 {
		t.Fatalf("after '6' active = %d, want 5", a.active)
	}
	if !strings.Contains(a.View(), "ticket #11") {
		t.Fatalf("config body not visible after '6'")
	}
	a.Update(key('9')) // out of range: ignored
	if a.active != 5 {
		t.Fatalf("out-of-range '9' should be ignored, active = %d", a.active)
	}
}

func TestTabs_TabAndShiftTab(t *testing.T) {
	a := NewApp()
	a.Update(tea.KeyMsg{Type: tea.KeyTab})
	if a.active != 1 {
		t.Fatalf("after Tab active = %d, want 1", a.active)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if a.active != 0 {
		t.Fatalf("after ShiftTab active = %d, want 0", a.active)
	}
}

func TestToasts_AppearAndExpire(t *testing.T) {
	a := NewApp()
	base := time.Now()
	a.toasts.Add(base, "Backup complete", ToastSuccess, time.Second)
	if !strings.Contains(a.View(), "Backup complete") {
		t.Fatalf("toast not shown")
	}
	a.Update(toastTickMsg{now: base.Add(500 * time.Millisecond)})
	if !strings.Contains(a.View(), "Backup complete") {
		t.Fatalf("toast should still be visible before expiry")
	}
	a.Update(toastTickMsg{now: base.Add(2 * time.Second)})
	if strings.Contains(a.View(), "Backup complete") {
		t.Fatalf("toast should have expired")
	}
	if a.toasts.Active() {
		t.Fatalf("no toasts should remain active")
	}
}

func TestStatusBar_PlaceholdersThenPopulated(t *testing.T) {
	a := NewApp()
	if !strings.Contains(a.View(), "— players") {
		t.Errorf("expected players placeholder")
	}
	a.SetStatus(compass.Status{
		State: "running", Name: "MontoyaWorld", Version: "1.26.43",
		Online: 3, Max: 15, CPUPct: 42,
		RAMUsed: 3 * 1024 * 1024 * 1024, RAMTotal: 7 * 1024 * 1024 * 1024,
	})
	view := a.View()
	for _, want := range []string{"MontoyaWorld", "1.26.43", "3/15 players", "42%", "3.0 GiB/7.0 GiB"} {
		if !strings.Contains(view, want) {
			t.Errorf("status bar missing %q\n%s", want, view)
		}
	}
}

func TestQuit(t *testing.T) {
	a := NewApp()
	_, cmd := a.Update(key('q'))
	if cmd == nil {
		t.Fatal("q should produce a quit command")
	}
}

func TestQuit_CtrlCFromAnyState(t *testing.T) {
	// even with a confirmation overlay open
	a := NewApp()
	a.Ask("Stop", "consequence", TierYes, "", nil)
	if _, cmd := a.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Fatal("Ctrl-C should quit even while confirming")
	}
	// even while search is open
	a = NewApp()
	a.Update(key('/'))
	if _, cmd := a.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Fatal("Ctrl-C should quit even while searching")
	}
}

func TestSearch_OpenTypeClose(t *testing.T) {
	a := NewApp()
	if a.searchOpen {
		t.Fatal("search should start closed")
	}
	a.Update(key('/'))
	if !a.searchOpen {
		t.Fatal("/ should open search")
	}
	typeStr(t, a, "abc")
	if a.searchBuf != "abc" {
		t.Fatalf("searchBuf = %q, want abc", a.searchBuf)
	}
	if !strings.Contains(a.View(), "Search: abc") {
		t.Fatal("search line not rendered")
	}
	a.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if a.searchBuf != "ab" {
		t.Fatalf("backspace: searchBuf = %q, want ab", a.searchBuf)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.searchOpen {
		t.Fatal("Esc should close search")
	}
}

func TestSearch_CapturesKeysWhileOpen(t *testing.T) {
	a := NewApp()
	a.Update(key('/'))
	// tab-switch keys must NOT fire while search is open
	a.Update(key('2'))
	if a.active != 0 {
		t.Fatalf("while searching, '2' must not switch tabs; active = %d", a.active)
	}
	if a.searchBuf != "2" {
		t.Fatalf("'2' should go into the search buffer; got %q", a.searchBuf)
	}
}

func TestApp_RunsAndQuits(t *testing.T) {
	var buf bytes.Buffer
	var in bytes.Buffer
	in.WriteString("q")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p := tea.NewProgram(NewApp(), tea.WithInput(&in), tea.WithOutput(&buf), tea.WithContext(ctx))
	if _, err := p.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
}
