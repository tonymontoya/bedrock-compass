package compass

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseLogLine_Tagged(t *testing.T) {
	e := ParseLogLine("[2026-08-19 01:30:22:503 INFO] Player connected: breal875623, xuid: 2535465377145824")
	if e.Level != "INFO" {
		t.Fatalf("level=%q", e.Level)
	}
	want := time.Date(2026, 8, 19, 1, 30, 22, 0, time.Local)
	if !e.Time.Equal(want) {
		t.Fatalf("time=%v, want %v", e.Time, want)
	}
	if e.Text != "[2026-08-19 01:30:22:503 INFO] Player connected: breal875623, xuid: 2535465377145824" {
		t.Fatalf("text=%q", e.Text)
	}
}

func TestParseLogLine_WarnAndError(t *testing.T) {
	w := ParseLogLine("[2026-08-19 01:31:00:000 WARN] Some warning")
	if w.Level != "WARN" {
		t.Fatalf("level=%q", w.Level)
	}
	e := ParseLogLine("[2026-08-19 01:32:00:000 ERROR] Something failed")
	if e.Level != "ERROR" {
		t.Fatalf("level=%q", e.Level)
	}
}

func TestParseLogLine_Untagged(t *testing.T) {
	e := ParseLogLine("/? <page: int>")
	if !e.Time.IsZero() || e.Level != "" {
		t.Fatalf("untagged entry should have zero time and empty level, got %v %q", e.Time, e.Level)
	}
	if e.Text != "/? <page: int>" {
		t.Fatalf("text=%q", e.Text)
	}
}

func TestParseLogLine_FullJournalctlLine(t *testing.T) {
	e := ParseLogLine("Aug 19 01:54:21 minecraft sh[107202]: [2026-08-19 01:54:21:901 INFO] Player connected: Tony75264, xuid: 2533274865350919")
	if e.Level != "INFO" {
		t.Fatalf("level=%q", e.Level)
	}
	want := time.Date(2026, 8, 19, 1, 54, 21, 0, time.Local)
	if !e.Time.Equal(want) {
		t.Fatalf("time=%v, want %v", e.Time, want)
	}
}

func TestFilterEntries_NoFilters(t *testing.T) {
	in := ParseEntries([]string{
		"[2026-08-19 01:30:22:503 INFO] Player connected: breal875623, xuid: 2535465377145824",
		"/? <page: int>",
	})
	out := FilterEntries(in, LogFilters{})
	if len(out) != 2 {
		t.Fatalf("got %d entries, want all 2", len(out))
	}
}

func TestFilterEntries_Level(t *testing.T) {
	in := ParseEntries([]string{
		"[2026-08-19 01:30:22:503 INFO] Server started.",
		"[2026-08-19 01:31:00:000 WARN] Disk almost full",
		"[2026-08-19 01:32:00:000 ERROR] Crash dump written",
	})
	out := FilterEntries(in, LogFilters{Level: "warn"})
	if len(out) != 1 || out[0].Text != "[2026-08-19 01:31:00:000 WARN] Disk almost full" {
		t.Fatalf("got %q", out)
	}
	out = FilterEntries(in, LogFilters{Level: "INFO"})
	if len(out) != 1 || out[0].Text != "[2026-08-19 01:30:22:503 INFO] Server started." {
		t.Fatalf("got %q", out)
	}
}

func TestFilterEntries_Text(t *testing.T) {
	in := ParseEntries([]string{
		"[2026-08-19 01:30:22:503 INFO] Player connected: breal875623, xuid: 2535465377145824",
		"[2026-08-19 01:31:00:000 WARN] Disk almost full",
	})
	out := FilterEntries(in, LogFilters{Text: "disk"})
	if len(out) != 1 || out[0].Level != "WARN" {
		t.Fatalf("got %q", out)
	}
}

func TestFilterEntries_Player(t *testing.T) {
	in := ParseEntries([]string{
		"[2026-08-19 01:30:22:503 INFO] Player connected: breal875623, xuid: 2535465377145824",
		"[2026-08-19 01:31:00:000 INFO] Player disconnected: Tony75264, xuid: 2533274865350919",
		"[2026-08-19 01:32:00:000 INFO] breal875623 issued server command: /time set day",
		"[2026-08-19 01:33:00:000 INFO] Player connected: Tony75264, xuid: 2533274865350919",
	})
	out := FilterEntries(in, LogFilters{Player: "breal875623"})
	if len(out) != 2 {
		t.Fatalf("got %d entries %q, want 2 (connect + command mention)", len(out), out)
	}
	if !strings.Contains(out[0].Text, "connected") || !strings.Contains(out[1].Text, "/time set day") {
		t.Fatalf("wrong lines kept: %q", out)
	}

	out = FilterEntries(in, LogFilters{Player: "tony75264"})
	if len(out) != 2 {
		t.Fatalf("case-insensitive match failed: got %d entries %q", len(out), out)
	}
}

func TestFilterEntries_PlayerNoMatch(t *testing.T) {
	in := ParseEntries([]string{
		"[2026-08-19 01:30:22:503 INFO] Player connected: breal875623, xuid: 2535465377145824",
	})
	out := FilterEntries(in, LogFilters{Player: "someoneelse"})
	if len(out) != 0 {
		t.Fatalf("got %q, want none", out)
	}
}

