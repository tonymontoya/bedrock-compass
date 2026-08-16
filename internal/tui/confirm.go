package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Tier is the confirmation strength for a mutating action (D11). Every tier
// shows the consequence before the input is accepted.
type Tier int

const (
	TierEnter  Tier = iota // trivial action: Enter confirms
	TierYes                // risky action: type YES
	TierTarget             // destructive: type the exact target
)

type confirmResult int

const (
	confirmPending confirmResult = iota
	confirmOK
	confirmCancel
)

// Confirm is the confirmation overlay. It captures all keys until dismissed;
// the shell reads Confirm.Result to learn the outcome.
type Confirm struct {
	Title       string
	Consequence string
	Tier        Tier
	Target      string // required when Tier == TierTarget

	buffer string
	err    string
	result confirmResult
}

// NewConfirm builds a confirmation overlay for one action.
func NewConfirm(title, consequence string, tier Tier, target string) *Confirm {
	return &Confirm{Title: title, Consequence: consequence, Tier: tier, Target: target}
}

// Prompt returns the input instruction for the current tier.
func (c *Confirm) Prompt() string {
	switch c.Tier {
	case TierYes:
		return `Type "YES" to confirm`
	case TierTarget:
		return "Type " + c.Target + " to confirm"
	default:
		return "Press Enter to confirm"
	}
}

// Update consumes one key. It returns the (possibly new) overlay and no
// command — the shell inspects Result.
func (c *Confirm) Update(msg tea.Msg) *Confirm {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return c
	}
	switch key.Type {
	case tea.KeyEnter:
		if c.acceptable(strings.TrimSpace(c.buffer)) {
			c.result = confirmOK
		} else {
			c.err = "input does not confirm this action"
		}
	case tea.KeyEsc:
		c.result = confirmCancel
	case tea.KeyBackspace:
		if len(c.buffer) > 0 {
			c.buffer = c.buffer[:len(c.buffer)-1]
			c.err = ""
		}
	case tea.KeyRunes:
		if len(key.Runes) > 0 && printable(key.Runes[0]) {
			c.buffer += string(key.Runes[0])
			c.err = ""
		}
	}
	return c
}

// acceptable reports whether buf satisfies the tier.
func (c *Confirm) acceptable(buf string) bool {
	switch c.Tier {
	case TierEnter:
		return true
	case TierYes:
		return buf == "YES"
	case TierTarget:
		return buf == c.Target
	}
	return false
}

// View renders the overlay: title, consequence, prompt + typed buffer, error.
func (c *Confirm) View(width int) string {
	var lines []string
	lines = append(lines, titleStyle.Render(c.Title))
	lines = append(lines, c.Consequence)
	lines = append(lines, "")
	lines = append(lines, promptStyle.Render(c.Prompt())+": "+c.buffer)
	if c.err != "" {
		lines = append(lines, toastErr.Render(c.err))
	}
	lines = append(lines, "")
	lines = append(lines, hintStyle.Render("Enter to confirm · Esc to cancel"))
	return confirmBox.MaxWidth(64).Render(strings.Join(lines, "\n"))
}

func printable(r rune) bool { return r >= 0x20 && r != 0x7f }
