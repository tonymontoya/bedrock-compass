package compass

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeConsole records every command sent and returns a canned reply, so the
// Players service is testable offline (same seam style as the lifecycle
// tests: Console's function fields are injectable). The real Send harvests
// via NewSince until quiescence; the fake delivers each reply on the first
// poll after its command was written and stays silent afterwards.
type fakeConsole struct {
	replies   []string // one per Send call; last repeats
	calls     []string
	delivered int
	err       error
}

func newFakeConsole(replies ...string) *fakeConsole {
	return &fakeConsole{replies: replies}
}

// consoleOver wires a real *Console (real Send/harvest loop, 1 ms quiescence)
// over the fake's seams.
func consoleOver(f *fakeConsole) *Console {
	return &Console{
		Write: func(ctx context.Context, line string) error {
			if f.err != nil {
				return f.err
			}
			f.calls = append(f.calls, line)
			return nil
		},
		Baseline: func(ctx context.Context) (string, error) { return "", nil },
		NewSince: func(ctx context.Context, token string) ([]string, error) {
			if len(f.calls) == 0 || f.delivered >= len(f.calls) {
				return nil, nil
			}
			i := len(f.calls) - 1
			if i >= len(f.replies) {
				i = len(f.replies) - 1
			}
			if i < 0 { // no canned replies at all: the command produced nothing
				f.delivered++
				return nil, nil
			}
			reply := f.replies[i]
			f.delivered++
			if reply == "" {
				return nil, nil
			}
			return strings.Split(reply, "\n"), nil
		},
		Policy: ConsolePolicy{Quiesce: time.Millisecond, Timeout: 50 * time.Millisecond, Poll: time.Millisecond},
	}
}

// playersRig wires a Players service over the fake console with a real trail.
func playersRig(t *testing.T, f *fakeConsole) (*Players, *Trail) {
	t.Helper()
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return NewPlayers(consoleOver(f), trail, nil), trail
}

func playerEvents(t *testing.T, tr *Trail) []Event {
	t.Helper()
	evs, err := tr.Recent(50)
	if err != nil {
		t.Fatalf("trail Recent: %v", err)
	}
	var out []Event
	for _, e := range evs {
		if e.Verb == "player" {
			out = append(out, e)
		}
	}
	return out
}

// --- ParsePlayerLine (the shared D9 parser) ---

func TestParsePlayerLine_Connected(t *testing.T) {
	name, xuid, kind, ok := ParsePlayerLine("Player connected: Tony75264, xuid: 2533274865350919")
	if !ok || name != "Tony75264" || xuid != "2533274865350919" || kind != PlayerEventConnected {
		t.Errorf("got (%q, %q, %v, %v)", name, xuid, kind, ok)
	}
}

func TestParsePlayerLine_DisconnectedWithPfid(t *testing.T) {
	name, xuid, kind, ok := ParsePlayerLine(
		"Player disconnected: Tony75264, xuid: 2533274865350919, pfid: ED6EDB84C3BDED35")
	if !ok || name != "Tony75264" || xuid != "2533274865350919" || kind != PlayerEventDisconnected {
		t.Errorf("got (%q, %q, %v, %v)", name, xuid, kind, ok)
	}
}

func TestParsePlayerLine_FullJournalctlLine(t *testing.T) {
	line := "Aug 19 01:54:21 minecraft sh[107202]: [2026-08-19 01:54:21:901 INFO] Player connected: breal875623, xuid: 2535465377145824"
	name, xuid, kind, ok := ParsePlayerLine(line)
	if !ok || name != "breal875623" || xuid != "2535465377145824" || kind != PlayerEventConnected {
		t.Errorf("got (%q, %q, %v, %v)", name, xuid, kind, ok)
	}
}

func TestParsePlayerLine_NoXUID(t *testing.T) {
	name, xuid, _, ok := ParsePlayerLine("Player connected: SomeName")
	if !ok || name != "SomeName" || xuid != "" {
		t.Errorf("got (%q, %q, %v)", name, xuid, ok)
	}
}

func TestParsePlayerLine_NonPlayerLines(t *testing.T) {
	for _, line := range []string{
		"There are 0/15 players online:",
		"[2026-08-19 01:54:21:901 INFO] Server started.",
		"Saving...",
		"",
	} {
		if _, _, _, ok := ParsePlayerLine(line); ok {
			t.Errorf("line %q should not parse as a player event", line)
		}
	}
}

