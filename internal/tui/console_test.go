package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// consoleFakeConsole is the offline seam for the Console screen's TUI tests:
// Console's function fields are injectable, so a RawConsole built over it never
// touches a real FIFO. Each command delivers its reply on exactly one NewSince
// poll (the quiescence detector settles on the retained line), mirroring
// accessFakeConsole. With grow set, every poll returns one more line than the
// last — output that never goes quiet, so the harvest runs to its hard timeout.
type consoleFakeConsole struct {
	calls   []string
	replies []string // reply for calls[i] ("") = no output
	polls   int
	grow    bool
	grows   int
}

func (f *consoleFakeConsole) wire() *compass.Console {
	return &compass.Console{
		Write: func(ctx context.Context, line string) error {
			f.calls = append(f.calls, line)
			return nil
		},
		Baseline: func(ctx context.Context) (string, error) { return "tok", nil },
		NewSince: func(ctx context.Context, token string) ([]string, error) {
			if f.grow {
				f.grows++
				lines := make([]string, f.grows)
				for i := range lines {
					lines[i] = fmt.Sprintf("line %d", i+1)
				}
				return lines, nil
			}
			if len(f.calls) == 0 || f.polls >= len(f.calls) {
				return nil, nil
			}
			i := len(f.calls) - 1
			f.polls++
			if i < len(f.replies) && f.replies[i] != "" {
				return []string{f.replies[i]}, nil
			}
			return nil, nil
		},
		Policy: compass.ConsolePolicy{Quiesce: time.Millisecond, Timeout: 2 * time.Second, Poll: time.Millisecond},
	}
}

// fakeConsoleHost records the shell calls the Console screen makes (Toast and
// ExitConsole) so tests can assert Esc routing without a live shell.
type fakeConsoleHost struct {
	toasts    []string
	exitCalls int
	lastRun   func() tea.Cmd
}

func (h *fakeConsoleHost) Toast(msg string, kind ToastKind) { h.toasts = append(h.toasts, msg) }
func (h *fakeConsoleHost) ExitConsole() tea.Cmd             { h.exitCalls++; return nil }

// newConsoleRig builds a Stack whose RawConsole service is driven by the fake
// console over a real temp audit trail — the same shape wire.go produces.
func newConsoleRig(t *testing.T, f *consoleFakeConsole) (*compass.Stack, *compass.Trail) {
	t.Helper()
	trail, err := compass.NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("trail: %v", err)
	}
	cfg := &compass.Config{ServerDir: t.TempDir(), Port: 19142, Service: "bedrock-compass-test.service"}
	return &compass.Stack{Cfg: cfg, RawConsole: compass.NewRawConsole(f.wire(), trail)}, trail
}

// runCmds executes a (possibly batched) tea.Cmd and returns every message it
// produced, so tests can drive async sends synchronously. In bubbletea v1.x a
// Batch's result is a tea.BatchMsg ([]Cmd), whose members are executed in turn;
// any other value is a plain Msg.
func runCmds(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	m := cmd()
	switch v := m.(type) {
	case tea.BatchMsg:
		var out []tea.Msg
		for _, c := range v {
			out = append(out, runCmds(c)...)
		}
		return out
	default:
		return []tea.Msg{m}
	}
}

