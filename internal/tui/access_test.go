package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// accessFakeConsole is the offline seam for the Access service in TUI tests:
// Console's function fields are injectable, so an Access built over it never
// touches a real FIFO. Each command delivers its reply on exactly one NewSince
// poll (the quiescence detector settles on the retained line).
type accessFakeConsole struct {
	calls   []string
	replies []string // reply for calls[i] ("") = no output
	polls   int
}

func (f *accessFakeConsole) wire() *compass.Console {
	return &compass.Console{
		Write: func(ctx context.Context, line string) error {
			f.calls = append(f.calls, line)
			return nil
		},
		Baseline: func(ctx context.Context) (string, error) { return "tok", nil },
		NewSince: func(ctx context.Context, token string) ([]string, error) {
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

// newAccessRig builds a Stack whose Access service is driven by the fake console
// over a real temp server dir (server.properties + allowlist.json) and a real
// trail. journalLines feed the Players.Recent mine when non-nil.
func newAccessRig(t *testing.T, f *accessFakeConsole, props string, allowJSON []byte, journalLines []string) *compass.Stack {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.properties"), []byte(props), 0o644); err != nil {
		t.Fatalf("write server.properties: %v", err)
	}
	if allowJSON != nil {
		if err := os.WriteFile(filepath.Join(dir, "allowlist.json"), allowJSON, 0o644); err != nil {
			t.Fatalf("write allowlist.json: %v", err)
		}
	}
	trail, err := compass.NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("trail: %v", err)
	}
	cfg := &compass.Config{ServerDir: dir, Port: 19142}

	var players *compass.Players
	if journalLines != nil {
		jl := make([]string, len(journalLines))
		for i, l := range journalLines {
			jl[i] = "Aug 19 01:30:22 bds bedrock_server[4242]: " + l
		}
		j := &compass.Journal{Unit: "u"}
		j.Exec = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte(strings.Join(jl, "\n")), nil
		}
		players = compass.NewPlayers(f.wire(), trail, j)
	}

	return &compass.Stack{Cfg: cfg, Access: compass.NewAccess(cfg, f.wire(), trail), Players: players}
}

// feedAccess drives the screen's async refresh message with canned data so View
// tests are deterministic (bypassing file + journal reads).
func feedAccess(t *testing.T, s *Access, gate compass.GateState, entries []compass.AllowEntry, recent []compass.RecentPlayer) {
	t.Helper()
	s.Update(accessRefreshMsg{gate: gate, entries: entries, recent: recent})
}

func TestAccess_View_GateOn(t *testing.T) {
	host := &fakeServerHost{}
	st := newAccessRig(t, &accessFakeConsole{}, "allow-list=true\n", []byte("null"), nil)
	s := NewAccess(host, st)
	feedAccess(t, s, compass.GateState{Enabled: true}, nil, nil)
	view := s.View()

	for _, want := range []string{
		"Access",
		"Gate",
		"ON",
		"Every player not on the allowlist is rejected",
		"g gate    a add    r remove    q quick-add    R reload    b block IP    x unblock IP    u refresh",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("View is missing %q\nview:\n%s", want, view)
		}
	}
}

func TestAccess_View_GateOff(t *testing.T) {
	host := &fakeServerHost{}
	st := newAccessRig(t, &accessFakeConsole{}, "allow-list=false\n", []byte("null"), nil)
	s := NewAccess(host, st)
	feedAccess(t, s, compass.GateState{Enabled: false}, nil, nil)
	view := s.View()

	for _, want := range []string{"Gate", "OFF", "(gate off — anyone may join)"} {
		if !strings.Contains(view, want) {
			t.Errorf("View is missing %q\nview:\n%s", want, view)
		}
	}
}

