package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// newLogsRig builds a Stack whose Logs service reads from a canned journal,
// so the screen's async plumbing is testable without a real unit.
func newLogsRig(t *testing.T) *compass.Stack {
	t.Helper()
	j := &compass.Journal{Unit: "minecraft-bedrock"}
	return &compass.Stack{Logs: &compass.Logs{Journal: j}}
}

// feedSnapshot drives the screen's initial-load message with canned entries.
func feedSnapshot(t *testing.T, s *Logs, lines []string, cursor string) {
	t.Helper()
	s.Update(logsSnapshotMsg{entries: compass.ParseEntries(lines), cursor: cursor})
}

// typeInto feeds runes straight into a Logs screen (the package-level typeStr
// helper drives the shell's App instead).
func typeInto(t *testing.T, s *Logs, str string) {
	t.Helper()
	for _, r := range str {
		s.Update(key(r))
	}
}

var logSample = []string{
	"[2026-08-19 01:30:22:503 INFO] Server started.",
	"[2026-08-19 01:31:00:000 WARN] Disk almost full",
	"[2026-08-19 01:32:00:000 ERROR] Crash dump written",
	"[2026-08-19 01:33:00:000 INFO] Player connected: player_two, xuid: 2222222222222222",
}

func TestLogs_View_Snapshot(t *testing.T) {
	s := NewLogs(newLogsRig(t))
	feedSnapshot(t, s, logSample, "cur-1")
	view := s.View()

	for _, want := range []string{
		"Logs (minecraft-bedrock)",
		"Server started.",
		"Disk almost full",
		"Crash dump written",
		"Player connected: player_two",
		"level:all",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("View is missing %q\nview:\n%s", want, view)
		}
	}
	if s.cursor != "cur-1" || !s.loaded {
		t.Fatalf("cursor=%q loaded=%v", s.cursor, s.loaded)
	}
}

func TestLogs_View_DegradedNoService(t *testing.T) {
	s := NewLogs(nil)
	view := s.View()
	if !strings.Contains(view, "logs unavailable") {
		t.Errorf("degraded view missing notice:\n%s", view)
	}
}

func TestLogs_PauseToggle(t *testing.T) {
	s := NewLogs(newLogsRig(t))
	feedSnapshot(t, s, logSample, "")
	if strings.Contains(s.View(), "[paused]") {
		t.Fatal("should start unpaused")
	}
	s.Update(key('p'))
	if !strings.Contains(s.View(), "[paused]") {
		t.Fatal("p should pause the live tail")
	}
	s.Update(key('p'))
	if strings.Contains(s.View(), "[paused]") {
		t.Fatal("second p should resume")
	}
}

func TestLogs_LevelFilterCycle(t *testing.T) {
	s := NewLogs(newLogsRig(t))
	feedSnapshot(t, s, logSample, "")

	s.Update(key('l')) // INFO only
	view := s.View()
	if !strings.Contains(view, "level:INFO") || strings.Contains(view, "Disk almost full") {
		t.Fatalf("INFO filter wrong:\n%s", view)
	}
	s.Update(key('l')) // WARN only
	view = s.View()
	if !strings.Contains(view, "level:WARN") || !strings.Contains(view, "Disk almost full") || strings.Contains(view, "Crash dump written") {
		t.Fatalf("WARN filter wrong:\n%s", view)
	}
	s.Update(key('l')) // ERROR only
	view = s.View()
	if !strings.Contains(view, "level:ERROR") || !strings.Contains(view, "Crash dump written") || strings.Contains(view, "Server started.") {
		t.Fatalf("ERROR filter wrong:\n%s", view)
	}
	s.Update(key('l')) // back to all
	view = s.View()
	if !strings.Contains(view, "level:all") || !strings.Contains(view, "Server started.") {
		t.Fatalf("cycle should return to all:\n%s", view)
	}
}

func TestLogs_PlayerFilterInput(t *testing.T) {
	s := NewLogs(newLogsRig(t))
	feedSnapshot(t, s, logSample, "")

	if s.CapturingKeys() {
		t.Fatal("should not capture keys outside input mode")
	}
	s.Update(key('f'))
	if !s.CapturingKeys() {
		t.Fatal("f should open the player field and capture keys")
	}
	typeInto(t, s, "player_two")
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})

	view := s.View()
	if !strings.Contains(view, "player:player_two") {
		t.Fatalf("player filter not shown:\n%s", view)
	}
	if strings.Contains(view, "Server started.") || strings.Contains(view, "Crash dump written") {
		t.Fatalf("unrelated lines must be filtered out:\n%s", view)
	}
	if !strings.Contains(view, "Player connected: player_two") {
		t.Fatalf("the player's own line must remain:\n%s", view)
	}

	// Clearable back to unfiltered: open the field and apply an empty value.
	s.Update(key('f'))
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	view = s.View()
	if !strings.Contains(view, "Server started.") {
		t.Fatalf("clearing the player filter should restore all lines:\n%s", view)
	}
}

