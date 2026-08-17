// Package tui: the Backups screen (ticket #10). It is a thin renderer over
// the compass core (D15): it calls the engine + timer and shows their
// results, reusing the shell's confirmation tiers and toasts. The six
// surfaces are List, Create (live/stopped), Restore, Prune, Recovery, and
// the scheduled Timer. Engine operations are blocking with no progress
// callbacks, so each runs in a goroutine and the screen shows an
// indeterminate spinner until the result message returns.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// backupsHost is the shell the Backups screen talks back to: confirmations
// (Ask) and transient notifications (Toast). *App implements it; a fake in
// tests keeps the screen decoupled from the concrete shell.
type backupsHost interface {
	Ask(title, consequence string, tier Tier, target string, onConfirm func() tea.Cmd)
	Toast(msg string, kind ToastKind)
}

// backupsSpinnerMsg advances the indeterminate spinner while an operation is
// in flight.
type backupsSpinnerMsg struct{ frame int }

const backupSpinnerFrames = "|/-\\"

// Backups is the ticket #10 screen: the backup list, the create/restore/
// prune actions, the interrupted-operation recovery banner, and the
// scheduled-backup timer panel.
type Backups struct {
	host  backupsHost
	stack *compass.Stack

	backups     []compass.Backup
	cursor      int
	recovery    *compass.RecoveryStatus
	timerStatus *compass.TimerStatus
	timerErr    string

	// In-flight operation (blocking engine call running in a goroutine).
	working   bool
	workLabel string
	spin      int

	// Timer-setup form state.
	timerMode bool
	cadence   string // "daily" | "weekly"
	weekday   int    // 0=Mon .. 6=Sun (weekly only)
	timeBuf   string // "HH:MM" being typed
	mode      string // "live" | "stopped"
}

// NewBackups builds the Backups screen over a resolved Stack. A nil stack
// renders a degraded "backups unavailable" state.
func NewBackups(host backupsHost, stack *compass.Stack) *Backups {
	b := &Backups{
		host:    host,
		stack:   stack,
		cadence: "daily",
		mode:    compass.ModeLive,
	}
	b.refresh()
	return b
}

func (b *Backups) Tab() string { return "Backups" }

func (b *Backups) Init() tea.Cmd { return b.ticks() }

// refresh pulls the list, the recovery banner, and the timer status. It is
// read-only and cheap; a nil stack or engine yields empty state, not an error.
func (b *Backups) refresh() {
	if b.stack == nil || b.stack.Engine == nil {
		return
	}
	if list, err := b.stack.Engine.List(); err == nil {
		b.backups = list
		if b.cursor >= len(b.backups) {
			b.cursor = 0
		}
	}
	if st, err := b.stack.Engine.CheckRecovery(); err == nil {
		b.recovery = st
	}
	if b.stack.Timer != nil {
		if st, err := b.stack.Timer.Status(context.Background()); err == nil {
			b.timerStatus = st
			b.timerErr = ""
		} else {
			b.timerErr = err.Error()
		}
	}
}

func (b *Backups) ticks() tea.Cmd {
	if b.working {
		return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg {
			return backupsSpinnerMsg{frame: b.spin}
		})
	}
	return nil
}

// Update handles the spinner tick, the operation-result message, and keys.
func (b *Backups) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case backupsSpinnerMsg:
		b.spin = (b.spin + 1) % len(backupSpinnerFrames)
		return b, b.ticks()
	case opResultMsg:
		b.working = false
		b.workLabel = ""
		if m.err != "" {
			b.host.Toast(m.label+": "+m.err, ToastError)
		} else {
			kind := ToastSuccess
			if m.detail != "" {
				b.host.Toast(m.label+": "+m.detail, kind)
			} else {
				b.host.Toast(m.label+": ok", kind)
			}
		}
		b.refresh()
		return b, b.ticks()
	case tea.KeyMsg:
		if b.working {
			return b, b.ticks() // an op owns the interaction
		}
		return b.updateKey(m)
	}
	return b, b.ticks()
}

// opResultMsg is delivered by an in-flight operation's command.
type opResultMsg struct {
	label  string
	detail string
	err    string
}

// runOp wraps a blocking engine/timer call in a tea.Cmd that reports back an
// opResultMsg. The label is shown in the toast; detail is a short success
// note (e.g. the new backup id).
func runOp(label string, fn func(ctx context.Context) (string, error)) tea.Cmd {
	return func() tea.Msg {
		detail, err := fn(context.Background())
		if err != nil {
			return opResultMsg{label: label, err: err.Error()}
		}
		return opResultMsg{label: label, detail: detail}
	}
}

