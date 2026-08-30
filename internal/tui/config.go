// Package tui: the Config screen (ticket #11). It is a thin renderer over
// the compass Props service (D15): the server.properties keys with hot/cold
// markers and per-key allowed-values help, a read-only pointer to the live
// JSON configs (owned by the Access and Players screens), and tiered
// editing — hot keys confirm at TierEnter and apply live through the
// console, cold keys confirm at TierYes (type "YES") and apply on the
// restart the screen triggers in the same action.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// configHost is the shell the Config screen talks back to: confirmations
// (Ask) and transient notifications (Toast). *App implements it.
type configHost interface {
	Ask(title, consequence string, tier Tier, target string, onConfirm func() tea.Cmd)
	Toast(msg string, kind ToastKind)
}

// configSpinnerMsg advances the indeterminate spinner while an operation is
// in flight.
type configSpinnerMsg struct{ frame int }

const configSpinnerFrames = "|/-\\"

// Config is the ticket #11 screen: the server.properties editor.
type Config struct {
	host  configHost
	stack *compass.Stack

	rows    []compass.PropRow
	rowsErr string
	cursor  int

	editing bool
	buf     string
	errIn   string

	// In-flight operation (Set / Restart running in a goroutine).
	working   bool
	workLabel string
	spin      int
}

// NewConfig builds the Config screen over a resolved Stack and loads the
// current rows (a cheap file read). A nil stack or a missing service renders
// a degraded state.
func NewConfig(host configHost, stack *compass.Stack) *Config {
	s := &Config{host: host, stack: stack}
	s.refresh()
	return s
}

// Tab is the screen's label in the shell tab bar (D10).
func (c *Config) Tab() string { return "Config" }

// props returns the Props service, or nil (degraded).
func (c *Config) props() *compass.Props {
	if c.stack == nil {
		return nil
	}
	return c.stack.Props
}

// refresh re-reads the rows from the service.
func (c *Config) refresh() {
	p := c.props()
	if p == nil {
		c.rows = nil
		c.rowsErr = "config service unavailable (server dir not resolved)"
		return
	}
	rows, err := p.Rows()
	if err != nil {
		c.rows = nil
		c.rowsErr = err.Error()
		return
	}
	c.rows = rows
	c.rowsErr = ""
	if c.cursor >= len(rows) {
		c.cursor = 0
	}
}

// Init has no startup command.
func (c *Config) Init() tea.Cmd { return nil }

// CapturingKeys reports whether the Config screen owns raw keyboard input
// (the value field while editing a row).
func (c *Config) CapturingKeys() bool { return c.editing }

// ticks schedules the spinner animation while an operation is in flight.
func (c *Config) ticks() tea.Cmd {
	if c.working {
		return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg {
			return configSpinnerMsg{frame: c.spin}
		})
	}
	return nil
}

// Update handles the spinner tick, the operation-result message, and keys.
func (c *Config) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case configSpinnerMsg:
		c.spin = (c.spin + 1) % len(configSpinnerFrames)
		return c, c.ticks()
	case opResultMsg:
		c.working = false
		c.workLabel = ""
		if m.err != "" {
			c.host.Toast(m.label+": "+m.err, ToastError)
		} else {
			if m.detail != "" {
				c.host.Toast(m.label+": "+m.detail, ToastSuccess)
			} else {
				c.host.Toast(m.label+": ok", ToastSuccess)
			}
		}
		c.refresh()
		return c, c.ticks()
	case tea.KeyMsg:
		if c.working {
			return c, c.ticks() // an op owns the interaction
		}
		return c.updateKey(m)
	}
	return c, c.ticks()
}

func (c *Config) updateKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if c.editing {
		return c.editingKey(key)
	}
	switch {
	case key.Type == tea.KeyUp || isRune(key, 'k'):
		if c.cursor > 0 {
			c.cursor--
		}
	case key.Type == tea.KeyDown || isRune(key, 'j'):
		if c.cursor < len(c.rows)-1 {
			c.cursor++
		}
	case key.Type == tea.KeyEnter || isRune(key, 'e'):
		c.beginEdit()
	case isRune(key, 'r'):
		c.askRestart()
	case isRune(key, 'u'):
		c.refresh()
	}
	return c, nil
}

// beginEdit opens the value field on the selected row, starting empty. The
// current value stays visible in the row list above, so the field holds
// exactly the value being set — no stale characters to clear.
func (c *Config) beginEdit() {
	if len(c.rows) == 0 {
		return
	}
	c.editing = true
	c.buf = ""
	c.errIn = ""
}

// editingKey handles raw input while the value field is open.
func (c *Config) editingKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Type == tea.KeyEsc:
		c.editing = false
	case key.Type == tea.KeyBackspace:
		if len(c.buf) > 0 {
			c.buf = c.buf[:len(c.buf)-1]
		}
		c.errIn = ""
	case key.Type == tea.KeyEnter:
		c.commitEdit()
	case key.Type == tea.KeyRunes:
		for _, r := range key.Runes {
			if printable(r) {
				c.buf += string(r)
			}
		}
		c.errIn = ""
	}
	return c, nil
}

