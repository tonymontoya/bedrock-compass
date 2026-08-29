// Package tui: the Players screen (ticket #6). It is a thin renderer over
// the compass Players service (D15): it shows the live roster (`list`) and
// the recently-connected list mined from the journal (D3b), and drives the
// per-row actions kick / op / deop / allow — each confirmed at TierEnter
// (D11: "allowlist/op/kick-one"), run in a goroutine with an indeterminate
// spinner, toasted on completion, and audit-logged by the service. The
// screen never asks for an XUID (D3b): rows carry gamertag-first identity.
//
// Roster freshness without manual refresh: the shell polls status every 2 s
// and broadcasts it to the active screen; this screen piggybacks that tick
// to re-poll `list` + the journal tail while its tab is active (same pattern
// as the Server screen's pong probe). All console work runs in goroutines —
// Console.Send is not concurrency-safe, so a mutex serializes roster polls
// against in-flight actions.
package tui

import (
	"context"
	"fmt"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// playersHost is the shell the Players screen talks back to: confirmations
// (Ask) and transient notifications (Toast). *App implements it; a fake in
// tests keeps the screen decoupled from the concrete shell.
type playersHost interface {
	Ask(title, consequence string, tier Tier, target string, onConfirm func() tea.Cmd)
	Toast(msg string, kind ToastKind)
}

// Players is the ticket #6 screen: the live roster (online name + XUID via
// `list`), the recently-connected list from journal connect lines (name +
// XUID, survives the player leaving), and per-row kick / op / deop / allow.
type Players struct {
	host  playersHost
	stack *compass.Stack

	roster compass.Roster
	recent []compass.RecentPlayer
	cursor int // selected roster row

	rosterErr string // last `list` failure ("" when fresh)
	recentErr string // last journal mine failure ("unavailable" degrades here)

	// In-flight operation (a blocking console call running in a goroutine).
	working   bool
	workLabel string
	spin      int

	// consoleMu serializes every Console.Send: the roster poll and an
	// action must never harvest each other's output.
	consoleMu sync.Mutex
}

// NewPlayers builds the Players screen over a resolved Stack. A nil stack (or
// one without a Players service) renders a degraded state; the first roster
// + recent fetch is kicked off in Init.
func NewPlayers(host playersHost, stack *compass.Stack) *Players {
	return &Players{host: host, stack: stack}
}

func (p *Players) Tab() string { return "Players" }

func (p *Players) Init() tea.Cmd { return tea.Batch(p.pollCmds(), p.recentCmd()) }

// serviceOK reports whether the screen can drive a console action.
func (p *Players) serviceOK() bool {
	return p.stack != nil && p.stack.Players != nil
}

// playersRosterMsg carries one async `list` round-trip's result.
type playersRosterMsg struct {
	roster compass.Roster
	err    string
}

// playersRecentMsg carries one async journal mine's result.
type playersRecentMsg struct {
	recent []compass.RecentPlayer
	err    string
}

// pollCmds starts the roster fetch (async; it shares the console mutex, so
// it serializes with any in-flight action). The recent-list fetch is chained
// from the roster result — one command per Update keeps the screen's message
// flow simple and testable.
func (p *Players) pollCmds() tea.Cmd {
	if !p.serviceOK() || p.working {
		return nil
	}
	return func() tea.Msg {
		p.consoleMu.Lock()
		defer p.consoleMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		r, err := p.stack.Players.Roster(ctx)
		if err != nil {
			return playersRosterMsg{err: err.Error()}
		}
		return playersRosterMsg{roster: r}
	}
}

// recentCmd starts the journal mine (no console involved — it reads the
// unit's journal, so no mutex needed).
func (p *Players) recentCmd() tea.Cmd {
	if !p.serviceOK() || p.working {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rec, err := p.stack.Players.Recent(ctx)
		if err != nil {
			return playersRecentMsg{err: "unavailable (" + err.Error() + ")"}
		}
		return playersRecentMsg{recent: rec}
	}
}

func (p *Players) ticks() tea.Cmd {
	if p.working {
		return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg {
			return serverSpinnerMsg{frame: p.spin}
		})
	}
	return nil
}

