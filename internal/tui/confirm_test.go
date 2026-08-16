package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestConfirm_TierEnter(t *testing.T) {
	fired := false
	a := NewApp()
	a.Ask("Stop server", "This stops the server for ~10s.", TierEnter, "", func() tea.Cmd { fired = true; return nil })
	if !strings.Contains(a.View(), "This stops the server for ~10s.") {
		t.Fatalf("consequence not shown")
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if fired {
		t.Fatal("Esc must cancel")
	}
	if a.confirm != nil {
		t.Fatal("overlay should close on Esc")
	}

	a = NewApp()
	a.Ask("Stop server", "consequence", TierEnter, "", func() tea.Cmd { fired = true; return nil })
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !fired {
		t.Fatal("Enter should confirm TierEnter")
	}
}

func TestConfirm_TierYes(t *testing.T) {
	fired := false
	a := NewApp()
	a.Ask("Restart", "This restarts the server.", TierYes, "", func() tea.Cmd { fired = true; return nil })
	typeStr(t, a, "yes")
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if fired {
		t.Fatal("lowercase 'yes' must not confirm")
	}
	if !strings.Contains(a.View(), "does not confirm") {
		t.Fatal("expected a rejection message")
	}
	a.confirm.buffer = ""
	typeStr(t, a, "YES")
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !fired {
		t.Fatal("YES should confirm TierYes")
	}
}

func TestConfirm_TierTarget(t *testing.T) {
	fired := false
	a := NewApp()
	a.Ask("Restore", "This restores the backup, replacing the current world.", TierTarget, "backup-2024", func() tea.Cmd { fired = true; return nil })
	if !strings.Contains(a.View(), "backup-2024") {
		t.Fatal("target not shown in prompt")
	}
	typeStr(t, a, "backup-20")
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if fired {
		t.Fatal("partial target must not confirm")
	}
	a.confirm.buffer = ""
	typeStr(t, a, "backup-2024")
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !fired {
		t.Fatal("exact target should confirm")
	}
}
