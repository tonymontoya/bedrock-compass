// Package tui renders bedrock-compass over a bubbletea program. It imports
// the domain core (compass) for the status it displays, but the core never
// imports this package (D15).
package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// Host is the shell the dashboard talks back to: it requests confirmations
// (Ask) and routes the operator to other tabs (GoTo). *App implements it; a
// fake in tests keeps the dashboard decoupled from the concrete shell.
type Host interface {
	Ask(title, consequence string, tier Tier, target string, onConfirm func() tea.Cmd)
	GoTo(label string)
}

// QuickAction is a dashboard shortcut (D10): it confirms at the given tier and
// then routes the operator to the tab that owns the action.
type QuickAction struct {
	Key         rune   // key that triggers it, e.g. 'b'
	Label       string // human label, e.g. "Backup now"
	Tier        Tier   // confirmation tier (D10/D11)
	Target      string // required for TierTarget
	Consequence string // shown in the confirmation overlay
	RouteTo     string // owning tab label (e.g. "Backups")
}

func defaultQuickActions() []QuickAction {
	return []QuickAction{
		{
			Key:         'b',
			Label:       "Backup now",
			Tier:        TierEnter,
			Consequence: "Create a live backup: hold saves, copy the world, then resume.",
			RouteTo:     "Backups",
		},
		{
			Key:         'r',
			Label:       "Restart server",
			Tier:        TierYes,
			Consequence: "Restart the server; online players are disconnected.",
			RouteTo:     "Server",
		},
	}
}

// Dashboard is the default screen (D10): live telemetry tiles, a recent-
// operations panel fed by the audit trail, and quick actions that confirm and
// route to the owning tab.
type Dashboard struct {
	host    Host
	events  func() []compass.Event
	status  compass.Status
	recent  []compass.Event
	actions []QuickAction
}

// NewDashboard builds a Dashboard. It receives live status from the shell
// (the App polls and broadcasts a statusMsg); events returns the recent audit
// records (may be nil). Quick actions confirm via host and then route through it.
func NewDashboard(host Host, events func() []compass.Event) *Dashboard {
	return &Dashboard{
		host:    host,
		events:  events,
		actions: defaultQuickActions(),
	}
}

func (d *Dashboard) Tab() string { return "Dashboard" }

// Init is a no-op: the shell (App) is the single poller and broadcasts a
// statusMsg to the active screen every 2 s.
func (d *Dashboard) Init() tea.Cmd { return nil }

// Update handles the shell's status broadcast and the quick-action keys.
func (d *Dashboard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case statusMsg:
		d.status = compass.Status(m)
		if d.events != nil {
			d.recent = d.events()
		}
		return d, nil
	case tea.KeyMsg:
		return d.updateKey(m)
	}
	return d, nil
}

func (d *Dashboard) updateKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	for i := range d.actions {
		a := d.actions[i]
		if isRune(key, a.Key) {
			d.host.Ask(a.Label, a.Consequence, a.Tier, a.Target, func() tea.Cmd {
				d.host.GoTo(a.RouteTo)
				return nil
			})
			return d, nil
		}
	}
	return d, nil
}

// View renders the tiles, the recent-operations panel, and the quick actions.
func (d *Dashboard) View() string {
	s := d.status
	lines := []string{titleStyle.Render("Dashboard"), ""}
	lines = append(lines,
		tileSection.Render("Server"),
		row("State", stateTile(s.State)),
		row("Name", orDash(s.Name)),
		row("Version", orDash(s.Version)),
		row("MOTD", orDash(s.MOTD)),
		row("Uptime", uptimeTile(s.Uptime)),
		row("Players", playersTile(s)),
		"",
		tileSection.Render("Host"),
		row("CPU", cpuTile(s.CPUPct)),
		row("RAM", ramTile(s.RAMUsed, s.RAMTotal)),
		row("Server RAM", rssTile(s.RSS)),
		row("Disk", diskTile(s.DiskUsed, s.DiskTotal)),
		row("Net", netTile(s.NetUp, s.NetDown)),
		"",
		tileSection.Render("Recent operations"),
	)
	lines = append(lines, d.recentView()...)
	lines = append(lines, "", tileSection.Render("Quick actions"))
	for _, a := range d.actions {
		lines = append(lines, fmt.Sprintf("  [%c] %s", a.Key, a.Label))
	}
	return strings.Join(lines, "\n")
}

// recentView renders the last few audit records (verb, params, result,
// duration). compass.Trail.Recent returns them newest-first, so the most
// recent operation is at the top.
func (d *Dashboard) recentView() []string {
	if len(d.recent) == 0 {
		return []string{hintStyle.Render("  (no operations yet)")}
	}
	lines := make([]string, 0, len(d.recent))
	for _, ev := range d.recent {
		resStyle, res := eventOK, ev.Result
		if strings.HasPrefix(ev.Result, "error") {
			resStyle, res = eventErr, ev.Result
		}
		lines = append(lines, fmt.Sprintf("  %s  %s%s  %s  %s",
			ev.Time.Format("15:04:05"),
			ev.Verb, paramsTail(ev.Params),
			resStyle.Render(res),
			durTile(ev.Duration)))
	}
	return lines
}

// --- tile value helpers (pure formatting; no invented metrics, D8) ---

func row(label, value string) string {
	return "  " + tileLabel.Render(label) + " " + value
}

func stateTile(state string) string {
	switch state {
	case "running":
		return stateRun.Render("running")
	case "stopped":
		return stateStop.Render("stopped")
	default:
		return stateUnknown.Render("unknown")
	}
}

func uptimeTile(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return d.Round(time.Second).String()
}

func playersTile(s compass.Status) string {
	switch {
	case s.Max > 0:
		return fmt.Sprintf("%d / %d", s.Online, s.Max)
	case s.Online > 0:
		return fmt.Sprintf("%d online", s.Online)
	default:
		return "-"
	}
}

func cpuTile(pct float64) string {
	if pct <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", pct)
}

func ramTile(used, total uint64) string {
	if total == 0 {
		return "-"
	}
	return fmt.Sprintf("%s / %s", humanBytes(used), humanBytes(total))
}

func rssTile(b uint64) string {
	if b == 0 {
		return "-"
	}
	return humanBytes(b)
}

func diskTile(used, total uint64) string {
	if total == 0 {
		return "-"
	}
	return fmt.Sprintf("%s / %s", humanBytes(used), humanBytes(total))
}

func netTile(up, down uint64) string {
	if up == 0 && down == 0 {
		return "idle"
	}
	return fmt.Sprintf("up %s/s  down %s/s", humanBytes(up), humanBytes(down))
}

func durTile(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return d.Round(time.Millisecond).String()
}

func paramsTail(p map[string]string) string {
	if len(p) == 0 {
		return ""
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(p))
	for _, k := range keys {
		parts = append(parts, k+"="+p[k])
	}
	return " (" + strings.Join(parts, " ") + ")"
}
