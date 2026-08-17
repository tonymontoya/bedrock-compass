package compass

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Supervisor stops and starts the server process. The backup engine uses
// it for stop-and-copy backups and for restore; the TUI's lifecycle screen
// (ticket #13) will use the same seam.
//
// The function fields keep the seam testable offline: production wiring
// goes through ExecFunc and the console channel, tests inject fakes.
type Supervisor struct {
	// Poll tunes the process-exit wait loop in tmux Stop. Zero takes the
	// default (250 ms).
	Poll time.Duration

	Stop    func(ctx context.Context) error
	Start   func(ctx context.Context) error
	Running func(ctx context.Context) (bool, error)
}

func (s *Supervisor) poll() time.Duration {
	if s.Poll > 0 {
		return s.Poll
	}
	return 250 * time.Millisecond
}

// NewSystemdSupervisor wires a server under systemd. Stop/Start use
// systemctl as the current user (no sudo, D11 pattern: a permission
// failure surfaces the exact command to the operator).
func NewSystemdSupervisor(unit string, exec ExecFunc) *Supervisor {
	if exec == nil {
		exec = Exec
	}
	return &Supervisor{
		Stop: func(ctx context.Context) error {
			if _, err := exec(ctx, "systemctl", "stop", unit); err != nil {
				return fmt.Errorf("systemctl stop %s: %w (run it yourself if the tool lacks permission)", unit, err)
			}
			return nil
		},
		Start: func(ctx context.Context) error {
			if _, err := exec(ctx, "systemctl", "start", unit); err != nil {
				return fmt.Errorf("systemctl start %s: %w (run it yourself if the tool lacks permission)", unit, err)
			}
			return nil
		},
		Running: func(ctx context.Context) (bool, error) {
			out, _ := exec(ctx, "systemctl", "is-active", unit)
			// is-active exits non-zero when the unit is not active; an
			// inactive unit is a normal state, not an error.
			return strings.TrimSpace(string(out)) == "active", nil
		},
	}
}

// ErrTmuxStartDeferred is returned by the tmux supervisor's Start: starting
// a tmux-hosted server is deferred to ticket #13, and the engine must fail
// loudly rather than guess.
var ErrTmuxStartDeferred = errors.New("starting a tmux-hosted server is not implemented yet (ticket #13); start it manually")

// NewTmuxSupervisor wires a server running in a tmux pane. Stop types the
// console "stop" command and waits for the process to exit; Running falls
// back to a process-table check. Start is deferred (ticket #13).
func NewTmuxSupervisor(console *Console, processAlive func(ctx context.Context) (bool, error)) *Supervisor {
	if processAlive == nil {
		processAlive = ServerProcessAlive
	}
	s := &Supervisor{}
	s.Stop = func(ctx context.Context) error {
		if _, err := console.Send(ctx, "stop"); err != nil {
			return fmt.Errorf("console stop: %w", err)
		}
		for {
			alive, err := processAlive(ctx)
			if err != nil {
				return fmt.Errorf("wait for server exit: %w", err)
			}
			if !alive {
				return nil
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("server did not exit after the stop command: %w", ctx.Err())
			case <-time.After(s.poll()):
			}
		}
	}
	s.Start = func(ctx context.Context) error { return ErrTmuxStartDeferred }
	s.Running = processAlive
	return s
}

// ServerProcessAlive reports whether a bedrock_server process is running on
// this host.
func ServerProcessAlive(ctx context.Context) (bool, error) {
	procs, err := procsFromProc(ctx)
	if err != nil {
		return false, fmt.Errorf("read process table: %w", err)
	}
	for _, p := range procs {
		if p.Name == serverProcName {
			return true, nil
		}
	}
	return false, nil
}
