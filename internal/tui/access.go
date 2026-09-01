// Package tui: the Access screen (ticket #7). It is a thin renderer over the
// compass Access service (D15): it displays Bedrock's deny mechanism — the
// allowlist gate, the current allowlist, and the recently-connected list —
// read-only by default. Every change is an explicit, tiered action:
//
//   - gate toggle (g)  → TierYes with a prominent "rejects every player not on
//     the list" consequence; the tool never auto-flips it (operator-onboarding
//     constraint);
//   - add by gamertag/XUID (a) → type the identity, Enter to confirm at
//     TierEnter; the server resolves gamertags to XUIDs itself (D3a);
//   - remove selected allowlist row (r) → TierTarget on the exact name;
//   - quick-add a recently-connected player (q) → one action, TierEnter;
//   - reload from disk (R) → TierEnter;
//   - block / unblock an IP (b / x) → type the IP; block is TierYes, unblock
//     TierEnter. When unprivileged the service returns the exact command and
//     the screen shows it instead of applying (graceful degradation).
//
// All console-touching work runs in goroutines behind a mutex (Console.Send is
// not concurrency-safe); file reads for display are safe to run alongside an
// in-flight action because writes are atomic temp+rename.
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

// accessHost is the shell the Access screen talks back to: confirmations (Ask)
// and transient notifications (Toast). *App implements it; a fake in tests keeps
// the screen decoupled from the concrete shell.
type accessHost interface {
	Ask(title, consequence string, tier Tier, target string, onConfirm func() tea.Cmd)
	Toast(msg string, kind ToastKind)
}

// inputMode selects which free-text field (if any) is active. The Access screen
// uses a single buffer for all three text inputs; the mode decides what Enter
// does with it and how it is labelled in View.
type inputMode int

const (
	inputNone  inputMode = iota
	inputAdd             // add to allowlist by gamertag/XUID
	inputBlock           // block an IP
	inputUnblock
)

// Access is the ticket #7 screen: the gate state, the current allowlist, and
// the recently-connected list, plus the tiered actions that change them.
type Access struct {
	host  accessHost
	stack *compass.Stack

	gate      compass.GateState
	gateErr   string // last gate read failure ("") when fresh
	entries   []compass.AllowEntry
	listErr   string // last allowlist read failure ("") when fresh
	recent    []compass.RecentPlayer
	recentErr string // last journal mine failure ("unavailable" degrades here)

	cursor int // selected row across the combined (allowlist + recent) list

	// In-flight operation (a blocking service call running in a goroutine).
	working   bool
	workLabel string
	spin      int

	// Free-text input state.
	mode  inputMode
	buf   string
	errIn string // validation error shown under the input line

	// consoleMu serializes every Console.Send: an action's live-apply/reload
	// must not interleave with another. Display refreshes read files + journal
	// only (no console), so they need no lock.
	consoleMu sync.Mutex
}

// NewAccess builds the Access screen over a resolved Stack. A nil stack (or one
// without an Access service) renders a degraded state; the first fetch is kicked
// off in Init.
func NewAccess(host accessHost, stack *compass.Stack) *Access {
	return &Access{host: host, stack: stack}
}

func (a *Access) Tab() string { return "Access" }

// CapturingKeys reports whether the Access screen owns raw keyboard input — i.e.
// a free-text field (add / block / unblock) is active. While true, the shell
// routes every key here and suppresses its global shortcuts so digits and other
// characters reach the field instead of jumping tabs or quitting. See KeyCaptor.
func (a *Access) CapturingKeys() bool { return a.mode != inputNone }

func (a *Access) Init() tea.Cmd { return a.refreshCmd() }

// serviceOK reports whether the screen can drive an action.
func (a *Access) serviceOK() bool {
	return a.stack != nil && a.stack.Access != nil
}

// accessRefreshMsg carries one async refresh's results (gate + allowlist +
// recent). Each field degrades independently so a partial failure still shows
// the rest of the state.
type accessRefreshMsg struct {
	gate      compass.GateState
	gateErr   string
	entries   []compass.AllowEntry
	listErr   string
	recent    []compass.RecentPlayer
	recentErr string
}

