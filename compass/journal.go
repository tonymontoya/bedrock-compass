package compass

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ExecFunc runs a command and returns its stdout. Injectable so the
// journalctl/tmux wiring is testable with PATH shims.
type ExecFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// Exec is the production ExecFunc.
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Journal reads a systemd unit's log entries via journalctl.
type Journal struct {
	Unit string // journal unit filter, e.g. "minecraft-bedrock"
	Exec ExecFunc
}

// Cursor returns a position token at the unit's last journal entry.
// A unit with no entries yet yields ("", nil); NewSince with an empty
// token reads everything.
func (j *Journal) Cursor(ctx context.Context) (string, error) {
	out, err := j.exec()(ctx, "journalctl", "-u", j.Unit, "--show-cursor", "-n", "1", "--no-pager")
	text := strings.TrimSpace(string(out))
	if strings.Contains(text, "No entries") {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("journal cursor for unit %q: %w", j.Unit, err)
	}
	lines := strings.Split(text, "\n")
	last := lines[len(lines)-1]
	if i := strings.Index(last, "cursor: "); i >= 0 {
		return strings.TrimSpace(last[i+len("cursor: "):]), nil
	}
	return "", fmt.Errorf("journal cursor for unit %q: no cursor line in output %q", j.Unit, text)
}

// NewSince returns the message part of each unit entry added after token
// (all entries when token is empty), in journal order.
func (j *Journal) NewSince(ctx context.Context, token string) ([]string, error) {
	args := []string{"-u", j.Unit, "--no-pager"}
	if token != "" {
		args = append(args, "--after-cursor", token)
	}
	out, err := j.exec()(ctx, "journalctl", args...)
	text := strings.TrimSpace(string(out))
	if strings.Contains(text, "No entries") {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("journal read for unit %q: %w", j.Unit, err)
	}
	var lines []string
	for _, raw := range strings.Split(text, "\n") {
		if msg, ok := journalLine(raw); ok {
			lines = append(lines, msg)
		}
	}
	return lines, nil
}

// RecentLines returns the message part of the unit's last n entries, in
// journal order (oldest first). Bounded work per call — this is what makes a
// periodic "recently connected" mine (ticket #6) cheap on a long-running
// server: only the tail of the journal is read.
func (j *Journal) RecentLines(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		return nil, fmt.Errorf("journal recent for unit %q: n must be positive", j.Unit)
	}
	out, err := j.exec()(ctx, "journalctl", "-u", j.Unit, "--no-pager", "-n", strconv.Itoa(n))
	text := strings.TrimSpace(string(out))
	if strings.Contains(text, "No entries") {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("journal read for unit %q: %w", j.Unit, err)
	}
	var lines []string
	for _, raw := range strings.Split(text, "\n") {
		if msg, ok := journalLine(raw); ok {
			lines = append(lines, msg)
		}
	}
	return lines, nil
}

// Tail returns the message part of the unit's last n entries in journal
// order (oldest first) plus a cursor positioned after them, so a caller can
// poll for new lines with NewSince. This is the bounded initial load behind
// the Logs screen (#8): one read gets both the scrollback and the tail
// position.
func (j *Journal) Tail(ctx context.Context, n int) ([]string, string, error) {
	if n <= 0 {
		return nil, "", fmt.Errorf("journal tail for unit %q: n must be positive", j.Unit)
	}
	out, err := j.exec()(ctx, "journalctl", "-u", j.Unit, "--no-pager", "-n", strconv.Itoa(n), "--show-cursor")
	text := strings.TrimSpace(string(out))
	if strings.Contains(text, "No entries") {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("journal read for unit %q: %w", j.Unit, err)
	}
	var lines []string
	cursor := ""
	for _, raw := range strings.Split(text, "\n") {
		if i := strings.Index(raw, "-- cursor: "); i >= 0 {
			cursor = strings.TrimSpace(raw[i+len("-- cursor: "):])
			continue
		}
		if msg, ok := journalLine(raw); ok {
			lines = append(lines, msg)
		}
	}
	return lines, cursor, nil
}

// Since returns the message part of each unit entry added after token (all
// entries when token is empty), in journal order, plus a cursor positioned
// after them for the next poll. It is the live-tail half of the Logs screen
// (#8): cheap because only new entries are read per call.
func (j *Journal) Since(ctx context.Context, token string) ([]string, string, error) {
	args := []string{"-u", j.Unit, "--no-pager", "--show-cursor"}
	if token != "" {
		args = append(args, "--after-cursor", token)
	}
	out, err := j.exec()(ctx, "journalctl", args...)
	text := strings.TrimSpace(string(out))
	if strings.Contains(text, "No entries") {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("journal read for unit %q: %w", j.Unit, err)
	}
	var lines []string
	cursor := ""
	for _, raw := range strings.Split(text, "\n") {
		if i := strings.Index(raw, "-- cursor: "); i >= 0 {
			cursor = strings.TrimSpace(raw[i+len("-- cursor: "):])
			continue
		}
		if msg, ok := journalLine(raw); ok {
			lines = append(lines, msg)
		}
	}
	return lines, cursor, nil
}

// Range returns the message part of each unit entry in [since, until], in
// journal order. Empty bounds are omitted (open-ended). Times are local wall
// clock — the same interpretation journalctl applies to --since/--until — so
// a "today" query built from time.Now() lands where the operator expects it.
func (j *Journal) Range(ctx context.Context, since, until time.Time) ([]string, error) {
	args := []string{"-u", j.Unit, "--no-pager"}
	if !since.IsZero() {
		args = append(args, "--since", since.Format("2006-01-02 15:04:05"))
	}
	if !until.IsZero() {
		args = append(args, "--until", until.Format("2006-01-02 15:04:05"))
	}
	out, err := j.exec()(ctx, "journalctl", args...)
	text := strings.TrimSpace(string(out))
	if strings.Contains(text, "No entries") {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("journal read for unit %q: %w", j.Unit, err)
	}
	var lines []string
	for _, raw := range strings.Split(text, "\n") {
		if msg, ok := journalLine(raw); ok {
			lines = append(lines, msg)
		}
	}
	return lines, nil
}

// journalLine extracts the message from one journalctl plain-output line:
//
//	Aug 16 02:21:29 minecraft sh[74883]: [2026-08-16 02:21:29:146 INFO] Server started.
//	    -> [2026-08-16 02:21:29:146 INFO] Server started.
//
// The boundary is the first "]: ": the server's own "[ts LEVEL]" tag ends
// with "] " (no colon), so only the syslog identifier "name[pid]:" can
// produce "]: " (verified against live 1.26.43 captures).
func journalLine(line string) (string, bool) {
	i := strings.Index(line, "]: ")
	if i < 0 {
		return "", false
	}
	return line[i+len("]: "):], true
}

func (j *Journal) exec() ExecFunc {
	if j.Exec != nil {
		return j.Exec
	}
	return Exec
}
