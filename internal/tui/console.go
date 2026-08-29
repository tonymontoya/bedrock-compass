// Package tui: the Console screen (ticket #12). It is a thin renderer over
// the compass RawConsole service — the raw passthrough surface where typed
// input goes to the server verbatim and its output streams back into a
// transcript. Esc returns to the TUI cleanly; sent-command history is kept
// and navigable with ↑/↓.
//
// Raw mode is deliberately the no-hands-off surface: the confirmation tiers
// do not apply here (ADR-0009), because every command typed at this surface
// is already an explicit, deliberate act by a human who can see exactly what
// they are typing. What raw mode does NOT waive is the audit trail — the
// service records every raw command with verb "console" regardless of outcome.
//
// The screen owns all keyboard input while active (CapturingKeys reports true):
// printable runes append to the buffer, Backspace deletes, Enter sends the line
// verbatim, Esc exits back to the previous tab, and ↑/↓ walk sent-command
// history. A send is a blocking console round-trip run in a goroutine; while it
// is in flight the screen owns the interaction (keys are ignored) so nothing is
// left pending in the channel — the round-trip always runs to completion before
// Esc can exit, and its output lands in the transcript.
package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// consoleScrollback caps the in-memory transcript.
const consoleScrollback = 2000

// consoleHistoryCap caps sent-command history (↑/↓ navigation).
const consoleHistoryCap = 50

// consoleLineKind classifies one transcript line for styling.
type consoleLineKind int

const (
	lineCmd consoleLineKind = iota // an echoed command ("❯ …")
	lineOut                        // server output
	lineErr                        // a send failure note
)

// consoleLine is one row of the transcript.
type consoleLine struct {
	kind consoleLineKind
	text string
}

// consoleHost is the shell the Console screen talks back to: transient
// notifications (Toast) and returning to the previous tab on Esc
// (ExitConsole). *App implements it; a fake in tests keeps the screen decoupled.
type consoleHost interface {
	Toast(msg string, kind ToastKind)
	ExitConsole() tea.Cmd
}

// Console is the ticket #12 raw passthrough screen over a resolved Stack's
// RawConsole service. A nil stack (or one without a RawConsole) renders a
// degraded state and does not capture keys.
type Console struct {
	host  consoleHost
	stack *compass.Stack

	lines     []consoleLine // transcript, oldest first, capped
	buf       string        // the command being typed
	working   bool          // a send round-trip is in flight
	workLabel string        // "sending …" shown while working
	spin      int

	history []string // sent commands, oldest first
	histIdx int      // -1 = not navigating; else index into history

	offset int // lines hidden below the viewport; 0 = stuck to the newest line

	width  int
	height int

	// consoleMu serializes every RawConsole.Send: only one round-trip at a time,
	// so sends never harvest each other's output.
	consoleMu sync.Mutex
}

// NewConsole builds the Console screen over a resolved Stack.
func NewConsole(host consoleHost, stack *compass.Stack) *Console {
	return &Console{host: host, stack: stack, histIdx: -1, width: 80, height: 24}
}

func (c *Console) Tab() string { return "Console" }

// CapturingKeys reports whether the screen owns raw keyboard input. The Console
// screen is always in raw mode while active and serviceable — every key goes to
// it (digits are typed, not tab jumps; q is typed, not quit). Esc exits back to
// the TUI; Ctrl+C still quits globally at the shell level.
func (c *Console) CapturingKeys() bool { return c.serviceOK() }

func (c *Console) serviceOK() bool { return c.stack != nil && c.stack.RawConsole != nil }

func (c *Console) Init() tea.Cmd { return nil }

// consoleSendMsg carries one send round-trip's result.
type consoleSendMsg struct {
	command string
	out     string
	err     string
}

// Update handles the async send result, spinner ticks, window resizes, and keys.
// Status polls are ignored for content (the console is on-demand — operator
// sends, not a live tail) but must keep an in-flight send's spinner alive.
func (c *Console) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case serverSpinnerMsg:
		c.spin = (c.spin + 1) % len(backupSpinnerFrames)
		return c, c.ticks()
	case consoleSendMsg:
		c.working = false
		c.workLabel = ""
		if m.out != "" {
			for _, ln := range strings.Split(m.out, "\n") {
				c.appendLine(consoleLine{kind: lineOut, text: ln})
			}
		}
		if m.err != "" {
			note := "error: " + m.err
			if m.out == "" {
				note = m.err
			}
			c.appendLine(consoleLine{kind: lineErr, text: note})
		}
		c.offset = 0 // stick to the newest line after a send completes
		return c, nil
	case tea.WindowSizeMsg:
		c.width, c.height = m.Width, m.Height
		return c, nil
	case statusMsg:
		// No content refresh (on-demand surface), but keep an in-flight send's
		// spinner animating across the shell's 2s status broadcast.
		return c, c.ticks()
	case tea.KeyMsg:
		if c.working {
			return c, c.ticks() // a send owns the interaction until it settles
		}
		return c.updateKey(m)
	}
	return c, nil
}