// typeText feeds one key per rune into a model, using KeySpace for spaces —
// bubbletea v1.x delivers a lone space as tea.KeySpace, not KeyRunes.
func typeText(m tea.Model, text string) {
	for _, r := range []rune(text) {
		if r == ' ' {
			m.Update(tea.KeyMsg{Type: tea.KeySpace})
			continue
		}
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// deliverTabSwitch executes cmd, extracts any deferred tab switch it produced
// (GoTo / ExitConsole), and feeds that message back into the app — where the
// switch happens synchronously. Periodic tick commands are deliberately NOT
// executed: they reschedule themselves forever and would block on their timers
// in a test. The ticks returned by the shell's tabSwitchMsg handler are dropped
// for the same reason; tests assert on state, not on live polling.
func deliverTabSwitch(app *App, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	for _, m := range runCmds(cmd) {
		if ts, ok := m.(tabSwitchMsg); ok {
			app.Update(ts)
		}
	}
}

// sendCommand types a command into the screen and drives its async round-trip
// to completion, returning the consoleSendMsg it produced (nil on failure).
func sendCommand(t *testing.T, c *Console, text string) *consoleSendMsg {
	t.Helper()
	typeText(c, text)
	_, cmd := c.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !c.working {
		t.Fatalf("screen should be working after Enter")
	}
	for _, m := range runCmds(cmd) {
		if sm, ok := m.(consoleSendMsg); ok {
			c.Update(sm) // land the result in the transcript
			return &sm
		}
	}
	t.Fatal("no consoleSendMsg produced by the send command")
	return nil
}

func TestConsole_TabLabel(t *testing.T) {
	c := NewConsole(&fakeConsoleHost{}, &compass.Stack{})
	if c.Tab() != "Console" {
		t.Errorf("Tab() = %q, want Console", c.Tab())
	}
}

func TestConsole_View_DegradedWhenNoService(t *testing.T) {
	host := &fakeConsoleHost{}
	c := NewConsole(host, &compass.Stack{}) // no RawConsole resolved
	view := c.View()
	if !strings.Contains(view, "console unavailable") {
		t.Errorf("View should degrade clearly\nview:\n%s", view)
	}
	if c.CapturingKeys() {
		t.Error("a degraded Console must not capture keys (Esc/typing would be dead)")
	}
}

func TestConsole_CapturingKeysWhenServiceOK(t *testing.T) {
	st, _ := newConsoleRig(t, &consoleFakeConsole{})
	c := NewConsole(&fakeConsoleHost{}, st)
	if !c.CapturingKeys() {
		t.Error("a serviceable Console must capture keys (raw mode)")
	}
}

func TestConsole_SendVerbatimAndAudits(t *testing.T) {
	fc := &consoleFakeConsole{replies: []string{"Done. 0 matching entities"}}
	st, trail := newConsoleRig(t, fc)
	c := NewConsole(&fakeConsoleHost{}, st)

	sm := sendCommand(t, c, "list all")
	if sm.err != "" {
		t.Fatalf("send err = %q, want ok", sm.err)
	}
	if len(fc.calls) != 1 || fc.calls[0] != "list all" {
		t.Errorf("console calls = %v, want [list all]", fc.calls)
	}

	view := c.View()
	for _, want := range []string{"❯ list all", "Done. 0 matching entities"} {
		if !strings.Contains(view, want) {
			t.Errorf("View is missing %q\nview:\n%s", want, view)
		}
	}

	evs, err := trail.Recent(5)
	if err != nil {
		t.Fatalf("trail.Recent: %v", err)
	}
	found := false
	for _, ev := range evs {
		if ev.Verb == "console" && ev.Params["command"] == "list all" && ev.Result == "ok" {
			found = true
		}
	}
	if !found {
		t.Errorf("audit trail missing a console event for %q: %+v", "list all", evs)
	}
}

func TestConsole_SendFailureLandsInTranscriptAndTrail(t *testing.T) {
	fc := &consoleFakeConsole{grow: true} // output never settles → hard timeout
	st, trail := newConsoleRig(t, fc)
	c := NewConsole(&fakeConsoleHost{}, st)

	// A short policy timeout keeps the test fast; the fake keeps growing.
	st.RawConsole.Console.Policy.Timeout = 30 * time.Millisecond

	for _, r := range []rune("ticking") {
		c.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	_, cmd := c.Update(tea.KeyMsg{Type: tea.KeyEnter})
	var sm *consoleSendMsg
	for _, m := range runCmds(cmd) {
		if s, ok := m.(consoleSendMsg); ok {
			sm = &s
		}
	}
	if sm == nil || sm.err == "" {
		t.Fatalf("expected a timed-out send with an error, got %+v", sm)
	}
	c.Update(*sm)

	view := c.View()
	if !strings.Contains(view, "error:") {
		t.Errorf("View should show the failure note\nview:\n%s", view)
	}

	evs, err := trail.Recent(5)
	if err != nil {
		t.Fatalf("trail.Recent: %v", err)
	}
	found := false
	for _, ev := range evs {
		if ev.Verb == "console" && ev.Params["command"] == "ticking" && strings.HasPrefix(ev.Result, "error:") {
			found = true
		}
	}
	if !found {
		t.Errorf("audit trail missing a failed console event for %q: %+v", "ticking", evs)
	}
}

// TestConsole_SendDeadline_FollowsPolicy verifies the per-send context deadline
// tracks the resolved console policy (policy timeout + margin) instead of a
// hardcoded constant, and falls back to 20s when no channel resolved.
func TestConsole_SendDeadline_FollowsPolicy(t *testing.T) {
	fc := &consoleFakeConsole{}
	st, _ := newConsoleRig(t, fc)

	// The rig's fake console carries a 2s policy -> deadline is 2s + 5s margin.
	c := NewConsole(&fakeConsoleHost{}, st)
	if got := c.sendDeadline(); got != 7*time.Second {
		t.Fatalf("sendDeadline() = %v with 2s policy, want 7s", got)
	}

	// A raised policy timeout must extend the deadline past it.
	st.RawConsole.Console.Policy.Timeout = 30 * time.Second
	if got := c.sendDeadline(); got != 35*time.Second {
		t.Fatalf("sendDeadline() = %v with 30s policy, want 35s", got)
	}

	// A nil RawConsole (unresolved surface) falls back to the constant.
	c2 := NewConsole(&fakeConsoleHost{}, &compass.Stack{})
	if got := c2.sendDeadline(); got != 20*time.Second {
		t.Fatalf("sendDeadline() = %v with nil stack service, want 20s", got)
	}
}

// TestConsole_TranscriptScrollback_PgUpPgDown verifies PgUp/PgDown move the
// viewport through the transcript (the offset mechanism) and that the "older
// lines" hint appears only while scrolled.
func TestConsole_TranscriptScrollback_PgUpPgDown(t *testing.T) {
	fc := &consoleFakeConsole{}
	st, _ := newConsoleRig(t, fc)
	c := NewConsole(&fakeConsoleHost{}, st)

	// Size the screen through Update (the returned model carries state).
	var m tea.Model = c
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	c = m.(*Console)

	// Fill the transcript well past one viewport (enough that a full page of
	// scrolling does not hit the clamp).
	for i := 0; i < 40; i++ {
		c.appendLine(consoleLine{kind: lineOut, text: fmt.Sprintf("line %d", i)})
	}

	vh := c.viewHeight()
	if vh <= 0 {
		t.Fatalf("viewHeight = %d, want > 0", vh)
	}

	view := func() string { return c.View() }
	if strings.Contains(view(), "older line(s)") {
		t.Fatal("hint shown while stuck to newest (offset must be 0)")
	}

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp}) // one page toward older lines
	c = m.(*Console)
	if c.offset != vh {
		t.Fatalf("offset = %d after PgUp, want %d", c.offset, vh)
	}
	if !strings.Contains(view(), "older line(s)") {
		t.Fatal("hint not shown while scrolled")
	}

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown}) // back to newest
	c = m.(*Console)
	if c.offset != 0 {
		t.Fatalf("offset = %d after PgDown, want 0", c.offset)
	}

	// Clamping: scrolling far past the top must not run away.
	for i := 0; i < 10; i++ {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
		c = m.(*Console)
	}
	if max := len(c.lines) - vh; c.offset > max {
		t.Fatalf("offset = %d exceeds clamp %d", c.offset, max)
	}
}

