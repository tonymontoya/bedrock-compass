package compass

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
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