func (c *Console) ticks() tea.Cmd {
	if !c.working {
		return nil
	}
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg {
		return serverSpinnerMsg{frame: c.spin}
	})
}

// updateKey handles raw-mode keys. Esc exits back to the TUI; Enter sends the
// buffer verbatim (empty is a no-op); ↑/↓ walk history; printable runes append;
// Backspace deletes the last rune.
func (c *Console) updateKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.Type {
	case tea.KeyEsc:
		return c, c.host.ExitConsole()
	case tea.KeyEnter:
		cmd := strings.TrimSpace(c.buf)
		if cmd == "" {
			return c, nil
		}
		c.pushHistory(cmd)
		c.appendLine(consoleLine{kind: lineCmd, text: "❯ " + cmd})
		c.buf = ""
		c.histIdx = -1 // a fresh send resets history navigation
		c.working = true
		c.workLabel = "sending…"
		return c, tea.Batch(c.sendCmd(cmd), c.ticks())
	case tea.KeyBackspace:
		if len(c.buf) > 0 {
			r := []rune(c.buf)
			c.buf = string(r[:len(r)-1])
		}
		return c, nil
	case tea.KeyUp:
		c.historyPrev()
		return c, nil
	case tea.KeyDown:
		c.historyNext()
		return c, nil
	case tea.KeyPgUp:
		c.scroll(c.viewHeight()) // one page toward OLDER lines (offset grows)
		return c, nil
	case tea.KeyPgDown:
		c.scroll(-c.viewHeight()) // one page back toward the newest line
		return c, nil
	case tea.KeySpace:
		c.buf += " "
		c.histIdx = -1 // typing a new command leaves history navigation
		return c, nil
	case tea.KeyRunes:
		for _, r := range key.Runes {
			if printable(r) {
				c.buf += string(r)
			}
		}
		if len(key.Runes) > 0 {
			c.histIdx = -1 // typing a new command leaves history navigation
		}
		return c, nil
	}
	return c, nil
}

// sendCmd runs one verbatim round-trip in a goroutine. The console mutex
// serializes it against any other send; the service audits the command (D11).
func (c *Console) sendCmd(command string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), c.sendDeadline())
		defer cancel()
		c.consoleMu.Lock()
		defer c.consoleMu.Unlock()
		out, err := c.stack.RawConsole.Send(ctx, command)
		if err != nil {
			return consoleSendMsg{command: command, out: out, err: err.Error()}
		}
		return consoleSendMsg{command: command, out: out}
	}
}

// sendDeadline derives the per-send context deadline from the resolved console
// policy so the screen can never cut a harvest short. The harvest's own Timeout
// is the hard cap on how long Console.Send runs; we add margin for round-trip
// overhead beyond it. Falls back to 20s when no channel/policy resolved (the
// send then fails fast anyway, but with a sane deadline).
func (c *Console) sendDeadline() time.Duration {
	const fallback = 20 * time.Second
	if c.stack != nil && c.stack.RawConsole != nil && c.stack.RawConsole.Console != nil {
		t := c.stack.RawConsole.Console.Policy.Timeout
		if t > 0 {
			return t + 5*time.Second // margin beyond the harvest hard cap
		}
	}
	return fallback
}

// pushHistory records a sent command (deduping a leading repeat) and caps the
// list at consoleHistoryCap.
func (c *Console) pushHistory(cmd string) {
	if n := len(c.history); n > 0 && c.history[n-1] == cmd {
		return // don't stack identical consecutive commands
	}
	c.history = append(c.history, cmd)
	if len(c.history) > consoleHistoryCap {
		c.history = c.history[len(c.history)-consoleHistoryCap:]
	}
}

// historyPrev moves toward older sent commands (↑). Starting from a fresh buffer
// jumps to the most recent; repeated presses walk back.
func (c *Console) historyPrev() {
	if len(c.history) == 0 {
		return
	}
	if c.histIdx < 0 {
		c.histIdx = len(c.history) - 1
	} else if c.histIdx > 0 {
		c.histIdx--
	}
	c.buf = c.history[c.histIdx]
}