// refreshCmd starts one display refresh in a goroutine: read the gate, read the
// allowlist, and mine the journal for recently-connected players. It touches no
// console (file + journal reads only), so it is safe to run while an action is
// in flight — atomic writes mean a concurrent reader sees a complete file.
func (a *Access) refreshCmd() tea.Cmd {
	if !a.serviceOK() || a.working {
		return nil
	}
	svc := a.stack.Access
	players := a.stack.Players
	return func() tea.Msg {
		var m accessRefreshMsg

		g, gerr := svc.Gate()
		m.gate = g
		if gerr != nil {
			m.gateErr = gerr.Error()
		}

		e, lerr := svc.List()
		m.entries = e
		if lerr != nil {
			m.listErr = lerr.Error()
		}

		if players != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			rec, rerr := players.Recent(ctx)
			cancel()
			m.recent = rec
			if rerr != nil {
				m.recentErr = "unavailable (" + rerr.Error() + ")"
			}
		} else {
			m.recentErr = "unavailable (no journal)"
		}

		return m
	}
}

func (a *Access) ticks() tea.Cmd {
	if a.working {
		return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg {
			return serverSpinnerMsg{frame: a.spin}
		})
	}
	return nil
}

// Update handles the spinner tick, async refresh results, operation results,
// the shell's status poll (piggybacked refresh), and keys.
func (a *Access) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case serverSpinnerMsg:
		a.spin = (a.spin + 1) % len(backupSpinnerFrames)
		return a, a.ticks()
	case accessRefreshMsg:
		if m.gateErr != "" {
			a.gateErr = m.gateErr
		} else {
			a.gate = m.gate
			a.gateErr = ""
		}
		if m.listErr != "" {
			a.listErr = m.listErr
		} else {
			a.entries = m.entries
			a.listErr = ""
		}
		if m.recentErr != "" {
			a.recentErr = m.recentErr
		} else {
			a.recent = m.recent
			a.recentErr = ""
		}
		a.clampCursor()
		return a, a.ticks()
	case opResultMsg:
		a.working = false
		a.workLabel = ""
		if m.err != "" {
			a.host.Toast(m.label+": "+m.err, ToastError)
		} else if m.detail != "" {
			a.host.Toast(m.label+": "+m.detail, ToastSuccess)
		} else {
			a.host.Toast(m.label+": ok", ToastSuccess)
		}
		// Any mutation changes the world: refresh display state.
		return a, tea.Batch(a.ticks(), a.refreshCmd())
	case statusMsg:
		// The shell polls every 2 s; piggyback it so joins and gate/allowlist
		// edits made elsewhere show up without a manual refresh while active.
		if !a.working {
			return a, tea.Batch(a.ticks(), a.refreshCmd())
		}
		return a, a.ticks()
	case tea.KeyMsg:
		if a.working {
			return a, a.ticks() // an op owns the interaction
		}
		if a.mode != inputNone {
			return a.updateInputKey(m)
		}
		return a.updateKey(m)
	}
	return a, a.ticks()
}

// clampCursor keeps the cursor within the combined list bounds after a refresh.
func (a *Access) clampCursor() {
	n := len(a.entries) + len(a.recent)
	if n == 0 {
		a.cursor = 0
		return
	}
	if a.cursor >= n {
		a.cursor = n - 1
	}
}

// cursorTarget reports which row the cursor is on: kind is "allow", "recent",
// or "none"; identity is the gamertag/name of that row.
func (a *Access) cursorTarget() (kind, identity string) {
	if a.cursor < len(a.entries) {
		return "allow", a.entries[a.cursor].Name
	}
	idx := a.cursor - len(a.entries)
	if idx >= 0 && idx < len(a.recent) {
		return "recent", a.recent[idx].Name
	}
	return "none", ""
}

func (a *Access) updateKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.Type {
	case tea.KeyUp:
		if a.cursor > 0 {
			a.cursor--
		}
		return a, nil
	case tea.KeyDown:
		if a.cursor < len(a.entries)+len(a.recent)-1 {
			a.cursor++
		}
		return a, nil
	}
	switch {
	case isRune(key, 'g'):
		a.askGate()
	case isRune(key, 'a'):
		a.beginInput(inputAdd)
	case isRune(key, 'r'):
		a.askRemove()
	case isRune(key, 'q'):
		a.askQuickAdd()
	case isRune(key, 'R'):
		a.askReload()
	case isRune(key, 'b'):
		a.beginInput(inputBlock)
	case isRune(key, 'x'):
		a.beginInput(inputUnblock)
	case isRune(key, 'u'):
		if !a.serviceOK() || a.working {
			return a, nil
		}
		return a, tea.Batch(a.ticks(), a.refreshCmd())
	}
	return a, nil
}

