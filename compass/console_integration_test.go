//go:build integration

package compass

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These tests run against the scratch rig (unit bedrock-compass-test on the
// test host) and self-skip when the rig is not active. They are opt-in:
// built with -tags integration and shipped to the rig host (make integ).

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func rigFIFO() string { return envOr("COMPASS_TEST_FIFO", "/run/bedrock-compass-test.fifo") }
func rigUnit() string { return envOr("COMPASS_TEST_UNIT", "bedrock-compass-test") }

func skipIfRigDown(t *testing.T) {
	t.Helper()
	out, err := exec.Command("systemctl", "is-active", rigUnit()).Output()
	if err != nil || strings.TrimSpace(string(out)) != "active" {
		t.Skipf("scratch rig is not active (unit %s); start it to run these tests", rigUnit())
	}
	settleConsole(t)
}

// settleConsole blocks until the rig's console actually answers a command,
// or the rig is skipped. A freshly started server reports "active" to
// systemd before its console stdin is ready; the first command issued in
// that window returns an empty response and would spuriously fail a
// backup/restore test. Probing with a harmless read-only command until a
// real response arrives makes the suite deterministic regardless of which
// test runs first and of server startup timing.
func settleConsole(t *testing.T) {
	t.Helper()
	c := rigConsole(t)
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		out, err := c.Send(ctx, "list")
		cancel()
		if err == nil && strings.Contains(out, "players online") {
			return // console is live and answering
		}
		if time.Now().After(deadline) {
			t.Skipf("rig is active but the console is not answering within 30s (last: %v / %q); start the server and retry", err, out)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func rigConsole(t *testing.T) *Console {
	t.Helper()
	return NewSystemdConsole(rigFIFO(), rigUnit(),
		ConsolePolicy{Quiesce: 500 * time.Millisecond, Timeout: 15 * time.Second, Poll: 100 * time.Millisecond})
}

func TestIntegration_RoundTrip_Help(t *testing.T) {
	skipIfRigDown(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := rigConsole(t).Send(ctx, "help")
	if err != nil {
		t.Fatalf("Send(help): %v\npartial output:\n%s", err, out)
	}
	for _, want := range []string{"Showing help page 1 of 4", "/deop"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help output missing %q:\n%s", want, out)
		}
	}
}

func TestIntegration_RoundTrip_List(t *testing.T) {
	skipIfRigDown(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := rigConsole(t).Send(ctx, "list")
	if err != nil {
		t.Fatalf("Send(list): %v\npartial output:\n%s", err, out)
	}
	if !strings.Contains(out, "players online") {
		t.Fatalf("list output missing roster line:\n%s", out)
	}
}

func TestIntegration_UnknownCommand_IsReported(t *testing.T) {
	skipIfRigDown(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := rigConsole(t).Send(ctx, "definitely-not-a-command")
	if err != nil {
		t.Fatalf("Send(bogus): %v\npartial output:\n%s", err, out)
	}
	if !strings.Contains(out, "Unknown command") {
		t.Fatalf("expected the server's unknown-command report:\n%s", out)
	}
}

// TestIntegration_NoFIFO_CleanErrorNoHang needs no rig: a console FIFO with
// no attached server must fail fast and clean, never hang.
func TestIntegration_NoFIFO_CleanErrorNoHang(t *testing.T) {
	c := NewSystemdConsole("/run/bedrock-compass-no-such-fifo", rigUnit(),
		ConsolePolicy{Quiesce: 50 * time.Millisecond, Timeout: 500 * time.Millisecond, Poll: 10 * time.Millisecond})
	start := time.Now()
	_, err := c.Send(context.Background(), "help")
	if err == nil {
		t.Fatal("Send against a missing FIFO must fail")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("failure path took %v; must not hang", d)
	}
	t.Logf("clean error: %v", err)
}