// --- ParseRoster (defensive: bare name, [xuid:], (N)) ---

func TestParseRoster_ZeroPlayers(t *testing.T) {
	r, err := ParseRoster("There are 0/15 players online:")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.Count != 0 || r.Max != 15 || len(r.Online) != 0 {
		t.Errorf("got %+v", r)
	}
}

func TestParseRoster_BareNames(t *testing.T) {
	out := "There are 2/15 players online:\nTony75264\nbreal875623"
	r, err := ParseRoster(out)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.Count != 2 || len(r.Online) != 2 {
		t.Fatalf("got %+v", r)
	}
	if r.Online[0].Name != "Tony75264" || r.Online[1].Name != "breal875623" {
		t.Errorf("names = %v", r.Online)
	}
}

func TestParseRoster_NameWithXUIDBracket(t *testing.T) {
	out := "There are 1/15 players online:\nTony75264 [xuid: 2533274865350919]"
	r, err := ParseRoster(out)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(r.Online) != 1 || r.Online[0].Name != "Tony75264" || r.Online[0].XUID != "2533274865350919" {
		t.Errorf("got %+v", r.Online)
	}
}

func TestParseRoster_NameWithXUIDParens(t *testing.T) {
	out := "There are 1/15 players online:\nTony75264 (2533274865350919)"
	r, err := ParseRoster(out)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(r.Online) != 1 || r.Online[0].Name != "Tony75264" || r.Online[0].XUID != "2533274865350919" {
		t.Errorf("got %+v", r.Online)
	}
}

func TestParseRoster_TrimsRowsPastCount(t *testing.T) {
	// Interleaved journal noise after the header: only `count` rows are real.
	out := "There are 1/15 players online:\nTony75264\nPlayer connected: breal875623, xuid: 2535465377145824"
	r, err := ParseRoster(out)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(r.Online) != 1 || r.Online[0].Name != "Tony75264" {
		t.Errorf("got %+v (event line must not be a roster row)", r.Online)
	}
}

func TestParseRoster_NoHeader(t *testing.T) {
	if _, err := ParseRoster("some unrelated output"); err == nil {
		t.Error("expected an error when no roster header is present")
	}
}

// --- MinePlayerHistory (recently-connected mining, D3b) ---

func TestMinePlayerHistory_DedupesByXUID_NewestFirst(t *testing.T) {
	lines := []string{
		"[2026-08-19 01:30:22:503 INFO] Player connected: breal875623, xuid: 2535465377145824",
		"[2026-08-19 01:54:21:901 INFO] Player connected: Tony75264, xuid: 2533274865350919",
		"[2026-08-19 02:56:29:353 INFO] Player disconnected: Tony75264, xuid: 2533274865350919, pfid: ED6EDB84C3BDED35",
	}
	got := MinePlayerHistory(lines)
	if len(got) != 2 {
		t.Fatalf("got %d players: %+v", len(got), got)
	}
	// Tony's last activity (disconnect at 02:56) is newer than breal's.
	if got[0].Name != "Tony75264" || got[0].XUID != "2533274865350919" {
		t.Errorf("newest = %+v, want Tony75264", got[0])
	}
	if got[0].State != "disconnected" {
		t.Errorf("Tony state = %q, want disconnected (last event)", got[0].State)
	}
	if got[1].Name != "breal875623" || got[1].State != "connected" {
		t.Errorf("second = %+v", got[1])
	}
	wantAt, _ := time.Parse("2006-01-02 15:04:05", "2026-08-19 02:56:29")
	if !got[0].LastSeen.Equal(wantAt) {
		t.Errorf("Tony LastSeen = %v, want %v", got[0].LastSeen, wantAt)
	}
}

func TestMinePlayerHistory_CorrelatesByNameWhenNoXUID(t *testing.T) {
	lines := []string{
		"[2026-08-19 01:30:22:503 INFO] Player connected: breal875623, xuid: 2535465377145824",
		"[2026-08-19 02:00:00:000 INFO] Player disconnected: breal875623", // no xuid on this line
	}
	got := MinePlayerHistory(lines)
	if len(got) != 1 || got[0].Name != "breal875623" || got[0].XUID != "2535465377145824" {
		t.Fatalf("got %+v, want one breal875623 with the XUID from the earlier line", got)
	}
	if got[0].State != "disconnected" {
		t.Errorf("state = %q, want disconnected", got[0].State)
	}
}