// Update handles the spinner tick, async results, the shell's status poll
// (piggybacked roster refresh), and keys.
func (p *Players) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case serverSpinnerMsg:
		p.spin = (p.spin + 1) % len(backupSpinnerFrames)
		return p, p.ticks()
	case playersRosterMsg:
		if m.err != "" {
			p.rosterErr = m.err
		} else {
			p.roster = m.roster
			p.rosterErr = ""
			if p.cursor >= len(p.roster.Online) {
				p.cursor = 0
			}
		}
		// Chain the journal mine so one poll cycle refreshes both lists.
		return p, tea.Batch(p.ticks(), p.recentCmd())
	case playersRecentMsg:
		if m.err != "" {
			p.recentErr = m.err
		} else {
			p.recent = m.recent
			p.recentErr = ""
		}
		return p, p.ticks()
	case opResultMsg:
		p.working = false
		p.workLabel = ""
		if m.err != "" {
			p.host.Toast(m.label+": "+m.err, ToastError)
		} else {
			kind := ToastSuccess
			if m.detail != "" {
				p.host.Toast(m.label+": "+m.detail, kind)
			} else {
				p.host.Toast(m.label+": ok", kind)
			}
		}
		// A kick/op/deop/allow changes the world: re-poll both lists.
		return p, tea.Batch(p.pollCmds(), p.recentCmd())
	case statusMsg:
		// The shell polls every 2 s; piggyback it so joins and leaves show
		// up without a manual refresh while this tab is active.
		if !p.working {
			return p, tea.Batch(p.ticks(), p.pollCmds())
		}
		return p, p.ticks()
	case tea.KeyMsg:
		if p.working {
			return p, p.ticks() // an op owns the interaction
		}
		return p.updateKey(m)
	}
	return p, p.ticks()
}

func (p *Players) updateKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.Type {
	case tea.KeyUp:
		if p.cursor > 0 {
			p.cursor--
		}
		return p, nil
	case tea.KeyDown:
		if p.cursor < len(p.roster.Online)-1 {
			p.cursor++
		}
		return p, nil
	}
	switch {
	case isRune(key, 'k'):
		p.askAction("kick", "Kick player", "Disconnect the selected player from the world.", func(ctx context.Context, pl compass.Player) (string, error) {
			return p.stack.Players.Kick(ctx, pl)
		})
	case isRune(key, 'o'):
		p.askAction("op", "Make operator", "Grant operator status to the selected player.", func(ctx context.Context, pl compass.Player) (string, error) {
			return p.stack.Players.Op(ctx, pl)
		})
	case isRune(key, 'd'):
		p.askAction("deop", "Remove operator", "Remove operator status from the selected player.", func(ctx context.Context, pl compass.Player) (string, error) {
			return p.stack.Players.Deop(ctx, pl)
		})
	case isRune(key, 'a'):
		p.askAllow()
	case isRune(key, 'u'):
		if !p.serviceOK() || p.working {
			return p, nil
		}
		return p, tea.Batch(p.pollCmds(), p.recentCmd())
	}
	return p, nil
}

// selected returns the player under the cursor (zero value when nobody is
// online — callers guard with Name != "").
func (p *Players) selected() compass.Player {
	if p.cursor < 0 || p.cursor >= len(p.roster.Online) {
		return compass.Player{}
	}
	return p.roster.Online[p.cursor]
}

// askAction confirms one per-row action at TierEnter (D11: allowlist/op/
// kick-one are the Enter tier) and runs it through runOp on confirm. The
// target is the gamertag, so the confirmation shows exactly who is affected.
// The selected player is captured at confirm time — the cursor may move by
// the time the goroutine runs — and the console mutex serializes the action
// against any roster poll (Console.Send is not concurrency-safe).
func (p *Players) askAction(label, title, consequence string, fn func(ctx context.Context, pl compass.Player) (string, error)) {
	if !p.serviceOK() {
		p.host.Toast("players unavailable (config did not resolve)", ToastError)
		return
	}
	pl := p.selected()
	if pl.Name == "" {
		p.host.Toast("no player selected", ToastInfo)
		return
	}
	target := pl.Name
	p.host.Ask(
		title,
		consequence+" Target: "+target+".",
		TierEnter,
		target,
		func() tea.Cmd {
			p.working = true
			p.workLabel = label + " " + target + "…"
			return runOp(label, func(ctx context.Context) (string, error) {
				p.consoleMu.Lock()
				defer p.consoleMu.Unlock()
				return fn(ctx, pl)
			})
		},
	)
}

