package compass

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// LogEntry is one parsed journal line for the Logs screen (#8). Text keeps
// the message exactly as captured (the server's own "[ts LEVEL]" tag
// included when present), so rendering stays faithful; Time and Level are
// extracted from that tag for filtering.
type LogEntry struct {
	Time  time.Time // server timestamp, second precision; zero when untagged
	Level string    // "INFO", "WARN", or "ERROR"; "" when the line has no tag
	Text  string    // full message text as captured
}

// logTagRE extracts the server's own "[ts LEVEL]" tag from a log line, e.g.
// "[2026-08-19 01:30:22:503 INFO] Player connected: ...". Unanchored on
// purpose — mirrors playerTagRE in players.go so both parsers accept the
// same input shapes (full journalctl line or bare message).
var logTagRE = regexp.MustCompile(`\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}):(\d{3})\s+(\w+)\]`)

// ParseLogLine parses one journal line or bare message into a LogEntry.
// Lines without the server tag (e.g. help-page continuation lines) yield a
// zero Time and empty Level; Text is always preserved.
func ParseLogLine(line string) LogEntry {
	e := LogEntry{Text: line}
	if m := logTagRE.FindStringSubmatch(line); m != nil {
		if ts, err := time.ParseInLocation("2006-01-02 15:04:05", m[1], time.Local); err == nil {
			e.Time = ts
		}
		e.Level = strings.ToUpper(m[3])
	}
	return e
}

// ParseEntries parses a batch of captured journal lines, preserving order.
func ParseEntries(lines []string) []LogEntry {
	entries := make([]LogEntry, 0, len(lines))
	for _, line := range lines {
		entries = append(entries, ParseLogLine(line))
	}
	return entries
}

// LogFilters narrows a set of log entries. Every empty field means "no
// filter"; all set fields must match (AND composition).
type LogFilters struct {
	Level  string // "", "INFO", "WARN", or "ERROR" (case-insensitive)
	Text   string // case-insensitive substring of the message
	Player string // gamertag: their connect/disconnect lines plus any line mentioning it
	Since  time.Time
	Until  time.Time
}

// FilterEntries returns the entries matching every set filter, in order.
// Untagged entries (zero Time) are dropped whenever a time bound is set —
// they cannot be placed inside a window.
func FilterEntries(entries []LogEntry, f LogFilters) []LogEntry {
	var out []LogEntry
	for _, e := range entries {
		if !entryMatches(e, f) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func entryMatches(e LogEntry, f LogFilters) bool {
	if f.Level != "" && !strings.EqualFold(e.Level, f.Level) {
		return false
	}
	if f.Text != "" && !strings.Contains(strings.ToLower(e.Text), strings.ToLower(f.Text)) {
		return false
	}
	if f.Player != "" && !entryMentionsPlayer(e.Text, f.Player) {
		return false
	}
	if !f.Since.IsZero() && (e.Time.IsZero() || e.Time.Before(f.Since)) {
		return false
	}
	if !f.Until.IsZero() && (e.Time.IsZero() || e.Time.After(f.Until)) {
		return false
	}
	return true
}

// entryMentionsPlayer reports whether a line belongs to the given gamertag:
// an exact connect/disconnect match via the shared parser, or any other line
// that mentions the name (case-insensitive substring).
func entryMentionsPlayer(line, player string) bool {
	if name, _, _, ok := ParsePlayerLine(line); ok && strings.EqualFold(name, player) {
		return true
	}
	return strings.Contains(strings.ToLower(line), strings.ToLower(player))
}

// Logs reads and filters server log entries for the Logs screen (#8).
// Reader-only: it never writes to or rotates logs.
type Logs struct {
	Journal *Journal
}

// Snapshot loads up to n most recent entries (oldest first) plus a cursor
// positioned after them, so the caller can poll Poll for new lines. One
// journal read gets both the scrollback and the tail position.
func (l *Logs) Snapshot(ctx context.Context, n int) ([]LogEntry, string, error) {
	lines, cursor, err := l.Journal.Tail(ctx, n)
	if err != nil {
		return nil, "", err
	}
	return ParseEntries(lines), cursor, nil
}

// Poll returns the entries added after token (all entries when empty) plus
// a cursor positioned after them for the next poll. Cheap: only new journal
// entries are read per call.
func (l *Logs) Poll(ctx context.Context, token string) ([]LogEntry, string, error) {
	lines, cursor, err := l.Journal.Since(ctx, token)
	if err != nil {
		return nil, "", err
	}
	return ParseEntries(lines), cursor, nil
}

// Range returns the entries in [since, until] for historical queries. Empty
// bounds are open-ended; times use local wall clock, matching journalctl's
// --since/--until interpretation.
func (l *Logs) Range(ctx context.Context, since, until time.Time) ([]LogEntry, error) {
	lines, err := l.Journal.Range(ctx, since, until)
	if err != nil {
		return nil, err
	}
	return ParseEntries(lines), nil
}