func (b *Backups) updateKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if b.timerMode {
		return b.updateTimerKey(key)
	}
	switch {
	case isRune(key, 'b'):
		b.askBackup(compass.ModeLive)
	case isRune(key, 's'):
		b.askBackup(compass.ModeStopped)
	case isRune(key, 'r'):
		b.askRestore()
	case isRune(key, 'p'):
		b.askPrune()
	case isRune(key, 'x'):
		b.askRecover()
	case isRune(key, 't'):
		b.enterTimer()
	case isRune(key, 'D'):
		b.askTimerDisable()
	case key.Type == tea.KeyUp || isRune(key, 'k'):
		if b.cursor > 0 {
			b.cursor--
		}
	case key.Type == tea.KeyDown || isRune(key, 'j'):
		if b.cursor < len(b.backups)-1 {
			b.cursor++
		}
	}
	return b, b.ticks()
}

func (b *Backups) engineOK() bool {
	return b.stack != nil && b.stack.Engine != nil
}

func (b *Backups) askBackup(mode string) {
	if !b.engineOK() {
		b.host.Toast("backups unavailable (config did not resolve)", ToastError)
		return
	}
	consequence := "Create a live backup: hold saves, copy the world, then resume."
	if mode == compass.ModeStopped {
		consequence = "Create a stopped backup: stop the server, cold-copy the world, then restart."
	}
	b.host.Ask(
		"Backup now ("+mode+")",
		consequence,
		TierEnter,
		"",
		func() tea.Cmd {
			b.working = true
			b.workLabel = "creating " + mode + " backup…"
			if mode == compass.ModeStopped {
				return runOp("backup (stopped)", func(ctx context.Context) (string, error) {
					bk, err := b.stack.Engine.StoppedBackup(ctx)
					if err != nil {
						return "", err
					}
					return bk.ID, nil
				})
			}
			return runOp("backup (live)", func(ctx context.Context) (string, error) {
				bk, err := b.stack.Engine.LiveBackup(ctx)
				if err != nil {
					return "", err
				}
				return bk.ID, nil
			})
		},
	)
}

func (b *Backups) askRestore() {
	if !b.engineOK() {
		b.host.Toast("backups unavailable (config did not resolve)", ToastError)
		return
	}
	if len(b.backups) == 0 {
		b.host.Toast("no backups to restore", ToastError)
		return
	}
	id := b.backups[b.cursor].ID
	b.host.Ask(
		"Restore "+id,
		"Stop the server, safety-net the current world, then replace it with "+id+". Undo = restore the safety-net.",
		TierTarget,
		id,
		func() tea.Cmd {
			b.working = true
			b.workLabel = "restoring " + id + "…"
			return runOp("restore "+id, func(ctx context.Context) (string, error) {
				res, err := b.stack.Engine.Restore(ctx, id)
				if err != nil {
					return "", err
				}
				return "safety-net " + res.SafetyNet + " (undo: restore it)", nil
			})
		},
	)
}

func (b *Backups) askPrune() {
	if !b.engineOK() {
		b.host.Toast("backups unavailable (config did not resolve)", ToastError)
		return
	}
	removable, trash, total := b.prunePreview()
	if len(removable) == 0 && len(trash) == 0 {
		b.host.Toast("nothing to prune (within retention, no trash)", ToastInfo)
		return
	}
	b.host.Ask(
		"Prune backups",
		fmt.Sprintf("Remove %d backup(s) beyond retention and %d incomplete dir(s); frees %s.",
			len(removable), len(trash), humanBytes(uint64(total))),
		TierYes,
		"",
		func() tea.Cmd {
			b.working = true
			b.workLabel = "pruning…"
			return runOp("prune", func(ctx context.Context) (string, error) {
				n, err := b.stack.Engine.Prune()
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("%d removed", n), nil
			})
		},
	)
}

// prunePreview computes exactly what Engine.Prune will remove: the complete
// backups beyond the retention window, plus all manifest-less trash dirs. It
// mirrors the engine's Prune ordering (newest-first, keep the first `keep`).
func (b *Backups) prunePreview() (removable []string, trash []string, total int64) {
	if b.stack == nil || b.stack.Engine == nil {
		return
	}
	keep := b.stack.Engine.Retention
	if keep <= 0 {
		keep = 7
	}
	list := b.backups // newest first
	for i := keep; i < len(list); i++ {
		removable = append(removable, list[i].ID)
		total += list[i].TotalSize
	}
	if b.recovery != nil {
		trash = b.recovery.TrashDirs
	}
	return
}