func TestMinePlayerHistory_CapsAt25(t *testing.T) {
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, "[2026-08-19 01:30:22:503 INFO] Player connected: p"+string(rune('a'+i%26))+string(rune('0'+i/26))+", xuid: 1000000000000000"+strconv.Itoa(i))
	}
	got := MinePlayerHistory(lines)
	if len(got) != 25 {
		t.Errorf("got %d players, want the cap of 25", len(got))
	}
}

func TestPlayers_Recent_NilJournalDegrades(t *testing.T) {
	s, _ := playersRig(t, newFakeConsole())
	if _, err := s.Recent(context.Background()); err == nil {
		t.Error("expected an error when the deployment has no journal")
	}
}

func TestPlayers_Recent_MinesJournalTail(t *testing.T) {
	f := newFakeConsole()
	s, _ := playersRig(t, f)
	lines := []string{
		"[2026-08-19 01:30:22:503 INFO] Player connected: breal875623, xuid: 2535465377145824",
		"[2026-08-19 01:54:21:901 INFO] Player connected: Tony75264, xuid: 2533274865350919",
	}
	s.Journal = &Journal{Unit: "u"}
	// The fake returns full journalctl plain-output lines (syslog prefix +
	// message), as RecentLines' exec seam sees them.
	jl := make([]string, len(lines))
	for i, l := range lines {
		jl[i] = "Aug 19 01:30:22 minecraft sh[74883]: " + l
	}
	s.Journal.Exec = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte(strings.Join(jl, "\n")), nil
	}
	got, err := s.Recent(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 || got[0].Name != "Tony75264" {
		t.Errorf("got %+v", got)
	}
}

// --- classifyReply (failure markers verified live on 1.26.43) ---

func TestClassifyReply_FailureMarkers(t *testing.T) {
	cases := []struct {
		out     string
		marker  string
		wantErr bool
	}{
		{"Could not find player definitely-not-a-player", "", true},
		{"No targets matched selector", "", true},
		{"[2026-08-20 13:12:35:306 ERROR] No targets matched selector\nCould not find player x", "to the allowlist", true},
		{"Added definitely-not-a-player to the allowlist", "to the allowlist", false},
		{"Removed definitely-not-a-player from the allowlist", "", false},
		{"", "", false}, // silence on success (kick/op/deop)
	}
	for i, c := range cases {
		detail, ok := classifyReply(c.out, c.marker)
		if c.wantErr && ok {
			t.Errorf("case %d: want failure for %q, got ok (%q)", i, c.out, detail)
		}
		if !c.wantErr && !ok {
			t.Errorf("case %d: want success for %q, got failure line %q", i, c.out, detail)
		}
	}
}

func TestClassifyReply_DuplicateAllowlistAdd(t *testing.T) {
	detail, ok := classifyReply("[2026-08-20 14:30:28:791 ERROR] Could not add definitely-not-a-player to the allowlist", "to the allowlist")
	if ok {
		t.Errorf("duplicate add must be a failure, got success (%q)", detail)
	}
	if !strings.Contains(detail, "Could not add") {
		t.Errorf("failure line = %q, want the verbatim server line", detail)
	}
}

// --- Players actions: command shape + audit (D11) ---

func TestPlayers_Kick_SendsGamertagFirst(t *testing.T) {
	f := newFakeConsole() // silence on success
	s, tr := playersRig(t, f)
	detail, err := s.Kick(context.Background(), Player{Name: "Tony75264", XUID: "2533274865350919"})
	if err != nil {
		t.Fatalf("err = %v (silence on success must not be an error)", err)
	}
	if detail != "" {
		t.Errorf("detail = %q, want empty", detail)
	}
	if len(f.calls) != 1 || f.calls[0] != "kick Tony75264" {
		t.Fatalf("calls = %v, want [kick Tony75264]", f.calls)
	}
	evs := playerEvents(t, tr)
	if len(evs) != 1 || evs[0].Params["action"] != "kick" || evs[0].Result != "ok" {
		t.Errorf("audit = %+v", evs)
	}
}

