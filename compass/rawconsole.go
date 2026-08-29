// Package compass: the raw console surface (ticket #12). It is the single
// execution path for verbatim server commands: one Console.Send round-trip,
// audit-logged through the Trail (D11) with verb "console" and the command
// itself as a param. The TUI's Console screen is a thin renderer over it;
// nothing else knows about the other.
//
// Raw mode is deliberately the no-hands-off surface: the confirmation tiers
// do not apply here (documented in ADR-0009), because every command typed at
// this surface is already an explicit, deliberate act by a human who can see
// exactly what they are typing. What raw mode does NOT waive is the audit
// trail — every raw command lands in it regardless of outcome.
package compass

import (
	"context"
	"errors"
)

// RawConsole sends verbatim commands to the server's console and records
// each one in the audit trail. Console may be nil (a deployment with no
// resolved channel) — Send then fails cleanly instead of panicking, and the
// attempt is still audited so a misconfigured surface leaves a trace.
type RawConsole struct {
	Console *Console
	Trail   *Trail
}

// NewRawConsole wires the service over already-resolved seams (the Stack's).
func NewRawConsole(console *Console, trail *Trail) *RawConsole {
	return &RawConsole{Console: console, Trail: trail}
}

// Send issues one command verbatim and returns the harvested output. The
// command is recorded in the audit trail with verb "console" whether it
// succeeds or fails (D11). A nil console channel yields a clean error that
// is audited too.
func (r *RawConsole) Send(ctx context.Context, command string) (string, error) {
	if r.Console == nil {
		err := errors.New("no console channel resolved")
		r.audit(command, err)
		return "", err
	}
	out, err := r.Console.Send(ctx, command)
	r.audit(command, err)
	return out, err
}

// audit appends one event per raw command (D11: every raw command is
// recorded). A nil trail (audit disabled) is a no-op. The command itself is
// the param — that is what makes the trail an operator-grade record of what
// was actually typed at the server.
func (r *RawConsole) audit(command string, err error) {
	if r.Trail == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error: " + err.Error()
	}
	_ = r.Trail.Append(Event{Verb: "console", Params: map[string]string{"command": command}, Result: result})
}
