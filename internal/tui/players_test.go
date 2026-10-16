package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// playersFakeConsole is the same offline seam as the compass package's fake:
// Console's function fields are injectable, so a Players service built over it
// never touches a real FIFO or journal. Each command delivers its reply on
// exactly one NewSince poll (the quiescence detector then settles on the
// retained line), and baseline reads before any write return nothing.
type playersFakeConsole struct {
	calls   []string
	replies []string // reply for calls[i] ("" = no output)
	polls   int      // NewSince invocations; each delivers at most one reply
}

func (f *playersFakeConsole) wire() *compass.Console {
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

// newPlayersRig builds a Stack whose Players service is driven by the fake
// console and a real trail + journal tail.
func newPlayersRig(t *testing.T, f *playersFakeConsole, journalLines []string) *compass.Stack {
	t.Helper()
	trail, err := compass.NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("trail: %v", err)
	}
	jl := make([]string, len(journalLines))
	for i, l := range journalLines {
		jl[i] = "Aug 19 01:30:22 rig bedrock_server[4242]: " + l
	}
	j := &compass.Journal{Unit: "u"}
	j.Exec = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte(strings.Join(jl, "\n")), nil
	}
	return &compass.Stack{Players: compass.NewPlayers(f.wire(), trail, j)}
}

// feedRoster drives the screen's async roster + recent messages with canned
// data (bypassing the console round-trip) so View tests are deterministic.
func feedRoster(t *testing.T, s *Players, online []compass.Player, count, max int, recent []compass.RecentPlayer) {
	t.Helper()
	s.Update(playersRosterMsg{roster: compass.Roster{Count: count, Max: max, Online: online}})
	s.Update(playersRecentMsg{recent: recent})
}

func TestPlayers_View_Online(t *testing.T) {
	host := &fakeServerHost{}
	st := newPlayersRig(t, &playersFakeConsole{}, nil)
	s := NewPlayers(host, st)
	feedRoster(t, s,
		[]compass.Player{
			{Name: "brea", XUID: "1111111111111111"},
			{Name: "player_one", XUID: "2222222222222222"},
		}, 2, 15,
		[]compass.RecentPlayer{
			{Name: "brea", XUID: "1111111111111111", State: "connected", LastSeen: time.Date(2026, 8, 19, 1, 30, 22, 0, time.UTC)},
			{Name: "oldfriend", XUID: "", State: "disconnected", LastSeen: time.Date(2026, 8, 18, 22, 4, 5, 0, time.UTC)},
		},
	)
	view := s.View()

	for _, want := range []string{
		"Players",
		"Online (2 / 15 players online)",
		"brea",
		"xuid 1111111111111111",
		"player_one",
		"Recently connected (2)",
		"oldfriend",
		"left",
		"k kick    o op    d deop    a allow    u refresh",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("View is missing %q\nview:\n%s", want, view)
		}
	}
}

func TestPlayers_View_EmptyRoster(t *testing.T) {
	host := &fakeServerHost{}
	st := newPlayersRig(t, &playersFakeConsole{}, nil)
	s := NewPlayers(host, st)
	feedRoster(t, s, nil, 0, 15, nil)
	view := s.View()

	for _, want := range []string{
		"Online (0 / 15 players online)",
		"(nobody in the world)",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("View is missing %q\nview:\n%s", want, view)
		}
	}
}

func TestPlayers_View_DegradedWhenNoService(t *testing.T) {
	host := &fakeServerHost{}
	s := NewPlayers(host, &compass.Stack{}) // no Players service resolved
	view := s.View()
	if !strings.Contains(view, "players unavailable") {
		t.Errorf("View should degrade clearly\nview:\n%s", view)
	}
	if !strings.Contains(view, "k kick    o op    d deop    a allow    u refresh") {
		t.Errorf("View should still show the key hints\nview:\n%s", view)
	}
}

func TestPlayers_View_RosterError(t *testing.T) {
	host := &fakeServerHost{}
	st := newPlayersRig(t, &playersFakeConsole{}, nil)
	s := NewPlayers(host, st)
	s.Update(playersRosterMsg{err: "console send \"list\": timeout"})
	view := s.View()
	if !strings.Contains(view, "timeout") {
		t.Errorf("View should surface the roster error\nview:\n%s", view)
	}
}

func TestPlayers_KickConfirmsAtTierEnter(t *testing.T) {
	host := &fakeServerHost{}
	fc := &playersFakeConsole{replies: []string{"Kicked brea"}}
	st := newPlayersRig(t, fc, nil)
	s := NewPlayers(host, st)
	feedRoster(t, s, []compass.Player{{Name: "brea", XUID: "1111111111111111"}}, 1, 15, nil)

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if len(host.asks) != 1 || host.asks[0] != "Kick player" {
		t.Fatalf("asks = %v, want [Kick player]", host.asks)
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
		t.Errorf("kick err = %q, want ok", r.err)
	}
	if len(fc.calls) != 1 || fc.calls[0] != "kick brea" {
		t.Errorf("console calls = %v, want [kick brea]", fc.calls)
	}
}

func TestPlayers_OpDeopAllowConfirm(t *testing.T) {
	host := &fakeServerHost{}
	fc := &playersFakeConsole{replies: []string{"", "", ""}}
	st := newPlayersRig(t, fc, nil)
	s := NewPlayers(host, st)
	feedRoster(t, s, []compass.Player{{Name: "brea"}}, 1, 15, nil)

	cases := []struct {
		key       byte
		wantTitle string
	}{
		{'o', "Make operator"},
		{'d', "Remove operator"},
		{'a', "Allow player"},
	}
	for _, c := range cases {
		host.asks = nil
		s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{rune(c.key)}})
		if len(host.asks) != 1 || host.asks[0] != c.wantTitle {
			t.Errorf("key %c: asks = %v, want [%s]", c.key, host.asks, c.wantTitle)
		}
	}
}