func TestAccess_View_AllowlistAndRecent(t *testing.T) {
	host := &fakeServerHost{}
	st := newAccessRig(t, &accessFakeConsole{}, "allow-list=true\n", nil, nil)
	s := NewAccess(host, st)
	feedAccess(t, s, compass.GateState{Enabled: true},
		[]compass.AllowEntry{{Name: "brea"}, {Name: "tony"}},
		[]compass.RecentPlayer{
			{Name: "oldfriend", State: "disconnected", LastSeen: time.Date(2026, 8, 18, 22, 4, 5, 0, time.UTC)},
			{Name: "brea", State: "connected", LastSeen: time.Date(2026, 8, 19, 1, 30, 22, 0, time.UTC)},
		},
	)
	view := s.View()

	for _, want := range []string{
		"Allowlist (2)",
		"brea",
		"tony",
		"Recently connected (2)",
		"oldfriend",
		"(allowed)", // brea is on the allowlist, so its recent row is annotated
	} {
		if !strings.Contains(view, want) {
			t.Errorf("View is missing %q\nview:\n%s", want, view)
		}
	}

	// #17: each recent row carries its last-seen timestamp (local wall clock).
	for _, rp := range []compass.RecentPlayer{
		{Name: "oldfriend", LastSeen: time.Date(2026, 8, 18, 22, 4, 5, 0, time.UTC)},
		{Name: "brea", LastSeen: time.Date(2026, 8, 19, 1, 30, 22, 0, time.UTC)},
	} {
		if !strings.Contains(view, rp.LastSeen.Format("2006-01-02 15:04")) {
			t.Errorf("View is missing last-seen timestamp for %q\nview:\n%s", rp.Name, view)
		}
	}
}

func TestAccess_View_DegradedWhenNoService(t *testing.T) {
	host := &fakeServerHost{}
	s := NewAccess(host, &compass.Stack{}) // no Access service resolved
	view := s.View()
	if !strings.Contains(view, "access unavailable") {
		t.Errorf("View should degrade clearly\nview:\n%s", view)
	}
	if !strings.Contains(view, "g gate    a add    r remove    q quick-add    R reload    b block IP    x unblock IP    u refresh") {
		t.Errorf("View should still show the key hints\nview:\n%s", view)
	}
}

func TestAccess_GateToggleConfirmsAtTierYes(t *testing.T) {
	host := &fakeServerHost{}
	fc := &accessFakeConsole{replies: []string{"Turned on the allowlist"}}
	st := newAccessRig(t, fc, "allow-list=false\n", []byte("null"), nil)
	s := NewAccess(host, st)
	feedAccess(t, s, compass.GateState{Enabled: false}, nil, nil)

	// 'g' toggles the gate (currently off → enabling).
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	if len(host.asks) != 1 || host.asks[0] != "Enable allowlist gate" {
		t.Fatalf("asks = %v, want [Enable allowlist gate]", host.asks)
	}
	if host.lastRun == nil {
		t.Fatal("no confirm command recorded")
	}
	msg := host.lastRun()()
	r, ok := msg.(opResultMsg)
	if !ok {
		t.Fatalf("msg = %#v, want opResultMsg", msg)
	}
	if r.err != "" {
		t.Errorf("gate err = %q, want ok", r.err)
	}
	// The durable write must have flipped server.properties.
	data, _ := os.ReadFile(filepath.Join(st.Cfg.ServerDir, "server.properties"))
	if !strings.Contains(string(data), "allow-list=true") {
		t.Errorf("server.properties = %q, want allow-list=true", string(data))
	}
	// The live apply must have sent `allowlist on`.
	found := false
	for _, c := range fc.calls {
		if c == "allowlist on" {
			found = true
		}
	}
	if !found {
		t.Errorf("console calls = %v, want [allowlist on]", fc.calls)
	}
}