func TestConsole_HistoryNavigation(t *testing.T) {
	fc := &consoleFakeConsole{replies: []string{"a", "b"}}
	st, _ := newConsoleRig(t, fc)
	c := NewConsole(&fakeConsoleHost{}, st)

	sendCommand(t, c, "first")
	sendCommand(t, c, "second")

	// ↑ recalls the most recent command.
	c.Update(tea.KeyMsg{Type: tea.KeyUp})
	if c.buf != "second" {
		t.Errorf("buf = %q after first ↑, want second", c.buf)
	}
	// ↑ again walks back to the older one.
	c.Update(tea.KeyMsg{Type: tea.KeyUp})
	if c.buf != "first" {
		t.Errorf("buf = %q after second ↑, want first", c.buf)
	}
	// ↓ returns toward newer; at the newest it clears navigation + buffer.
	c.Update(tea.KeyMsg{Type: tea.KeyDown})
	if c.buf != "second" {
		t.Errorf("buf = %q after ↓, want second", c.buf)
	}
	c.Update(tea.KeyMsg{Type: tea.KeyDown})
	if c.buf != "" || c.histIdx != -1 {
		t.Errorf("after final ↓ buf=%q histIdx=%d, want empty/-1", c.buf, c.histIdx)
	}

	// Typing a new command leaves history navigation.
	for _, r := range []rune("x") {
		c.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if c.histIdx != -1 {
		t.Errorf("histIdx = %d after typing, want -1", c.histIdx)
	}
}

func TestConsole_EscExitsToHost(t *testing.T) {
	st, _ := newConsoleRig(t, &consoleFakeConsole{})
	host := &fakeConsoleHost{}
	c := NewConsole(host, st)

	c.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if host.exitCalls != 1 {
		t.Errorf("ExitConsole calls = %d, want 1", host.exitCalls)
	}
}

func TestConsole_EmptyEnterIsNoOp(t *testing.T) {
	fc := &consoleFakeConsole{}
	st, _ := newConsoleRig(t, fc)
	c := NewConsole(&fakeConsoleHost{}, st)

	if _, cmd := c.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil || c.working {
		t.Error("an empty Enter must not start a send")
	}
	if len(fc.calls) != 0 {
		t.Errorf("console calls = %v, want none", fc.calls)
	}
}

// TestApp_ConsoleCapturesKeysAndEscReturns locks the shell-level contract for
// ticket #12: while the Console tab is active it owns every key (digits are
// typed, not tab jumps; q is typed, not quit), and Esc returns to the tab that
// was active before — mirroring TestApp_AccessInputCapturesDigits from #7.
func TestApp_ConsoleCapturesKeysAndEscReturns(t *testing.T) {
	fc := &consoleFakeConsole{replies: []string{"ok"}}
	st, _ := newConsoleRig(t, fc)

	app := NewApp()
	app.SetStatusProvider(statProvider(compass.Status{}))
	app.Screens[6] = NewConsole(app, st)

	// Jump to the Console tab (index 6). prevActive must record where we came from.
	app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'7'}})
	if app.active != 6 {
		t.Fatalf("active = %d, want 6 (Console)", app.active)
	}

	// Type a command with digits, letters, and spaces; none of it may jump
	// tabs or quit. Spaces arrive as tea.KeySpace (bubbletea v1.x).
	typeText(app, "say hi 123")
	if app.active != 6 {
		t.Fatalf("active = %d after typing, want 6 (tab must not jump)", app.active)
	}
	console := app.Screens[6].(*Console)
	if console.buf != "say hi 123" {
		t.Errorf("buf = %q, want 'say hi 123'", console.buf)
	}

	// Esc returns to the previous tab (0 — we came from Dashboard). The switch
	// is deferred as a shell message; pump it through the app like the program
	// loop would.
	if _, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil {
		deliverTabSwitch(app, cmd)
	}
	if app.active != 0 {
		t.Errorf("active = %d after Esc, want 0 (previous tab)", app.active)
	}
}

