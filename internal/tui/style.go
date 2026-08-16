package tui

import "github.com/charmbracelet/lipgloss"

// Palette + styles shared across the shell, kept in one place so the look
// stays consistent as the real screens (tickets #5-#13) land.
var (
	colAccent = lipgloss.Color("#7c6df0")
	colOK     = lipgloss.Color("#4caf50")
	colErr    = lipgloss.Color("#e5484d")
	colMuted  = lipgloss.Color("#6b7280")
	colText   = lipgloss.Color("#e5e7eb")
	colBarBG  = lipgloss.Color("#151821")

	tabActive    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#0b0d12")).Background(colAccent).Padding(0, 1)
	tabInactive  = lipgloss.NewStyle().Foreground(colMuted).Padding(0, 1)
	barStyle     = lipgloss.NewStyle().Foreground(colText).Background(colBarBG).Padding(0, 1)
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(colText)
	hintStyle    = lipgloss.NewStyle().Foreground(colMuted)
	promptStyle  = lipgloss.NewStyle().Bold(true).Foreground(colText)
	stateRun     = lipgloss.NewStyle().Bold(true).Foreground(colOK)
	stateStop    = lipgloss.NewStyle().Bold(true).Foreground(colErr)
	stateUnknown = lipgloss.NewStyle().Foreground(colMuted)
	toastOK      = lipgloss.NewStyle().Foreground(colOK)
	toastInfo    = lipgloss.NewStyle().Foreground(colText)
	toastErr     = lipgloss.NewStyle().Foreground(colErr)
	confirmBox   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colAccent).Padding(1)

	// Dashboard (ticket #5) tile + panel styles.
	tileLabel   = lipgloss.NewStyle().Foreground(colMuted).Width(16)
	tileSection = lipgloss.NewStyle().Bold(true).Foreground(colAccent).MarginTop(1)
	eventOK     = lipgloss.NewStyle().Foreground(colOK)
	eventErr    = lipgloss.NewStyle().Foreground(colErr)
)