// beginInput enters a free-text input mode with an empty buffer.
func (a *Access) beginInput(m inputMode) {
	if !a.serviceOK() {
		a.host.Toast("access unavailable (config did not resolve)", ToastError)
		return
	}
	a.mode = m
	a.buf = ""
	a.errIn = ""
}

// updateInputKey handles keys while a text field is active.
func (a *Access) updateInputKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.Type {
	case tea.KeyEsc:
		a.mode = inputNone
		a.buf = ""
		a.errIn = ""
		return a, nil
	case tea.KeyBackspace:
		if len(a.buf) > 0 {
			a.buf = a.buf[:len(a.buf)-1]
		}
		a.errIn = ""
		return a, nil
	case tea.KeyRunes:
		if len(key.Runes) > 0 && printable(key.Runes[0]) {
			a.buf += string(key.Runes[0])
			a.errIn = ""
		}
		return a, nil
	case tea.KeyEnter:
		val := strings.TrimSpace(a.buf)
		switch a.mode {
		case inputAdd:
			if val == "" {
				a.errIn = "type a gamertag or XUID first"
				return a, nil
			}
			a.mode = inputNone
			a.askAdd(val)
		case inputBlock:
			if val == "" {
				a.errIn = "type an IP address first"
				return a, nil
			}
			a.mode = inputNone
			a.askIP("block", val)
		case inputUnblock:
			if val == "" {
				a.errIn = "type an IP address first"
				return a, nil
			}
			a.mode = inputNone
			a.askIP("unblock", val)
		}
	}
	return a, nil
}

// askGate confirms the gate toggle at TierYes with a prominent consequence. The
// tool never flips it without this explicit action (operator-onboarding).
func (a *Access) askGate() {
	if !a.serviceOK() {
		a.host.Toast("access unavailable (config did not resolve)", ToastError)
		return
	}
	target := !a.gate.Enabled // the direction we are toggling TO
	var title, consequence string
	if target {
		title = "Enable allowlist gate"
		consequence = "This REJECTS EVERY player not on the allowlist — including you — until they are added. The server applies it immediately and keeps it after a restart."
	} else {
		title = "Disable allowlist gate"
		consequence = "Anyone can now join without being on the allowlist. This opens the world to all players."
	}
	a.host.Ask(
		title,
		consequence,
		TierYes,
		"",
		func() tea.Cmd {
			a.working = true
			dir := "on"
			if !target {
				dir = "off"
			}
			a.workLabel = "setting gate " + dir + "…"
			return runOp("gate", func(ctx context.Context) (string, error) {
				a.consoleMu.Lock()
				defer a.consoleMu.Unlock()
				return a.stack.Access.SetGate(ctx, target)
			})
		},
	)
}

// askAdd confirms adding the typed identity at TierEnter. The server resolves
// gamertags to XUIDs itself (D3a), so no XUID knowledge is required up front.
func (a *Access) askAdd(identity string) {
	a.host.Ask(
		"Add to allowlist",
		"Add "+identity+" to the allowlist (the join gate). The server resolves gamertags on first join.",
		TierEnter,
		identity,
		func() tea.Cmd {
			a.working = true
			a.workLabel = "adding " + identity + "…"
			return runOp("allow-add", func(ctx context.Context) (string, error) {
				a.consoleMu.Lock()
				defer a.consoleMu.Unlock()
				return a.stack.Access.Add(ctx, identity)
			})
		},
	)
}

// askRemove confirms removing the selected allowlist row at TierTarget on its
// exact name (destructive: it revokes that player's access).
func (a *Access) askRemove() {
	if !a.serviceOK() {
		a.host.Toast("access unavailable (config did not resolve)", ToastError)
		return
	}
	kind, identity := a.cursorTarget()
	if kind != "allow" || identity == "" {
		a.host.Toast("no allowlist row selected", ToastInfo)
		return
	}
	a.host.Ask(
		"Remove from allowlist",
		"Remove "+identity+" from the allowlist. They will be rejected at the gate on their next join.",
		TierTarget,
		identity,
		func() tea.Cmd {
			a.working = true
			a.workLabel = "removing " + identity + "…"
			return runOp("allow-remove", func(ctx context.Context) (string, error) {
				a.consoleMu.Lock()
				defer a.consoleMu.Unlock()
				return a.stack.Access.Remove(ctx, identity)
			})
		},
	)
}

