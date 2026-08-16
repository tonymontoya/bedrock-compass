package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// Screen is one tab's content. Real screens (tickets #5-#13) implement it;
// the shell only depends on tea.Model plus the tab label.
type Screen interface {
	tea.Model
	Tab() string
}

// App is the top-level TUI model (ticket #3): the tab row, the active screen,
// the persistent status bar, the toast stack, and the confirmation overlay.
// It is the single tea.Model the program runs (cmd/bedrock-compass).
type App struct {
	Screens []Screen
	active  int
	width   int
	height  int

	status  compass.Status
	toasts  ToastStack
	confirm *Confirm
	action  func() tea.Cmd // pending action, run only on confirm

	// searchOpen/searchBuf implement the shell-level "/" search (D10), a
	// filter the active screen's list will honor once real screens land.
	searchOpen bool
	searchBuf  string
}

// toastTickMsg carries the current time so toast expiry is deterministic.
type toastTickMsg struct{ now time.Time }

const (
	toastPoll = 500 * time.Millisecond
	toastTTL  = 4 * time.Second
)

// NewApp builds the shell. With no arguments it uses the 8 placeholder
// screens (the shell's default state for ticket #3); real screens replace
// them as their tickets land.
func NewApp(screens ...Screen) *App {
	if len(screens) == 0 {
		screens = defaultScreens()
	}
	return &App{Screens: screens, width: 80, height: 24}
}

// defaultScreens returns the 8 D10 tabs, each a placeholder for now.
func defaultScreens() []Screen {
	return []Screen{
		Placeholder{Name: "Dashboard", Ticket: "#5"},
		Placeholder{Name: "Players", Ticket: "#6"},
		Placeholder{Name: "Access", Ticket: "#7"},
		Placeholder{Name: "Backups", Ticket: "#10"},
		Placeholder{Name: "Logs", Ticket: "#8"},
		Placeholder{Name: "Config", Ticket: "#11"},
		Placeholder{Name: "Console", Ticket: "#12"},
		Placeholder{Name: "Server", Ticket: "#13"},
	}
}

// Ask opens a confirmation overlay for a mutating action (D11). run is
// invoked only after the user confirms; its command is then executed.
func (a *App) Ask(title, consequence string, tier Tier, target string, run func() tea.Cmd) {
	a.confirm = NewConfirm(title, consequence, tier, target)
	a.action = run
}

// SetStatus updates the status bar (the telemetry core, ticket #4, drives it).
func (a *App) SetStatus(s compass.Status) { a.status = s }

// Toast pushes a transient notification with the default lifetime.
func (a *App) Toast(msg string, kind ToastKind) {
	a.toasts.Add(time.Now(), msg, kind, toastTTL)
}

// Init returns the shell's startup command (none unless a toast is live).
func (a *App) Init() tea.Cmd { return a.toastTick() }

func (a *App) toastTick() tea.Cmd {
	if !a.toasts.Active() {
		return nil
	}
	return tea.Tick(toastPoll, func(t time.Time) tea.Msg { return toastTickMsg{now: t} })
}

// Update routes one message: window size, toast ticks, and keys are handled
// by the shell; everything else goes to the active screen.
func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = m.Width, m.Height
		return a, a.toastTick()
	case toastTickMsg:
		a.toasts.Prune(m.now)
		return a, a.toastTick()
	case tea.KeyMsg:
		return a.updateKey(m)
	}

	if a.confirm != nil {
		return a, nil // overlay owns the interaction; drop other messages
	}
	ns, cmd := a.Screens[a.active].Update(msg)
	if s, ok := ns.(Screen); ok {
		a.Screens[a.active] = s
	}
	return a, cmd
}

func (a *App) updateKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if key.Type == tea.KeyCtrlC {
		return a, tea.Quit
	}
	if a.confirm != nil {
		c := a.confirm.Update(key)
		a.confirm = c
		switch c.result {
		case confirmOK:
			a.confirm = nil
			if a.action != nil {
				cmd := a.action()
				a.action = nil
				return a, cmd
			}
			a.action = nil
			return a, nil
		case confirmCancel:
			a.confirm = nil
			a.action = nil
			return a, nil
		}
		return a, nil
	}

	if a.searchOpen {
		a.handleSearchKey(key)
		return a, nil
	}
	if isRune(key, '/') {
		a.searchOpen = true
		a.searchBuf = ""
		return a, nil
	}

	switch {
	case isRune(key, 'q'):
		return a, tea.Quit
	case key.Type == tea.KeyLeft || isRune(key, '['):
		a.active = max(a.active-1, 0)
		return a, nil
	case key.Type == tea.KeyRight || isRune(key, ']'):
		a.active = min(a.active+1, len(a.Screens)-1)
		return a, nil
	case key.Type == tea.KeyTab:
		a.active = min(a.active+1, len(a.Screens)-1)
		return a, nil
	case key.Type == tea.KeyShiftTab:
		a.active = max(a.active-1, 0)
		return a, nil
	case isDigit(key):
		idx := int(key.Runes[0] - '1')
		if idx < len(a.Screens) {
			a.active = idx
		}
		return a, nil
	}

	ns, cmd := a.Screens[a.active].Update(key)
	if s, ok := ns.(Screen); ok {
		a.Screens[a.active] = s
	}
	return a, cmd
}

// View renders the whole shell: tab row, optional search line, the active
// screen (or the confirmation overlay), any live toasts, and the status bar.
func (a *App) View() string {
	w := a.width
	if w <= 0 {
		w = 80
	}

	var tabs []string
	for i, s := range a.Screens {
		label := s.Tab()
		if i == a.active {
			tabs = append(tabs, tabActive.Render(label))
		} else {
			tabs = append(tabs, tabInactive.Render(label))
		}
	}

	parts := []string{strings.Join(tabs, " ")}
	if a.searchOpen {
		parts = append(parts, promptStyle.Render("Search: "+a.searchBuf))
	}
	parts = append(parts, divider(w))
	if a.confirm != nil {
		parts = append(parts, a.confirm.View(w))
	} else {
		parts = append(parts, a.Screens[a.active].View())
	}
	if toasts := a.toasts.Render(); toasts != "" {
		parts = append(parts, "", toasts)
	}
	parts = append(parts, renderStatus(a.status, w))
	return strings.Join(parts, "\n")
}

// handleSearchKey edits the shell search buffer. Esc or Enter closes it.
func (a *App) handleSearchKey(key tea.KeyMsg) {
	switch key.Type {
	case tea.KeyEsc, tea.KeyEnter:
		a.searchOpen = false
		a.searchBuf = ""
	case tea.KeyBackspace:
		if len(a.searchBuf) > 0 {
			a.searchBuf = a.searchBuf[:len(a.searchBuf)-1]
		}
	case tea.KeyRunes:
		if len(key.Runes) > 0 && printable(key.Runes[0]) {
			a.searchBuf += string(key.Runes[0])
		}
	}
}

func divider(width int) string { return strings.Repeat("─", width) }

func isRune(key tea.KeyMsg, r rune) bool {
	return key.Type == tea.KeyRunes && len(key.Runes) == 1 && key.Runes[0] == r
}

func isDigit(key tea.KeyMsg) bool {
	return key.Type == tea.KeyRunes && len(key.Runes) == 1 && key.Runes[0] >= '1' && key.Runes[0] <= '9'
}