func TestLogs_PlayerFilterEscCancels(t *testing.T) {
	s := NewLogs(newLogsRig(t))
	feedSnapshot(t, s, logSample, "")
	s.Update(key('f'))
	typeInto(t, s, "brea")
	s.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if s.filters.Player != "" || s.CapturingKeys() {
		t.Fatal("Esc must cancel the field without applying it")
	}
	if !strings.Contains(s.View(), "Server started.") {
		t.Fatal("all lines should still be visible after Esc")
	}
}

func TestLogs_SetSearchFromShell(t *testing.T) {
	s := NewLogs(newLogsRig(t))
	feedSnapshot(t, s, logSample, "")
	s.SetSearch("disk")
	view := s.View()
	if !strings.Contains(view, `search:"disk"`) || strings.Contains(view, "Server started.") {
		t.Fatalf("text search should filter:\n%s", view)
	}
	s.SetSearch("") // empty clears it
	if !strings.Contains(s.View(), "Server started.") {
		t.Fatal("clearing the text search should restore all lines")
	}
}

func TestLogs_FiltersCompose(t *testing.T) {
	s := NewLogs(newLogsRig(t))
	feedSnapshot(t, s, logSample, "")

	// level=INFO + search "dump": the only dump line is ERROR, so nothing
	// matches — both filters must hold (AND).
	s.Update(key('l')) // INFO only
	s.SetSearch("dump")
	view := s.View()
	if !strings.Contains(view, "(no lines match the current filters)") {
		t.Fatalf("level=INFO + search=dump should leave no lines:\n%s", view)
	}

	// Cycle back to all levels (WARN -> ERROR -> all): the same search now
	// matches only the ERROR dump line.
	s.Update(key('l'))
	s.Update(key('l'))
	s.Update(key('l'))
	view = s.View()
	if !strings.Contains(view, "Crash dump written") || strings.Contains(view, "Server started.") {
		t.Fatalf("search alone should show only the matching line:\n%s", view)
	}

	// Clearing the search restores everything.
	s.SetSearch("")
	view = s.View()
	if !strings.Contains(view, "Server started.") || !strings.Contains(view, "Crash dump written") {
		t.Fatalf("clearing search should restore all lines:\n%s", view)
	}
}

func TestLogs_RangePresetCycle(t *testing.T) {
	s := NewLogs(newLogsRig(t))
	feedSnapshot(t, s, logSample, "")

	s.Update(key('t')) // last hour (historical)
	if !strings.Contains(s.View(), "range:last hour") {
		t.Fatalf("first t should switch to the last-hour window:\n%s", s.View())
	}
	hist := []string{"[2026-08-19 01:32:00:000 ERROR] Crash dump written"}
	s.Update(logsRangeMsg{entries: compass.ParseEntries(hist)})
	view := s.View()
	if !strings.Contains(view, "Crash dump written") || strings.Contains(view, "Server started.") {
		t.Fatalf("historical view should show only the window's lines:\n%s", view)
	}

	s.Update(key('t')) // today
	if !strings.Contains(s.View(), "range:today") {
		t.Fatal("second t should switch to today")
	}
	s.Update(logsRangeMsg{entries: compass.ParseEntries(hist)})

	s.Update(key('t')) // back to live (re-anchored snapshot)
	feedSnapshot(t, s, logSample, "cur-live")
	view = s.View()
	if !strings.Contains(view, "live") || strings.Contains(view, "[paused]") {
		t.Fatalf("third t should return to the live tail:\n%s", view)
	}
	if s.view != viewLive || s.rangeLabel != "all" {
		t.Fatalf("view=%v label=%q, want live/all", int(s.view), s.rangeLabel)
	}
}