// askAllow confirms the allowlist add (TierEnter per D11). The identity is
// gamertag-first: the service sends `allowlist add <gamertag>` and hands the
// same Player to whoever consumes it next — ticket #7's Access screen builds
// the full flow around this handoff.
func (p *Players) askAllow() {
	if !p.serviceOK() {
		p.host.Toast("players unavailable (config did not resolve)", ToastError)
		return
	}
	pl := p.selected()
	if pl.Name == "" {
		p.host.Toast("no player selected", ToastInfo)
		return
	}
	target := pl.Name
	p.host.Ask(
		"Allow player",
		"Add the selected player to the allowlist (the join gate). Target: "+target+".",
		TierEnter,
		target,
		func() tea.Cmd {
			p.working = true
			p.workLabel = "allowing " + target + "…"
			return runOp("allow", func(ctx context.Context) (string, error) {
				p.consoleMu.Lock()
				defer p.consoleMu.Unlock()
				return p.stack.Players.Allow(ctx, pl)
			})
		},
	)
}

// View renders the roster (cursor row marked), the recently-connected list,
// and the key hints.
func (p *Players) View() string {
	lines := []string{titleStyle.Render("Players"), ""}

	if p.working {
		f := string(backupSpinnerFrames[p.spin%len(backupSpinnerFrames)])
		lines = append(lines, promptStyle.Render("  "+f+" "+p.workLabel), "")
	}

	if !p.serviceOK() {
		lines = append(lines, toastErr.Render("  players unavailable (config did not resolve)"), "")
		lines = append(lines, hintStyle.Render("  k kick    o op    d deop    a allow    u refresh"))
		return join(lines)
	}

	// Live roster.
	if p.rosterErr != "" {
		lines = append(lines, tileSection.Render("Online"), toastErr.Render("  "+p.rosterErr), "")
	} else {
		header := fmt.Sprintf("%d / %d players online", p.roster.Count, p.roster.Max)
		lines = append(lines, tileSection.Render("Online ("+header+")"))
		if len(p.roster.Online) == 0 {
			lines = append(lines, hintStyle.Render("  (nobody in the world)"))
		} else {
			for i, pl := range p.roster.Online {
				mark := "  "
				if i == p.cursor {
					mark = promptStyle.Render("▸ ")
				}
				row := mark + pl.Name
				if pl.XUID != "" {
					row += hintStyle.Render("   xuid " + pl.XUID)
				}
				lines = append(lines, row)
			}
		}
		lines = append(lines, "")
	}

	// Recently connected (journal-mined; survives the player leaving).
	if p.recentErr != "" {
		lines = append(lines, tileSection.Render("Recently connected"), hintStyle.Render("  "+p.recentErr), "")
	} else if len(p.recent) == 0 {
		lines = append(lines, tileSection.Render("Recently connected"), hintStyle.Render("  (no player activity in the journal tail)"), "")
	} else {
		lines = append(lines, tileSection.Render(fmt.Sprintf("Recently connected (%d)", len(p.recent))))
		for _, rp := range p.recent {
			state := "left"
			if rp.State == "connected" {
				state = "online"
			}
			when := ""
			if !rp.LastSeen.IsZero() {
				when = rp.LastSeen.Format("2006-01-02 15:04")
			}
			row := "  " + rp.Name
			if rp.XUID != "" {
				row += hintStyle.Render("   xuid " + rp.XUID)
			}
			meta := state
			if when != "" {
				meta = meta + "  " + when
			}
			lines = append(lines, row+hintStyle.Render("   "+meta))
		}
		lines = append(lines, "")
	}

	lines = append(lines, hintStyle.Render("  ↑/↓ select    k kick    o op    d deop    a allow    u refresh"))
	return join(lines)
}