func (b *Backups) askRecover() {
	if !b.engineOK() {
		b.host.Toast("backups unavailable (config did not resolve)", ToastError)
		return
	}
	if b.recovery == nil || (!b.recovery.LockPresent && len(b.recovery.TrashDirs) == 0) {
		b.host.Toast("nothing to recover", ToastInfo)
		return
	}
	b.host.Ask(
		"Recover interrupted operation",
		"Resume a live save that died before its resume, remove incomplete backups, clear the stale lock.",
		TierEnter,
		"",
		func() tea.Cmd {
			b.working = true
			b.workLabel = "recovering…"
			return runOp("recover", func(ctx context.Context) (string, error) {
				res, err := b.stack.Engine.FinishRecovery(ctx)
				if err != nil {
					return "", err
				}
				parts := []string{}
				if res.Resumed {
					parts = append(parts, "resumed")
				}
				if res.ResumedErr != "" {
					parts = append(parts, "resume-unconfirmed")
				}
				if len(res.Cleaned) > 0 {
					parts = append(parts, fmt.Sprintf("cleaned %d", len(res.Cleaned)))
				}
				if res.ServerDown {
					parts = append(parts, "server down — start it")
				}
				if res.LockCleared {
					parts = append(parts, "lock cleared")
				}
				return strings.Join(parts, ", "), nil
			})
		},
	)
}

func (b *Backups) enterTimer() {
	b.timerMode = true
	b.cadence = "daily"
	b.weekday = 0
	b.timeBuf = "02:00"
	b.mode = compass.ModeLive
}

func (b *Backups) updateTimerKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.Type {
	case tea.KeyEsc:
		b.timerMode = false
		return b, b.ticks()
	case tea.KeyEnter:
		b.confirmTimer()
	case tea.KeyBackspace:
		if len(b.timeBuf) > 0 {
			b.timeBuf = b.timeBuf[:len(b.timeBuf)-1]
		}
	case tea.KeyRunes:
		if len(key.Runes) > 0 {
			r := key.Runes[0]
			switch {
			case isRune(key, 'c'):
				if b.cadence == "daily" {
					b.cadence = "weekly"
				} else {
					b.cadence = "daily"
				}
			case isRune(key, 'w'):
				b.weekday = (b.weekday + 1) % 7
			case isRune(key, 'm'):
				if b.mode == compass.ModeLive {
					b.mode = compass.ModeStopped
				} else {
					b.mode = compass.ModeLive
				}
			case r >= '0' && r <= '9', r == ':':
				if len(b.timeBuf) < 5 {
					b.timeBuf += string(r)
				}
			}
		}
	}
	return b, b.ticks()
}

func (b *Backups) confirmTimer() {
	if b.stack == nil || b.stack.Timer == nil {
		b.host.Toast("timer unavailable (wiring did not resolve)", ToastError)
		return
	}
	sched := compass.Schedule{
		Cadence: b.cadence,
		Time:    b.timeBuf,
		Weekday: b.weekday,
		Mode:    b.mode,
	}
	if err := sched.Validate(); err != nil {
		b.host.Toast("timer: "+err.Error(), ToastError)
		return
	}
	b.host.Ask(
		"Schedule backup",
		fmt.Sprintf("Create a systemd user timer: %s at %s, mode %s.", b.cadence, b.timeBuf, b.mode),
		TierEnter,
		"",
		func() tea.Cmd {
			b.working = true
			b.workLabel = "scheduling timer…"
			b.timerMode = false
			return runOp("schedule", func(ctx context.Context) (string, error) {
				st, err := b.stack.Timer.Create(ctx, sched)
				if err != nil {
					return "", err
				}
				detail := "timer on"
				if st.NextRun != "" {
					detail += " (next " + st.NextRun + ")"
				}
				return detail, nil
			})
		},
	)
}

func (b *Backups) askTimerDisable() {
	if b.stack == nil || b.stack.Timer == nil {
		b.host.Toast("timer unavailable (wiring did not resolve)", ToastError)
		return
	}
	if b.timerStatus == nil || !b.timerStatus.Enabled {
		b.host.Toast("timer is not enabled", ToastInfo)
		return
	}
	b.host.Ask(
		"Disable scheduled backup",
		"Stop and disable the systemd user timer (unit files stay in place).",
		TierEnter,
		"",
		func() tea.Cmd {
			b.working = true
			b.workLabel = "disabling timer…"
			return runOp("disable timer", func(ctx context.Context) (string, error) {
				if err := b.stack.Timer.Disable(ctx); err != nil {
					return "", err
				}
				return "timer off", nil
			})
		},
	)
}