func TestFilterEntries_TimeRange(t *testing.T) {
	in := ParseEntries([]string{
		"[2026-08-19 01:30:22:503 INFO] early line",
		"/? <page: int>",
		"[2026-08-19 02:00:00:000 INFO] mid line",
		"[2026-08-19 03:00:00:000 INFO] late line",
	})
	since := time.Date(2026, 8, 19, 1, 45, 0, 0, time.Local)
	until := time.Date(2026, 8, 19, 2, 30, 0, 0, time.Local)
	out := FilterEntries(in, LogFilters{Since: since, Until: until})
	if len(out) != 1 || out[0].Text != "[2026-08-19 02:00:00:000 INFO] mid line" {
		t.Fatalf("got %q", out)
	}

	out = FilterEntries(in, LogFilters{Since: since})
	if len(out) != 2 {
		t.Fatalf("open-ended until failed: got %d entries", len(out))
	}
}

func TestFilterEntries_Composed(t *testing.T) {
	in := ParseEntries([]string{
		"[2026-08-19 01:30:22:503 INFO] Player connected: breal875623, xuid: 2535465377145824",
		"[2026-08-19 01:31:00:000 WARN] Disk almost full",
		"[2026-08-19 02:30:00:000 ERROR] breal875623 kicked for cheating",
	})
	f := LogFilters{
		Player: "breal875623",
		Level:  "ERROR",
		Since:  time.Date(2026, 8, 19, 2, 0, 0, 0, time.Local),
	}
	out := FilterEntries(in, f)
	if len(out) != 1 || out[0].Text != "[2026-08-19 02:30:00:000 ERROR] breal875623 kicked for cheating" {
		t.Fatalf("got %q", out)
	}
}

// fakeJournal returns an ExecFunc that records its args and replays canned
// stdout, so Logs' journal plumbing is testable without a real journal.
type fakeJournal struct {
	stdout string
	args   []string
}

func (f *fakeJournal) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.args = append(f.args, args...)
	return []byte(f.stdout), nil
}

func TestLogs_Snapshot_ParsesAndReturnsCursor(t *testing.T) {
	fj := &fakeJournal{stdout: "Aug 19 01:30:22 minecraft sh[1]: [2026-08-19 01:30:22:503 INFO] Server started.\n" +
		"-- cursor: s=abc;i=def\n"}
	l := &Logs{Journal: &Journal{Unit: "minecraft-bedrock", Exec: fj.exec}}
	entries, cursor, err := l.Snapshot(context.Background(), 10)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(entries) != 1 || entries[0].Level != "INFO" || !strings.Contains(entries[0].Text, "Server started.") {
		t.Fatalf("entries=%q", entries)
	}
	if cursor != "s=abc;i=def" {
		t.Fatalf("cursor=%q", cursor)
	}
	joined := ""
	for _, a := range fj.args {
		joined += a + " "
	}
	if !strings.Contains(joined, "-n 10") || !strings.Contains(joined, "--show-cursor") {
		t.Fatalf("args=%q", joined)
	}
}

func TestLogs_Poll_AfterCursor(t *testing.T) {
	fj := &fakeJournal{stdout: "Aug 19 02:00:00 minecraft sh[1]: [2026-08-19 02:00:00:000 WARN] new line\n" +
		"-- cursor: s=xyz;i=uvw\n"}
	l := &Logs{Journal: &Journal{Unit: "minecraft-bedrock", Exec: fj.exec}}
	entries, cursor, err := l.Poll(context.Background(), "TOK")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(entries) != 1 || entries[0].Level != "WARN" {
		t.Fatalf("entries=%q", entries)
	}
	if cursor != "s=xyz;i=uvw" {
		t.Fatalf("cursor=%q", cursor)
	}
	joined := ""
	for _, a := range fj.args {
		joined += a + " "
	}
	if !strings.Contains(joined, "--after-cursor TOK") {
		t.Fatalf("args=%q", joined)
	}
}

func TestLogs_Range_PassesBounds(t *testing.T) {
	fj := &fakeJournal{stdout: ""}
	l := &Logs{Journal: &Journal{Unit: "minecraft-bedrock", Exec: fj.exec}}
	since := time.Date(2026, 8, 19, 1, 0, 0, 0, time.Local)
	until := time.Date(2026, 8, 19, 3, 0, 0, 0, time.Local)
	if _, err := l.Range(context.Background(), since, until); err != nil {
		t.Fatalf("Range: %v", err)
	}
	joined := ""
	for _, a := range fj.args {
		joined += a + " "
	}
	if !strings.Contains(joined, "--since 2026-08-19 01:00:00") || !strings.Contains(joined, "--until 2026-08-19 03:00:00") {
		t.Fatalf("args=%q", joined)
	}

	fj.args = nil
	if _, err := l.Range(context.Background(), time.Time{}, until); err != nil {
		t.Fatalf("Range open-ended: %v", err)
	}
	joined = ""
	for _, a := range fj.args {
		joined += a + " "
	}
	if strings.Contains(joined, "--since") {
		t.Fatalf("zero since must be omitted: %q", joined)
	}
}
