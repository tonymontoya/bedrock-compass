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

// KeyCaptor is an optional capability a screen implements when it owns raw
// keyboard input in some mode (a free-text field, e.g. Access's IP/XUID entry).
// While CapturingKeys() reports true, the shell routes every key to that
// screen and applies none of its global shortcuts (tab jump on digits, quit on
// q, search on /) — the field gets them first. Screens without a raw input mode
// simply don't implement it, so their behavior is unchanged: the confirm overlay
// already captures keys at the shell level, which is why earlier screens never
// needed this.
type KeyCaptor interface {
	CapturingKeys() bool
}

// SearchFilterer is an optional capability a screen implements when it honors
// the shell's "/" search as a filter on its content (D10). The shell applies
// the buffer to the active screen when the operator confirms with Enter —
// an empty buffer clears any text filter; Esc closes without applying.
type SearchFilterer interface {
	SetSearch(text string)
}

// App is the top-level TUI model (ticket #3): the tab row, the active screen,
// the persistent status bar, the toast stack, and the confirmation overlay.
// It is the single tea.Model the program runs (cmd/bedrock-compass).
type App struct {
	Screens    []Screen
	active     int
	prevActive int // last active index before the current switch (ticket #12)
	width      int
	height     int

	status         compass.Status
	statusProvider func() compass.Status // polled on a tick (ticket #4)
	toasts         ToastStack
	confirm        *Confirm
	action         func() tea.Cmd // pending action, run only on confirm

	// searchOpen/searchBuf implement the shell-level "/" search (D10): on
	// Enter the buffer is applied to the active screen when it implements
	// SearchFilterer; Esc closes without applying.
	searchOpen bool
	searchBuf  string
}

// toastTickMsg carries the current time so toast expiry is deterministic.
type toastTickMsg struct{ now time.Time }

// statusTickMsg triggers a poll of the status provider (ticket #4).
type statusTickMsg struct{}

// statusMsg carries a fresh Status snapshot to the active screen (the
// dashboard, ticket #5) so its tiles stay in sync with the status bar.
type statusMsg compass.Status

// tabSwitchMsg defers a tab switch until the shell processes it as its own
// message. Screens request switches through host methods (GoTo, ExitConsole);
// those run inside the screen's Update, and switching eagerly there would make
// the shell's write-back store the *screen* model into the slot of the tab we
// are leaving — corrupting that tab (ticket #12 found this: Esc from Console
// clobbered Config). Deferring keeps every mutation in the shell's own Update.
type tabSwitchMsg struct{ idx int }