func TestLogs_CustomRangeInput(t *testing.T) {
	s := NewLogs(newLogsRig(t))
	feedSnapshot(t, s, logSample, "")

	s.Update(key('c'))
	typeInto(t, s, "not-a-time")
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.mode == logsInputNone {
		t.Fatal("an invalid spec must keep the field open with an error")
	}
	if !strings.Contains(s.View(), "cannot parse not-a-time") {
		t.Fatalf("parse error should be shown:\n%s", s.View())
	}

	// The failed attempt leaves its text in the buffer; clear it, then apply
	// a valid spec.
	for i := 0; i < len("not-a-time"); i++ {
		s.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	typeInto(t, s, "2026-08-19 01:30:00,2026-08-19 01:31:59")
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.view != viewHistorical || s.mode != logsInputNone {
		t.Fatalf("valid spec should apply and close the field (view=%v mode=%v)", int(s.view), int(s.mode))
	}
	wantSince := time.Date(2026, 8, 19, 1, 30, 0, 0, time.Local)
	if !s.since.Equal(wantSince) {
		t.Fatalf("since=%v", s.since)
	}
	s.Update(logsRangeMsg{entries: compass.ParseEntries([]string{"[2026-08-19 01:30:22:503 INFO] Server started."})})
	if !strings.Contains(s.View(), "Server started.") {
		t.Fatalf("custom range result should render:\n%s", s.View())
	}
}

func TestLogs_PollAppendsAndAdvancesCursor(t *testing.T) {
	s := NewLogs(newLogsRig(t))
	feedSnapshot(t, s, logSample[:2], "cur-1")
	s.Update(logsPollMsg{entries: compass.ParseEntries(logSample[2:]), cursor: "cur-2"})

	view := s.View()
	if !strings.Contains(view, "Crash dump written") || !strings.Contains(view, "Player connected: player_two") {
		t.Fatalf("polled lines should be appended:\n%s", view)
	}
	if s.cursor != "cur-2" {
		t.Fatalf("cursor=%q, want cur-2", s.cursor)
	}

	// An empty poll must not advance the cursor.
	s.Update(logsPollMsg{entries: nil, cursor: ""})
	if s.cursor != "cur-2" {
		t.Fatalf("empty poll advanced cursor to %q", s.cursor)
	}
}

func TestLogs_Scrollback(t *testing.T) {
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, "[2026-08-19 01:30:00:000 INFO] line "+itoaTest(i))
	}
	s := NewLogs(newLogsRig(t))
	feedSnapshot(t, s, lines, "")

	view := s.View()
	if strings.Contains(view, "line 0") {
		t.Fatal("the newest viewport should not show the oldest line initially")
	}
	for i := 0; i < 40; i++ { // scroll up past everything
		s.Update(tea.KeyMsg{Type: tea.KeyUp})
	}
	view = s.View()
	if !strings.Contains(view, "line 0") || !strings.Contains(view, "older line(s)") {
		t.Fatalf("scrolling to the top should reveal old lines + hint:\n%s", view)
	}

	// Scrolling down returns to the newest line.
	for i := 0; i < 80; i++ {
		s.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if s.offset != 0 {
		t.Fatalf("offset=%d, want back at 0 (newest)", s.offset)
	}
}

func TestLogs_ScrollbackCap(t *testing.T) {
	var lines []string
	for i := 0; i < logScrollback+100; i++ {
		lines = append(lines, "[2026-08-19 01:30:00:000 INFO] line "+itoaTest(i))
	}
	s := NewLogs(newLogsRig(t))
	feedSnapshot(t, s, lines, "")
	if len(s.entries) != logScrollback {
		t.Fatalf("entries=%d, want capped at %d", len(s.entries), logScrollback)
	}
	last := s.entries[len(s.entries)-1]
	if !strings.Contains(last.Text, itoaTest(logScrollback+99)) {
		t.Fatalf("cap should keep the newest lines; last=%q", last.Text)
	}
}

func TestLogs_WindowSizeForwarded(t *testing.T) {
	s := NewLogs(newLogsRig(t))
	feedSnapshot(t, s, logSample, "")
	app := NewApp()
	app.Screens[0] = s
	app.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if s.width != 120 || s.height != 40 {
		t.Fatalf("screen got %dx%d, want the forwarded window size", s.width, s.height)
	}
}

func TestLogs_ShellSearchAppliesOnEnter(t *testing.T) {
	s := NewLogs(newLogsRig(t))
	feedSnapshot(t, s, logSample, "")
	app := NewApp()
	app.Screens[0] = s

	app.Update(key('/'))
	typeStr(t, app, "disk")
	app.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if app.searchOpen {
		t.Fatal("search should close on Enter")
	}
	if s.filters.Text != "disk" {
		t.Fatalf("shell search not applied to the screen; filters.Text=%q", s.filters.Text)
	}

	// Esc closes without applying.
	app.Update(key('/'))
	typeStr(t, app, "crash")
	app.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if s.filters.Text != "disk" {
		t.Fatalf("Esc must not apply the buffer; filters.Text=%q", s.filters.Text)
	}
}

func TestParseRangeSpec(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		wantErr bool
	}{
		{"since only", "2026-08-19 01:30:00", false},
		{"minute precision", "2026-08-19 01:30", false},
		{"since and until", "2026-08-19 01:30:00,2026-08-19 02:00:00", false},
		{"empty", "", true},
		{"garbage", "yesterday", true},
		{"bad until", "2026-08-19 01:30:00,soon", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			since, until, err := parseRangeSpec(c.spec)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, c.wantErr)
			}
			if !c.wantErr && since.IsZero() {
				t.Fatal("since should be parsed")
			}
			if !c.wantErr && strings.Contains(c.spec, ",") && until.IsZero() {
				t.Fatal("until should be parsed when present")
			}
		})
	}
}

// itoaTest is a test-local int->string (the screen uses fmt.Sprintf).
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