func TestPlayers_Kick_UnknownPlayerSurfacesServerLine(t *testing.T) {
	f := newFakeConsole("[2026-08-20 13:12:35:306 ERROR] No targets matched selector\nCould not find player definitely-not-a-player")
	s, tr := playersRig(t, f)
	_, err := s.Kick(context.Background(), Player{Name: "definitely-not-a-player"})
	if err == nil {
		t.Fatal("expected an error for a kick that matched nobody")
	}
	if !strings.Contains(err.Error(), "Could not find player") {
		t.Errorf("err = %v, want the verbatim server line", err)
	}
	evs := playerEvents(t, tr)
	if len(evs) != 1 || evs[0].Result == "ok" {
		t.Errorf("audit must record the failure: %+v", evs)
	}
}

func TestPlayers_OpDeop_CommandShape(t *testing.T) {
	f := newFakeConsole()
	s, _ := playersRig(t, f)
	if _, err := s.Op(context.Background(), Player{Name: "Tony75264"}); err != nil {
		t.Fatalf("Op: %v", err)
	}
	if _, err := s.Deop(context.Background(), Player{Name: "Tony75264"}); err != nil {
		t.Fatalf("Deop: %v", err)
	}
	want := []string{"op Tony75264", "deop Tony75264"}
	if len(f.calls) != 2 || f.calls[0] != want[0] || f.calls[1] != want[1] {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
}

func TestPlayers_Allow_SendsGamertagAndAudits(t *testing.T) {
	f := newFakeConsole("[2026-08-20 14:30:27:638 INFO] Added definitely-not-a-player to the allowlist")
	s, tr := playersRig(t, f)
	detail, err := s.Allow(context.Background(), Player{Name: "definitely-not-a-player"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(detail, "Added") {
		t.Errorf("detail = %q, want the server's confirmation line", detail)
	}
	if f.calls[0] != "allowlist add definitely-not-a-player" {
		t.Errorf("call = %q, want allowlist add <gamertag>", f.calls[0])
	}
	evs := playerEvents(t, tr)
	if len(evs) != 1 || evs[0].Params["action"] != "allow" || evs[0].Result != "ok" {
		t.Errorf("audit = %+v", evs)
	}
}

func TestPlayers_Allow_FallsBackToXUID(t *testing.T) {
	f := newFakeConsole("[2026-08-20 14:30:27:638 INFO] Added 2533274865350919 to the allowlist")
	s, _ := playersRig(t, f)
	// No gamertag known (should not happen from this tool's own data, but
	// the identity handoff must still work XUID-only).
	if _, err := s.Allow(context.Background(), Player{XUID: "2533274865350919"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if f.calls[0] != "allowlist add 2533274865350919" {
		t.Errorf("call = %q, want allowlist add <xuid>", f.calls[0])
	}
}

func TestPlayers_Allow_DuplicateIsAnError(t *testing.T) {
	f := newFakeConsole("[2026-08-20 14:30:28:791 ERROR] Could not add definitely-not-a-player to the allowlist")
	s, _ := playersRig(t, f)
	if _, err := s.Allow(context.Background(), Player{Name: "definitely-not-a-player"}); err == nil {
		t.Fatal("duplicate allowlist add must be an error (verified live)")
	}
}

func TestPlayers_NoConsoleFailsCleanly(t *testing.T) {
	s, _ := playersRig(t, newFakeConsole())
	s.Console = nil
	if _, err := s.Kick(context.Background(), Player{Name: "x"}); err == nil {
		t.Error("expected an error with no console channel")
	}
}

func TestPlayers_Roster_ParsesListReply(t *testing.T) {
	f := newFakeConsole("There are 2/15 players online:\nTony75264\nbreal875623")
	s, _ := playersRig(t, f)
	r, err := s.Roster(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.Count != 2 || len(r.Online) != 2 {
		t.Errorf("got %+v", r)
	}
	if f.calls[0] != "list" {
		t.Errorf("call = %q, want list", f.calls[0])
	}
}

func TestPlayers_Roster_SendErrorSurfaces(t *testing.T) {
	f := newFakeConsole()
	f.err = errors.New("fifo: no such file")
	s, _ := playersRig(t, f)
	if _, err := s.Roster(context.Background()); err == nil {
		t.Error("expected the send error to surface")
	}
}