func TestAccess_AddByGamertag(t *testing.T) {
	host := &fakeServerHost{}
	fc := &accessFakeConsole{replies: []string{"", "brea"}} // reload (no out), list -> brea
	st := newAccessRig(t, fc, "allow-list=true\n", []byte("null"), nil)
	s := NewAccess(host, st)

	// Enter add mode with 'a', type a gamertag, confirm with Enter.
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	for _, r := range []rune("brea") {
		s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if len(host.asks) != 1 || host.asks[0] != "Add to allowlist" {
		t.Fatalf("asks = %v, want [Add to allowlist]", host.asks)
	}
	msg := host.lastRun()()
	r, ok := msg.(opResultMsg)
	if !ok {
		t.Fatalf("msg = %#v, want opResultMsg", msg)
	}
	if r.err != "" {
		t.Errorf("add err = %q, want ok", r.err)
	}
	data, _ := os.ReadFile(filepath.Join(st.Cfg.ServerDir, "allowlist.json"))
	if !strings.Contains(string(data), `"name":"brea"`) {
		t.Errorf("allowlist.json = %q, want brea entry", string(data))
	}
}

func TestAccess_QuickAddSingleAction(t *testing.T) {
	host := &fakeServerHost{}
	fc := &accessFakeConsole{replies: []string{"", "oldfriend"}} // reload (no out), list -> oldfriend
	st := newAccessRig(t, fc, "allow-list=true\n", []byte("null"), nil)
	s := NewAccess(host, st)
	// Cursor is on the first recent row (empty allowlist → cursor 0 = recent[0]).
	feedAccess(t, s, compass.GateState{Enabled: true}, nil,
		[]compass.RecentPlayer{{Name: "oldfriend", State: "disconnected"}})

	// 'q' is the single quick-add action.
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if len(host.asks) != 1 || host.asks[0] != "Quick-add to allowlist" {
		t.Fatalf("asks = %v, want [Quick-add to allowlist]", host.asks)
	}
	msg := host.lastRun()()
	r, ok := msg.(opResultMsg)
	if !ok {
		t.Fatalf("msg = %#v, want opResultMsg", msg)
	}
	if r.err != "" {
		t.Errorf("quick-add err = %q, want ok", r.err)
	}
	data, _ := os.ReadFile(filepath.Join(st.Cfg.ServerDir, "allowlist.json"))
	if !strings.Contains(string(data), `"name":"oldfriend"`) {
		t.Errorf("allowlist.json = %q, want oldfriend entry", string(data))
	}
}

func TestAccess_RemoveSelectedRow(t *testing.T) {
	host := &fakeServerHost{}
	fc := &accessFakeConsole{replies: []string{"", ""}} // reload (no out), list -> empty
	st := newAccessRig(t, fc, "allow-list=true\n", []byte(`[{"name":"brea","ignoresPlayerLimit":false}]`), nil)
	s := NewAccess(host, st)
	feedAccess(t, s, compass.GateState{Enabled: true},
		[]compass.AllowEntry{{Name: "brea"}}, nil)

	// Cursor 0 = allowlist row brea. 'r' removes it (TierTarget on "brea").
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if len(host.asks) != 1 || host.asks[0] != "Remove from allowlist" {
		t.Fatalf("asks = %v, want [Remove from allowlist]", host.asks)
	}
	msg := host.lastRun()()
	r, ok := msg.(opResultMsg)
	if !ok {
		t.Fatalf("msg = %#v, want opResultMsg", msg)
	}
	if r.err != "" {
		t.Errorf("remove err = %q, want ok", r.err)
	}
	data, _ := os.ReadFile(filepath.Join(st.Cfg.ServerDir, "allowlist.json"))
	if strings.Contains(string(data), "brea") {
		t.Errorf("allowlist.json = %q, want brea removed", string(data))
	}
}

func TestAccess_BlockIP_DegradedShowsCommand(t *testing.T) {
	host := &fakeServerHost{}
	fc := &accessFakeConsole{}
	st := newAccessRig(t, fc, "allow-list=true\n", []byte("null"), nil)
	s := NewAccess(host, st)

	// Enter block mode with 'b', type an IP, confirm. The service is unprivileged
	// (euid != 0 in the test process), so it degrades to showing the command.
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	for _, r := range []rune("1.2.3.4") {
		s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if len(host.asks) != 1 || host.asks[0] != "Block IP" {
		t.Fatalf("asks = %v, want [Block IP]", host.asks)
	}
	msg := host.lastRun()()
	r, ok := msg.(opResultMsg)
	if !ok {
		t.Fatalf("msg = %#v, want opResultMsg", msg)
	}
	if r.err != "" {
		t.Errorf("block err = %q, want ok (degraded is not an error)", r.err)
	}
	if !strings.Contains(r.detail, "ufw deny from 1.2.3.4") {
		t.Errorf("detail = %q, want the exact ufw command", r.detail)
	}
	if !strings.Contains(r.detail, "not applied") {
		t.Errorf("detail = %q, want a 'not applied' note", r.detail)
	}
}

func TestAccess_BlockIP_InvalidToasts(t *testing.T) {
	host := &fakeServerHost{}
	fc := &accessFakeConsole{}
	st := newAccessRig(t, fc, "allow-list=true\n", []byte("null"), nil)
	s := NewAccess(host, st)

	// An invalid IP is rejected by the service (not a valid address).
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	for _, r := range []rune("999.1.1.1") {
		s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if len(host.asks) != 1 || host.asks[0] != "Block IP" {
		t.Fatalf("asks = %v, want [Block IP]", host.asks)
	}
	msg := host.lastRun()()
	r, ok := msg.(opResultMsg)
	if !ok {
		t.Fatalf("msg = %#v, want opResultMsg", msg)
	}
	if r.err == "" {
		t.Errorf("block err = empty, want a validation error for 999.1.1.1")
	}
}

func TestAccess_InputEscCancels(t *testing.T) {
	host := &fakeServerHost{}
	st := newAccessRig(t, &accessFakeConsole{}, "allow-list=true\n", []byte("null"), nil)
	s := NewAccess(host, st)

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if s.mode != inputAdd {
		t.Fatalf("mode = %v, want inputAdd", s.mode)
	}
	for _, r := range []rune("brea") {
		s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if s.buf != "brea" {
		t.Errorf("buf = %q, want brea", s.buf)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if s.mode != inputNone || s.buf != "" {
		t.Errorf("after Esc mode=%v buf=%q, want none/empty", s.mode, s.buf)
	}
	if len(host.asks) != 0 {
		t.Errorf("no confirmation expected after cancel, got %v", host.asks)
	}
}

func TestAccess_TabLabel(t *testing.T) {
	host := &fakeServerHost{}
	s := NewAccess(host, &compass.Stack{})
	if s.Tab() != "Access" {
		t.Errorf("Tab() = %q, want Access", s.Tab())
	}
}

// TestApp_AccessInputCapturesDigits locks a shell-level bug found during #7
// verification: the shell's global tab-jump (digits 1-9) and quit (q) fired
// BEFORE keys reached any screen, so an IP/XUID field could never receive digit
// characters — typing "203.0.113.66" jumped tabs instead of filling the field.
// The KeyCaptor fix routes every key to a capturing screen first. This test
// drives the real App shell with a real Access screen and asserts digits land in
// the buffer while the tab stays put.
func TestApp_AccessInputCapturesDigits(t *testing.T) {
	host := &fakeServerHost{}
	st := newAccessRig(t, &accessFakeConsole{}, "allow-list=true\n", []byte("null"), nil)

	app := NewApp()
	app.SetStatusProvider(statProvider(compass.Status{}))
	acc := NewAccess(host, st)
	app.Screens[2] = acc

	// Jump to Access (tab index 2). Digits are global shortcuts when no screen
	// is capturing — this must switch tabs.
	app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	if app.active != 2 {
		t.Fatalf("active = %d, want 2 (Access)", app.active)
	}

	// Enter block-IP input mode.
	app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	if !acc.CapturingKeys() {
		t.Fatal("Access should be capturing keys in input mode")
	}

	// Type a full dotted IP. Before the fix, '2' jumped to tab 2 (no-op here),
	// '.' fell through harmlessly but digits like '0'/'1' would have switched
	// tabs; now every character must land in the buffer and the tab stays put.
	for _, r := range []rune("203.0.113.66") {
		app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	if acc.buf != "203.0.113.66" {
		t.Errorf("buf = %q, want 203.0.113.66", acc.buf)
	}
	if app.active != 2 {
		t.Errorf("active = %d after typing digits, want 2 (tab must not jump)", app.active)
	}

	// Esc exits capture; global shortcuts work again ('q' quits).
	app.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if acc.CapturingKeys() {
		t.Error("Access should stop capturing after Esc")
	}
}