func TestPlayers_ActionWithoutSelectionToasts(t *testing.T) {
	host := &fakeServerHost{}
	st := newPlayersRig(t, &playersFakeConsole{}, nil)
	s := NewPlayers(host, st) // empty roster

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if len(host.asks) != 0 {
		t.Fatalf("no confirmation expected with no selection, got %v", host.asks)
	}
	found := false
	for _, toast := range host.toasts {
		if strings.Contains(toast, "no player selected") {
			found = true
		}
	}
	if !found {
		t.Errorf("toasts = %v, want a 'no player selected' note", host.toasts)
	}
}

func TestPlayers_StatusMsgTriggersRepoll(t *testing.T) {
	host := &fakeServerHost{}
	fc := &playersFakeConsole{replies: []string{"There are 1/15 players online:\nbrea [xuid: 1111111111111111]"}}
	st := newPlayersRig(t, fc, nil)
	s := NewPlayers(host, st)

	// The shell's status poll piggybacks a roster re-fetch.
	_, cmd := s.Update(statusMsg(compass.Status{}))
	if cmd == nil {
		t.Fatal("statusMsg should schedule a re-poll")
	}
	msgs := drainCmd(t, cmd)
	var sawRoster bool
	for _, m := range msgs {
		if rm, ok := m.(playersRosterMsg); ok && rm.err == "" {
			sawRoster = true
			if rm.roster.Count != 1 || len(rm.roster.Online) != 1 || rm.roster.Online[0].Name != "brea" {
				t.Errorf("roster = %+v, want brea online", rm.roster)
			}
		}
	}
	if !sawRoster {
		t.Fatalf("re-poll produced %v, want a roster message", msgs)
	}

	// The roster result chains the journal mine.
	_, cmd2 := s.Update(playersRosterMsg{roster: compass.Roster{Count: 1, Max: 15}})
	msgs = drainCmd(t, cmd2)
	var sawRecent bool
	for _, m := range msgs {
		if rm, ok := m.(playersRecentMsg); ok && rm.err == "" {
			sawRecent = true
		}
	}
	if !sawRecent {
		t.Errorf("roster result should chain the recent fetch; got %v", msgs)
	}
}

func TestPlayers_RepollAfterAction(t *testing.T) {
	host := &fakeServerHost{}
	fc := &playersFakeConsole{replies: []string{"Kicked brea"}}
	st := newPlayersRig(t, fc, nil)
	s := NewPlayers(host, st)

	// A completed action re-polls both lists.
	_, cmd := s.Update(opResultMsg{label: "kick", detail: ""})
	msgs := drainCmd(t, cmd)
	var sawRoster, sawRecent bool
	for _, m := range msgs {
		switch m.(type) {
		case playersRosterMsg:
			sawRoster = true
		case playersRecentMsg:
			sawRecent = true
		}
	}
	if !sawRoster || !sawRecent {
		t.Errorf("post-action re-poll produced %v, want roster + recent", msgs)
	}
}

func TestPlayers_TabLabel(t *testing.T) {
	host := &fakeServerHost{}
	s := NewPlayers(host, &compass.Stack{})
	if s.Tab() != "Players" {
		t.Errorf("Tab() = %q, want Players", s.Tab())
	}
}

// TestApp_SwitchTabSchedulesInitialPoll locks a shell-level bug found during
// #6 verification: broadcastStatus dropped the command returned by the newly
// active screen, so Players' initial roster + journal fetch never ran — the
// tab sat at its zero state ("0 / 0", "no player activity") until the
// operator pressed u. Switching tabs must schedule that first poll.
func TestApp_SwitchTabSchedulesInitialPoll(t *testing.T) {
	host := &fakeServerHost{}
	fc := &playersFakeConsole{replies: []string{"There are 1/15 players online:\nbrea [xuid: 1111111111111111]"}}
	st := newPlayersRig(t, fc, nil)

	app := NewApp()
	app.SetStatusProvider(statProvider(compass.Status{}))
	app.Screens[1] = NewPlayers(host, st)

	_, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	if cmd == nil {
		t.Fatal("switching to Players must schedule its initial poll")
	}
	msgs := drainCmd(t, cmd)
	var sawRoster bool
	for _, m := range msgs {
		if rm, ok := m.(playersRosterMsg); ok && rm.err == "" {
			sawRoster = true
			if rm.roster.Max != 15 || len(rm.roster.Online) != 1 || rm.roster.Online[0].Name != "brea" {
				t.Errorf("roster = %+v, want brea online (max 15)", rm.roster)
			}
		}
	}
	if !sawRoster {
		t.Fatalf("switch-tab command produced %v, want the roster fetch", msgs)
	}
}

// drainCmd runs a (possibly batched) tea.Cmd and collects its messages. In
// bubbletea v1.x, Batch yields a BatchMsg of sub-commands that the tea loop
// expands concurrently; tests expand it one level here.
func drainCmd(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	switch m := cmd().(type) {
	case tea.BatchMsg:
		var out []tea.Msg
		for _, sub := range m {
			out = append(out, drainCmd(t, sub)...)
		}
		return out
	default:
		return []tea.Msg{m}
	}
}