// View renders the recovery banner (when present), the backup list with the
// selection cursor, the timer panel, the key hints, and the working spinner.
func (b *Backups) View() string {
	lines := []string{titleStyle.Render("Backups"), ""}

	if b.working {
		f := string(backupSpinnerFrames[b.spin%len(backupSpinnerFrames)])
		lines = append(lines, promptStyle.Render("  "+f+" "+b.workLabel)+"…", "")
	}

	// Recovery banner (stale lock / interrupted op / trash).
	if b.recovery != nil {
		if b.recovery.LockPresent && b.recovery.Stale {
			lines = append(lines, toastErr.Render(fmt.Sprintf(
				"  ! interrupted %s (phase %s) — press x to recover",
				b.recovery.Holder.Op, b.recovery.Holder.Phase)))
		} else if b.recovery.LockPresent {
			lines = append(lines, toastInfo.Render(fmt.Sprintf(
				"  backup in progress (pid %d, phase %s)",
				b.recovery.Holder.PID, b.recovery.Holder.Phase)))
		}
		if len(b.recovery.TrashDirs) > 0 {
			lines = append(lines, hintStyle.Render(fmt.Sprintf(
				"  %d incomplete backup dir(s): %s (prune with p)",
				len(b.recovery.TrashDirs), strings.Join(b.recovery.TrashDirs, ", "))))
		}
		if (b.recovery.LockPresent && b.recovery.Stale) || len(b.recovery.TrashDirs) > 0 {
			lines = append(lines, "")
		}
	}

	// Backup list.
	lines = append(lines, tileSection.Render("Backups"))
	if len(b.backups) == 0 {
		lines = append(lines, hintStyle.Render("  (no backups yet)"))
	}
	for i, bk := range b.backups {
		marker := " "
		if i == b.cursor {
			marker = ">"
		}
		lines = append(lines, fmt.Sprintf("  %s %s  %-9s  %-9s  %s",
			marker, bk.ID, bk.Mode, orDash(bk.World), humanBytes(uint64(bk.TotalSize))))
	}
	lines = append(lines, "")

	// Timer panel.
	lines = append(lines, tileSection.Render("Scheduled backup"))
	lines = append(lines, b.timerView()...)
	lines = append(lines, "")

	// Key hints.
	lines = append(lines, tileSection.Render("Keys"))
	lines = append(lines,
		"  [b] backup live   [s] backup stopped   [r] restore",
		"  [p] prune         [x] recover          [t] schedule timer   [D] disable timer",
	)

	if b.timerMode {
		lines = append(lines, "", b.timerFormView())
	}

	return strings.Join(lines, "\n")
}

func (b *Backups) timerView() []string {
	if b.stack == nil || b.stack.Timer == nil {
		return []string{hintStyle.Render("  unavailable (wiring did not resolve)")}
	}
	if b.timerErr != "" {
		return []string{toastErr.Render("  " + b.timerErr)}
	}
	if b.timerStatus == nil {
		return []string{hintStyle.Render("  not configured — press [t]")}
	}
	st := b.timerStatus
	var state string
	switch {
	case st.Enabled && st.Active:
		state = stateRun.Render("scheduled, on")
	case st.Enabled:
		state = stateRun.Render("scheduled")
	case st.Loaded:
		state = stateUnknown.Render("loaded (off)")
	default:
		state = stateUnknown.Render("not configured")
	}
	next := ""
	if st.NextRun != "" {
		next = "  next: " + st.NextRun
	}
	return []string{fmt.Sprintf("  %s%s", state, next)}
}

func (b *Backups) timerFormView() string {
	wd := ""
	if b.cadence == "weekly" {
		wd = "  weekday: " + weekdayName(b.weekday)
	}
	body := strings.Join([]string{
		titleStyle.Render("Schedule a backup"),
		"  cadence: " + b.cadence + wd,
		"  time:    " + b.timeBuf,
		"  mode:    " + b.mode,
		"",
		"  [c] cadence   [w] weekday   [m] mode   edit time   [Enter] confirm   [Esc] back",
	}, "\n")
	return confirmBox.MaxWidth(64).Render(body)
}

func weekdayName(i int) string {
	names := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	if i < 0 || i >= len(names) {
		return "Mon"
	}
	return names[i]
}