// commitEdit validates the buffer and opens the confirmation for the
// matching tier: hot keys at TierEnter (applies live), cold keys at TierYes
// (restarts the server so the queued value takes effect).
func (c *Config) commitEdit() {
	p := c.props()
	row := c.rows[c.cursor]
	if p == nil {
		c.errIn = "config service unavailable"
		return
	}
	canonical, err := p.Validate(row.Key, c.buf)
	if err != nil {
		c.errIn = err.Error()
		return
	}
	c.editing = false
	c.buf = ""
	c.errIn = ""

	title := fmt.Sprintf("Set %s = %s", row.Key, canonical)
	if row.Info.Hot {
		c.host.Ask(
			title,
			"applies live through the console — the running server changes immediately.",
			TierEnter, "",
			func() tea.Cmd { return c.runSet(row.Key, canonical) },
		)
		return
	}
	c.host.Ask(
		title,
		"applies on restart — the server restarts now and players are disconnected.",
		TierYes, "",
		func() tea.Cmd { return c.runSetAndRestart(row.Key, canonical) },
	)
}

// askRestart offers a plain restart (queued cold changes take effect).
func (c *Config) askRestart() {
	c.host.Ask(
		"Restart server",
		"Players are disconnected; queued config changes take effect.",
		TierYes, "",
		func() tea.Cmd {
			c.working = true
			c.workLabel = "restart"
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				if err := c.stack.Props.Restart(ctx); err != nil {
					return opResultMsg{label: "restart", err: err.Error()}
				}
				return opResultMsg{label: "restart", detail: "restarted — queued config changes active"}
			}
		},
	)
}

// runSet applies a validated hot key through the service (console apply +
// atomic write + audit), reporting an opResultMsg.
func (c *Config) runSet(key, value string) tea.Cmd {
	c.working = true
	c.workLabel = "config " + key
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		detail, err := c.stack.Props.Set(ctx, key, value)
		if err != nil {
			return opResultMsg{label: c.workLabel, err: err.Error()}
		}
		return opResultMsg{label: c.workLabel, detail: detail}
	}
}

// runSetAndRestart queues a validated cold key and restarts the server so it
// takes effect, reporting an opResultMsg.
func (c *Config) runSetAndRestart(key, value string) tea.Cmd {
	c.working = true
	c.workLabel = "config " + key
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		p := c.stack.Props
		detail, err := p.Set(ctx, key, value)
		if err != nil {
			return opResultMsg{label: c.workLabel, err: err.Error()}
		}
		if err := p.Restart(ctx); err != nil {
			return opResultMsg{label: c.workLabel, err: "queued (" + detail + ") but restart failed: " + err.Error()}
		}
		return opResultMsg{label: c.workLabel, detail: detail + " — restarted, new value active"}
	}
}

// View renders the key list with hot/cold markers, the selected key's help
// (help text + allowed values), the live-JSON pointer section, the edit
// field when open, and the key hints.
func (c *Config) View() string {
	lines := []string{titleStyle.Render("Config — server.properties"), ""}

	if c.working {
		f := string(configSpinnerFrames[c.spin%len(configSpinnerFrames)])
		lines = append(lines, promptStyle.Render("  "+f+" "+c.workLabel)+"…", "")
	}

	if c.rowsErr != "" {
		lines = append(lines, toastErr.Render("  "+c.rowsErr), "")
	}

	for i, row := range c.rows {
		marker := "cold"
		if row.Info.Hot {
			marker = "hot "
		}
		cursor := " "
		if i == c.cursor {
			cursor = ">"
		}
		lines = append(lines, fmt.Sprintf("  %s%-24s %-3s %s", cursor, row.Key+"=", marker, row.Value))
	}

	if len(c.rows) > 0 {
		sel := c.rows[c.cursor]
		lines = append(lines, "")
		lines = append(lines, hintStyle.Render("  "+sel.Info.Help))
		lines = append(lines, hintStyle.Render("  allowed: "+sel.Info.Allowed))
	}

	lines = append(lines, "", tileSection.Render("Live JSON configs (managed elsewhere)"))
	lines = append(lines, "  allowlist.json   gate + players — Access screen")
	lines = append(lines, "  permissions.json op / deop      — Players screen")

	if c.editing && c.cursor < len(c.rows) {
		lines = append(lines, "")
		lines = append(lines, promptStyle.Render("  "+c.rows[c.cursor].Key+": "+c.buf+"█"))
		if c.errIn != "" {
			lines = append(lines, toastErr.Render("  "+c.errIn))
		} else {
			lines = append(lines, hintStyle.Render("  Enter to apply    Esc to cancel"))
		}
	}

	lines = append(lines, "", hintStyle.Render("  ↑/↓ or j/k select    Enter edit    r restart    u refresh"))
	return strings.Join(lines, "\n")
}