// askQuickAdd adds the selected recently-connected player in one action
// (TierEnter). This is the ticket's "quick-add from the recently-connected list
// is a single action".
func (a *Access) askQuickAdd() {
	if !a.serviceOK() {
		a.host.Toast("access unavailable (config did not resolve)", ToastError)
		return
	}
	kind, identity := a.cursorTarget()
	if kind != "recent" || identity == "" {
		a.host.Toast("no recently-connected player selected", ToastInfo)
		return
	}
	a.host.Ask(
		"Quick-add to allowlist",
		"Add "+identity+" (recently connected) to the allowlist.",
		TierEnter,
		identity,
		func() tea.Cmd {
			a.working = true
			a.workLabel = "quick-adding " + identity + "…"
			return runOp("allow-add", func(ctx context.Context) (string, error) {
				a.consoleMu.Lock()
				defer a.consoleMu.Unlock()
				return a.stack.Access.Add(ctx, identity)
			})
		},
	)
}

// askReload confirms re-reading allowlist.json into the running server.
func (a *Access) askReload() {
	if !a.serviceOK() {
		a.host.Toast("access unavailable (config did not resolve)", ToastError)
		return
	}
	a.host.Ask(
		"Reload allowlist",
		"Re-read allowlist.json into the running server and verify it took effect.",
		TierEnter,
		"",
		func() tea.Cmd {
			a.working = true
			a.workLabel = "reloading allowlist…"
			return runOp("reload", func(ctx context.Context) (string, error) {
				a.consoleMu.Lock()
				defer a.consoleMu.Unlock()
				return a.stack.Access.Reload(ctx)
			})
		},
	)
}

// askIP confirms an IP block/unblock. Block is TierYes (it can lock out traffic);
// unblock is TierEnter (removing a restriction is safe). When the service lacks
// permission to apply, it returns the exact command and the screen shows it.
func (a *Access) askIP(action, ip string) {
	if !a.serviceOK() {
		a.host.Toast("access unavailable (config did not resolve)", ToastError)
		return
	}
	var title, consequence string
	tier := TierEnter
	switch action {
	case "block":
		title = "Block IP"
		consequence = "Add a host-firewall deny rule for " + ip + " on the server's UDP port. This can lock out legitimate players from that address."
		tier = TierYes
	default: // unblock
		title = "Unblock IP"
		consequence = "Remove the host-firewall deny rule for " + ip + "."
	}
	a.host.Ask(
		title,
		consequence,
		tier,
		ip,
		func() tea.Cmd {
			a.working = true
			a.workLabel = action + " " + ip + "…"
			return runOp(action+"-ip", func(ctx context.Context) (string, error) {
				var res *compass.IPBlockResult
				var err error
				if action == "block" {
					res, err = a.stack.Access.BlockIP(ctx, ip)
				} else {
					res, err = a.stack.Access.UnblockIP(ctx, ip)
				}
				if err != nil {
					return "", err
				}
				if res.Applied {
					return action + " applied", nil
				}
				// Graceful degradation: not applied — surface the exact command.
				// Returned as detail so opResultMsg toasts it on the Update path
				// (never from this goroutine). Single line so wordWrap stays clean.
				return "not applied (" + res.Reason + ") — run: " + res.Command, nil
			})
		},
	)
}

// allowedSet returns the set of allowlist names (lower-cased) for annotating
// recent rows that are already allowed.
func (a *Access) allowedSet() map[string]bool {
	s := make(map[string]bool, len(a.entries))
	for _, e := range a.entries {
		s[strings.ToLower(e.Name)] = true
	}
	return s
}