const (
	toastPoll  = 500 * time.Millisecond
	toastTTL   = 4 * time.Second
	statusPoll = 2 * time.Second
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

// GoTo switches to the tab whose label matches (a no-op if none does). It is
// how a screen routes the operator to the screen that owns an action (D10),
// e.g. the dashboard's quick actions jump to Backups or Server. The switch is
// deferred: it runs when the shell processes the returned command, never
// eagerly inside the calling screen's Update (see tabSwitchMsg).
func (a *App) GoTo(label string) tea.Cmd {
	for i, s := range a.Screens {
		if s.Tab() == label {
			return deferTabSwitch(i)
		}
	}
	return nil
}

// deferTabSwitch schedules one tab switch as a shell message.
func deferTabSwitch(idx int) tea.Cmd {
	return func() tea.Msg { return tabSwitchMsg{idx: idx} }
}

// SetStatus updates the status bar (the telemetry core, ticket #4, drives it).
func (a *App) SetStatus(s compass.Status) { a.status = s }

// Toast pushes a transient notification with the default lifetime.
func (a *App) Toast(msg string, kind ToastKind) {
	a.toasts.Add(time.Now(), msg, kind, toastTTL)
}

// Init returns the shell's startup command (live periodic ticks, if any).
func (a *App) Init() tea.Cmd { return a.ticks() }

func (a *App) toastTick() tea.Cmd {
	if !a.toasts.Active() {
		return nil
	}
	return tea.Tick(toastPoll, func(t time.Time) tea.Msg { return toastTickMsg{now: t} })
}

// SetStatusProvider sets a provider the shell polls on a tick to keep the
// status bar live (ticket #4). nil disables polling.
func (a *App) SetStatusProvider(fn func() compass.Status) { a.statusProvider = fn }

func (a *App) statusTick() tea.Cmd {
	return tea.Tick(statusPoll, func(time.Time) tea.Msg { return statusTickMsg{} })
}

// ticks returns the batch of live periodic commands (toast expiry and status
// polling), or nil when none are active.
func (a *App) ticks() tea.Cmd {
	var cmds []tea.Cmd
	if a.toasts.Active() {
		cmds = append(cmds, a.toastTick())
	}
	if a.statusProvider != nil {
		cmds = append(cmds, a.statusTick())
	}
	switch len(cmds) {
	case 0:
		return nil
	case 1:
		return cmds[0]
	default:
		return tea.Batch(cmds...)
	}
}

// broadcastStatus refreshes the status from the provider and forwards the
// snapshot to the active screen, so the dashboard tiles (ticket #5) and the
// status bar always show the same data. It returns the command the active
// screen scheduled in response — screens piggyback this broadcast for their
// own async work (Players re-polls its roster on it), and dropping that
// command would strand a freshly activated tab with no fetch ever running.
func (a *App) broadcastStatus() tea.Cmd {
	if a.statusProvider == nil {
		return nil
	}
	s := a.statusProvider()
	a.status = s
	ns, cmd := a.Screens[a.active].Update(statusMsg(s))
	if sc, ok := ns.(Screen); ok {
		a.Screens[a.active] = sc
	}
	return cmd
}

// switchTab moves to the tab at idx (clamped) and re-broadcasts the current
// status so the newly active screen is current immediately. It returns the
// command that broadcast scheduled (the new screen's pending work). A real
// change records the prior index in prevActive so a screen can return there
// (ticket #12's Console "back" key); a no-op switch leaves it untouched.
func (a *App) switchTab(idx int) tea.Cmd {
	if idx < 0 {
		idx = 0
	}
	if idx >= len(a.Screens) {
		idx = len(a.Screens) - 1
	}
	if idx != a.active {
		a.prevActive = a.active
		a.active = idx
	}
	return a.broadcastStatus()
}

// ExitConsole returns to the tab that was active before the Console screen
// (ticket #12). The Console owns raw keyboard input while active, so Esc is its
// "back" key; this restores the previous tab and re-broadcasts status. If there
// is no valid previous tab it falls back to the first one. Like GoTo, the
// switch is deferred to a shell message — never executed eagerly inside the
// Console's Update (see tabSwitchMsg).
func (a *App) ExitConsole() tea.Cmd {
	target := a.prevActive
	if target < 0 || target >= len(a.Screens) || target == a.active {
		target = 0
	}
	return deferTabSwitch(target)
}

// Update routes one message: window size, toast ticks, and keys are handled
// by the shell; everything else goes to the active screen.
func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = m.Width, m.Height
		// Forward to the active screen too: screens that render variable-height
		// content (the Logs scrollback, #8) size their viewport from it.
		ns, cmd := a.Screens[a.active].Update(msg)
		if s, ok := ns.(Screen); ok {
			a.Screens[a.active] = s
		}
		return a, tea.Batch(cmd, a.ticks())
	case toastTickMsg:
		a.toasts.Prune(m.now)
		return a, a.ticks()
	case statusTickMsg:
		return a, tea.Batch(a.broadcastStatus(), a.ticks())
	case tabSwitchMsg:
		// Deferred screen-requested switch (GoTo / ExitConsole). Runs here —
		// in the shell's own Update — so no write-back can clobber a slot.
		return a, tea.Batch(a.switchTab(m.idx), a.ticks())
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

	// A screen that owns raw keyboard input (a free-text field) gets every key
	// first; none of the shell's global shortcuts apply while it captures.
	if kc, ok := a.Screens[a.active].(KeyCaptor); ok && kc.CapturingKeys() {
		ns, cmd := a.Screens[a.active].Update(key)
		if s, ok := ns.(Screen); ok {
			a.Screens[a.active] = s
		}
		return a, cmd
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
		return a, a.switchTab(a.active - 1)
	case key.Type == tea.KeyRight || isRune(key, ']'):
		return a, a.switchTab(a.active + 1)
	case key.Type == tea.KeyTab:
		return a, a.switchTab(a.active + 1)
	case key.Type == tea.KeyShiftTab:
		return a, a.switchTab(a.active - 1)
	case isDigit(key):
		if idx := int(key.Runes[0] - '1'); idx >= 0 && idx < len(a.Screens) {
			return a, a.switchTab(idx)
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
	if toasts := a.toasts.Render(w); toasts != "" {
		parts = append(parts, "", toasts)
	}
	parts = append(parts, renderStatus(a.status, w))
	return strings.Join(parts, "\n")
}

// handleSearchKey edits the shell search buffer. Esc closes without applying;
// Enter applies the buffer to the active screen when it implements
// SearchFilterer (an empty buffer clears its text filter).
func (a *App) handleSearchKey(key tea.KeyMsg) {
	switch key.Type {
	case tea.KeyEsc:
		a.searchOpen = false
		a.searchBuf = ""
	case tea.KeyEnter:
		if sf, ok := a.Screens[a.active].(SearchFilterer); ok {
			sf.SetSearch(a.searchBuf)
		}
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