// TestApp_ConsoleEscDoesNotClobberOtherTabs is the regression test for the bug
// ticket #12's live capture exposed: ExitConsole used to switch tabs eagerly
// inside the Console's Update, so the shell's write-back stored the *Console*
// model into the slot of the tab being left (Config). After Esc from Console,
// every other tab must still be its own screen.
func TestApp_ConsoleEscDoesNotClobberOtherTabs(t *testing.T) {
	fc := &consoleFakeConsole{replies: []string{"ok"}}
	st, _ := newConsoleRig(t, fc)

	app := NewApp()
	app.SetStatusProvider(statProvider(compass.Status{}))
	app.Screens[6] = NewConsole(app, st)

	// Jump to Console (index 6), then Esc back.
	app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'7'}})
	if _, cmd := app.Update(tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil {
		deliverTabSwitch(app, cmd)
	}

	for i, s := range app.Screens {
		if c, ok := s.(*Console); ok && i != 6 {
			t.Errorf("Screens[%d] is a *Console (%p), want the original placeholder — Esc clobbered it", i, c)
		}
	}
	if app.Screens[5].Tab() != "Config" {
		t.Errorf("Screens[5].Tab() = %q, want Config (the tab left of Console)", app.Screens[5].Tab())
	}

	// The same contract holds for GoTo: a dashboard quick action routes to
	// Backups without clobbering the Dashboard slot.
	app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}}) // back to Dashboard (0)
	if cmd := app.GoTo("Backups"); cmd != nil {
		deliverTabSwitch(app, cmd)
	}
	if app.active != 3 {
		t.Errorf("active = %d after GoTo(Backups), want 3", app.active)
	}
	for i, s := range app.Screens {
		if _, ok := s.(*Console); ok && i != 6 {
			t.Errorf("Screens[%d] is a *Console — GoTo clobbered it", i)
		}
	}
}