// View renders the gate state, the allowlist, the recently-connected list, any
// active input field, and the key hints.
func (a *Access) View() string {
	lines := []string{titleStyle.Render("Access"), ""}

	if a.working {
		f := string(backupSpinnerFrames[a.spin%len(backupSpinnerFrames)])
		lines = append(lines, promptStyle.Render("  "+f+" "+a.workLabel), "")
	}

	if !a.serviceOK() {
		lines = append(lines, toastErr.Render("  access unavailable (config did not resolve)"), "")
		lines = append(lines, hintStyle.Render("  g gate    a add    r remove    q quick-add    R reload    b block IP    x unblock IP    u refresh"))
		return join(lines)
	}

	// Gate.
	if a.gateErr != "" {
		lines = append(lines, tileSection.Render("Gate"), toastErr.Render("  "+a.gateErr), "")
	} else {
		state := "OFF"
		gateLine := "  allow-list = " + hintStyle.Render(state)
		if a.gate.Enabled {
			gateLine = "  allow-list = " + promptStyle.Render("ON")
		}
		lines = append(lines, tileSection.Render("Gate"), gateLine)
		if a.gate.Source != "" {
			lines = append(lines, hintStyle.Render("  source: "+a.gate.Source))
		}
		if a.gate.Enabled {
			lines = append(lines, toastErr.Render("  Every player not on the allowlist is rejected."))
		} else {
			lines = append(lines, hintStyle.Render("  (gate off — anyone may join)"))
		}
		lines = append(lines, "")
	}

	// Allowlist.
	if a.listErr != "" {
		lines = append(lines, tileSection.Render("Allowlist"), toastErr.Render("  "+a.listErr), "")
	} else if len(a.entries) == 0 {
		lines = append(lines, tileSection.Render("Allowlist (0)"), hintStyle.Render("  (empty — nobody is allowed)"), "")
	} else {
		lines = append(lines, tileSection.Render(fmt.Sprintf("Allowlist (%d)", len(a.entries))))
		for i, e := range a.entries {
			mark := "  "
			if i == a.cursor {
				mark = promptStyle.Render("▸ ")
			}
			row := mark + e.Name
			lines = append(lines, row)
		}
		lines = append(lines, "")
	}

	// Recently connected.
	if a.recentErr != "" {
		lines = append(lines, tileSection.Render("Recently connected"), hintStyle.Render("  "+a.recentErr), "")
	} else if len(a.recent) == 0 {
		lines = append(lines, tileSection.Render("Recently connected (0)"), hintStyle.Render("  (no player activity in the journal tail)"), "")
	} else {
		allowed := a.allowedSet()
		lines = append(lines, tileSection.Render(fmt.Sprintf("Recently connected (%d)", len(a.recent))))
		for i, rp := range a.recent {
			rowIdx := len(a.entries) + i
			mark := "  "
			if rowIdx == a.cursor {
				mark = promptStyle.Render("▸ ")
			}
			state := "left"
			if rp.State == "connected" {
				state = "online"
			}
			row := mark + rp.Name
			meta := state
			if allowed[strings.ToLower(rp.Name)] {
				meta += "  (allowed)"
			}
			if !rp.LastSeen.IsZero() {
				meta += "  " + rp.LastSeen.Format("2006-01-02 15:04")
			}
			lines = append(lines, row+hintStyle.Render("   "+meta))
		}
		lines = append(lines, "")
	}

	// Active input field.
	switch a.mode {
	case inputAdd:
		lines = append(lines, promptStyle.Render("  Add to allowlist (gamertag or XUID): "+a.buf+"█"))
		if a.errIn != "" {
			lines = append(lines, toastErr.Render("  "+a.errIn))
		} else {
			lines = append(lines, hintStyle.Render("  Enter to confirm · Esc to cancel"))
		}
	case inputBlock:
		lines = append(lines, promptStyle.Render("  Block IP: "+a.buf+"█"))
		if a.errIn != "" {
			lines = append(lines, toastErr.Render("  "+a.errIn))
		} else {
			lines = append(lines, hintStyle.Render("  Enter to confirm · Esc to cancel"))
		}
	case inputUnblock:
		lines = append(lines, promptStyle.Render("  Unblock IP: "+a.buf+"█"))
		if a.errIn != "" {
			lines = append(lines, toastErr.Render("  "+a.errIn))
		} else {
			lines = append(lines, hintStyle.Render("  Enter to confirm · Esc to cancel"))
		}
	}

	lines = append(lines, hintStyle.Render("  ↑/↓ select    g gate    a add    r remove    q quick-add    R reload    b block IP    x unblock IP    u refresh"))
	return join(lines)
}
