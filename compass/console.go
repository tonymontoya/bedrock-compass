package compass

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

// ConsolePolicy tunes command harvesting. Zero values take the defaults
// (500 ms quiescence, 15 s timeout, 100 ms poll).
type ConsolePolicy struct {
	Quiesce time.Duration
	Timeout time.Duration
	Poll    time.Duration
}

func (p ConsolePolicy) withDefaults() ConsolePolicy {
	q := p.Quiesce
	if q <= 0 {
		q = 500 * time.Millisecond
	}
	to := p.Timeout
	if to <= 0 {
		to = 15 * time.Second
	}
	poll := p.Poll
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	return ConsolePolicy{Quiesce: q, Timeout: to, Poll: poll}
}

// Console is the command channel to a running Bedrock server (ADR-0001):
// Send writes one command line to the server's stdin and harvests the
// server's output until it settles.
type Console struct {
	Write    func(ctx context.Context, line string) error
	Baseline func(ctx context.Context) (string, error)
	NewSince func(ctx context.Context, token string) ([]string, error)
	Policy   ConsolePolicy
}

// NewSystemdConsole wires the console of a server under systemd
// (ADR-0001): commands go to the unit's console FIFO, output is harvested
// from the journal.
func NewSystemdConsole(fifo, unit string, policy ConsolePolicy) *Console {
	j := &Journal{Unit: unit}
	return &Console{
		Write:    fifoWrite(fifo),
		Baseline: j.Cursor,
		NewSince: j.NewSince,
		Policy:   policy,
	}
}

// NewTmuxConsole wires the console of a server running in a tmux pane:
// commands are typed with send-keys, output is harvested by capture-pane.
func NewTmuxConsole(pane string, policy ConsolePolicy) *Console {
	p := &Pane{Target: pane}
	return &Console{
		Write:    tmuxSend(pane),
		Baseline: p.Baseline,
		NewSince: p.NewSince,
		Policy:   policy,
	}
}

// Send issues one command and returns its output, lines joined with "\n".
// If the output never settles, the error wraps ErrTimeout and the partial
// output captured so far is still returned. A canceled ctx returns the
// partial output with the context error.
func (c *Console) Send(ctx context.Context, command string) (string, error) {
	pol := c.Policy.withDefaults()
	h := &Harvest{Quiesce: pol.Quiesce, Timeout: pol.Timeout}

	token, err := c.Baseline(ctx)
	if err != nil {
		return "", fmt.Errorf("console baseline: %w", err)
	}
	if err := c.Write(ctx, command); err != nil {
		return "", fmt.Errorf("console send %q: %w", command, err)
	}

	var lines []string
	for {
		select {
		case <-ctx.Done():
			return strings.Join(lines, "\n"), fmt.Errorf("console send %q: %w", command, ctx.Err())
		case <-time.After(pol.Poll):
		}
		got, err := c.NewSince(ctx, token)
		if err != nil {
			return strings.Join(lines, "\n"), fmt.Errorf("console read after %q: %w", command, err)
		}
		if len(got) > len(lines) {
			lines = got
		}
		done, herr := h.Feed(Sample{Count: len(lines), At: time.Now()})
		if done {
			out := strings.Join(lines, "\n")
			if herr != nil {
				return out, fmt.Errorf("console send %q: %w", command, herr)
			}
			return out, nil
		}
	}
}

// fifoWrite delivers one line to the console FIFO. A FIFO open for writing
// blocks until a reader attaches, so O_NONBLOCK is used: a server that is
// not running (no reader) fails immediately with ENXIO instead of hanging.
func fifoWrite(fifo string) func(ctx context.Context, line string) error {
	return func(ctx context.Context, line string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Write([]byte(line + "\n")); err != nil {
			return err
		}
		return nil
	}
}

// tmuxSend types the command line literally, then presses Enter.
func tmuxSend(pane string) func(ctx context.Context, line string) error {
	return func(ctx context.Context, line string) error {
		if _, err := Exec(ctx, "tmux", "send-keys", "-t", pane, "-l", "--", line); err != nil {
			return err
		}
		if _, err := Exec(ctx, "tmux", "send-keys", "-t", pane, "Enter"); err != nil {
			return err
		}
		return nil
	}
}
