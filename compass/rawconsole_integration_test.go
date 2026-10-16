//go:build integration

package compass

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests exercise the RawConsole service (#12) against the scratch rig's
// real console FIFO + journal. They self-skip when the rig is not active and
// are opt-in: built with -tags integration and shipped to the rig host
// (make integ RIG_HOST=<rig host>). Only harmless, read-only commands are sent — the
// raw surface is being verified for round-trip fidelity and audit logging, not
// for mutating the server.

func TestIntegration_RawConsole_VerbatimRoundTripAndAudit(t *testing.T) {
	skipIfRigDown(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	rc := NewRawConsole(rigConsole(t), trail)

	// A harmless read-only command. The raw surface must deliver it verbatim
	// and return the server's output unchanged.
	out, err := rc.Send(ctx, "list")
	if err != nil {
		t.Fatalf("Send(list): %v\npartial output:\n%s", err, out)
	}
	if !strings.Contains(out, "players online") {
		t.Fatalf("raw console list output missing roster line:\n%s", out)
	}

	// The audit trail must record the raw command (D11): verb "console", the
	// exact command as param, and an ok result.
	evs, err := trail.Recent(50)
	if err != nil {
		t.Fatalf("trail.Recent: %v", err)
	}
	found := false
	for _, ev := range evs {
		if ev.Verb == "console" && ev.Params["command"] == "list" && ev.Result == "ok" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit trail missing a console event for %q; got %+v", "list", evs)
	}
}

// TestIntegration_RawConsole_FailureIsAudited sends an unknown command (which
// the server reports, not errors at the channel level) and confirms the raw
// surface still round-trips it. A true channel failure is covered by the unit
// tests; here we verify a real server response flows through verbatim.
func TestIntegration_RawConsole_UnknownCommandRoundTrips(t *testing.T) {
	skipIfRigDown(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	rc := NewRawConsole(rigConsole(t), trail)

	out, err := rc.Send(ctx, "definitely-not-a-command")
	if err != nil {
		t.Fatalf("Send(bogus): %v\npartial output:\n%s", err, out)
	}
	if !strings.Contains(out, "Unknown command") {
		t.Fatalf("expected the server's unknown-command report verbatim:\n%s", out)
	}

	evs, _ := trail.Recent(50)
	found := false
	for _, ev := range evs {
		if ev.Verb == "console" && ev.Params["command"] == "definitely-not-a-command" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit trail missing a console event for the unknown command; got %+v", evs)
	}
}