// historyNext moves toward newer sent commands (↓). Reaching the newest clears
// navigation and empties the buffer.
func (c *Console) historyNext() {
	if c.histIdx < 0 || len(c.history) == 0 {
		return
	}
	if c.histIdx < len(c.history)-1 {
		c.histIdx++
		c.buf = c.history[c.histIdx]
	} else {
		c.histIdx = -1
		c.buf = ""
	}
}

// appendLine adds one transcript line, trimming the oldest past the cap.
func (c *Console) appendLine(l consoleLine) {
	c.lines = append(c.lines, l)
	if len(c.lines) > consoleScrollback {
		c.lines = c.lines[len(c.lines)-consoleScrollback:]
	}
}

// scroll moves the transcript viewport by n lines (positive = toward older
// lines), clamped to the buffer — same convention as the Logs screen.
func (c *Console) scroll(n int) {
	max := len(c.lines) - c.viewHeight()
	if max < 0 {
		max = 0
	}
	c.offset += n
	if c.offset < 0 {
		c.offset = 0
	}
	if c.offset > max {
		c.offset = max
	}
}

// viewHeight is the number of transcript lines that fit between the title and
// the prompt/hint. The shell chrome (tab row, divider, status bar) plus this
// screen's fixed lines are subtracted from the terminal height — same budget as
// the Logs viewport so both screens size identically.
func (c *Console) viewHeight() int {
	h := c.height - 7
	if h < 4 {
		h = 4
	}
	return h
}

// View renders the title, the transcript viewport, the prompt line, and the key
// hints. Degraded state (no RawConsole) is shown when the service did not resolve.
func (c *Console) View() string {
	lines := []string{titleStyle.Render("Console"), ""}

	if !c.serviceOK() {
		lines = append(lines, toastErr.Render("  console unavailable (config did not resolve a channel)"))
		lines = append(lines, hintStyle.Render("  Esc back · Ctrl+C quit"))
		return join(lines)
	}

	unit := ""
	if c.stack.Cfg != nil && c.stack.Cfg.Service != "" {
		unit = " (" + c.stack.Cfg.Service + ")"
	}
	lines[0] = titleStyle.Render("Console" + unit + "  — raw passthrough, no confirmation tiers")

	vh := c.viewHeight()
	// Clamp the offset locally so View stays pure (no state mutation during
	// render). scroll() clamps on input; this guards against a stale offset
	// left behind when appendLine trims old lines.
	maxOffset := len(c.lines) - vh
	if maxOffset < 0 {
		maxOffset = 0
	}
	off := c.offset
	if off > maxOffset {
		off = maxOffset
	}
	start := len(c.lines) - vh - off
	if start < 0 {
		start = 0
	}
	end := start + vh
	if end > len(c.lines) {
		end = len(c.lines)
	}

	w := c.width - 2
	if w < 10 {
		w = 10
	}
	if len(c.lines) == 0 {
		lines = append(lines, hintStyle.Render("  (no commands sent yet — type a command and press Enter)"))
	} else {
		for _, l := range c.lines[start:end] {
			lines = append(lines, "  "+c.renderLine(l, w))
		}
		if off > 0 {
			lines = append(lines, hintStyle.Render(fmt.Sprintf("  … %d older line(s) — PgUp to scroll", off)))
		}
	}

	if c.working {
		f := string(backupSpinnerFrames[c.spin%len(backupSpinnerFrames)])
		lines = append(lines, promptStyle.Render("  "+f+" "+c.workLabel))
	} else {
		lines = append(lines, consolePrompt.Render("❯ ")+c.buf)
	}

	lines = append(lines, hintStyle.Render(
		"  type a command · Enter send · ↑/↓ history · PgUp/PgDn scroll · Esc back"))
	return join(lines)
}

// renderLine styles one transcript line: echoed commands are accented, output is
// plain, and error notes are red. Long lines are truncated to the viewport width.
// Truncation happens on the PLAIN text before styling — cutting a styled string
// by rune count can split an ANSI escape sequence in half (lipgloss wraps its
// output in SGR codes), which would corrupt the terminal's color state.
func (c *Console) renderLine(l consoleLine, width int) string {
	switch l.kind {
	case lineCmd:
		return consoleCmd.Render("❯ " + truncateRunes(l.text[len("❯ "):], width))
	case lineErr:
		return toastErr.Render(truncateRunes(l.text, width))
	default:
		return truncateRunes(l.text, width)
	}
}
