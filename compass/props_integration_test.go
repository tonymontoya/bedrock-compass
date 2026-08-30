//go:build integration

package compass

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests drive the Props service (#11) against the scratch rig and
// self-skip when the rig is not active. The hot max-players round-trip (the
// ticket's acceptance example) verifies the full chain: validate, atomic
// write to server.properties, live apply via setmaxplayers — and, most
// importantly, that the running server actually reports the new value on its
// own pong endpoint. The cold test verifies the opposite: the file holds the
// queued value while the running server is unchanged until a restart. Every
// test restores the rig's original state (and never leaves a key the
// service cannot remove).

func rigProps(t *testing.T) (*Props, *Trail) {
	t.Helper()
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("trail: %v", err)
	}
	cfg := &Config{
		ServerDir:  rigServerDir(),
		Supervisor: "systemd",
		Service:    rigUnit(),
		Port:       rigPort(t),
		ConsoleFif: rigFIFO(),
	}
	sup := NewSystemdSupervisor(rigUnit(), nil)
	lc := NewLifecycle(cfg, sup, rigConsole(t), trail)
	return NewProps(cfg, rigConsole(t), trail, lc), trail
}

// propsValue reads key's current value from the rig's server.properties
// ("" when the key is absent).
func propsValue(t *testing.T, dir, key string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "server.properties"))
	if err != nil {
		t.Fatalf("read server.properties: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// pongProbe returns the rig's live advertisement (or fails).
func pongProbe(t *testing.T, ctx context.Context) Pong {
	t.Helper()
	ping, err := NewPinger("127.0.0.1:" + strconv.Itoa(rigPort(t))).Ping(ctx)
	if err != nil {
		t.Fatalf("pong probe: %v", err)
	}
	return ping
}

func TestIntegration_Props_HotMaxPlayersRoundTrip(t *testing.T) {
	skipIfRigDown(t)
	p, tr := rigProps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dir := rigServerDir()
	orig := propsValue(t, dir, "max-players")
	if orig == "" {
		t.Skip("rig has no max-players key; skipping to avoid leaving one behind")
	}
	if _, err := strconv.Atoi(orig); err != nil {
		t.Fatalf("original max-players = %q is not a number; refusing to risk a broken restore", orig)
	}
	// Restore the rig's own value (file + live) so it stays clean.
	t.Cleanup(func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rcancel()
		if _, werr := p.Set(rctx, "max-players", orig); werr != nil {
			t.Logf("cleanup restore: %v", werr)
		}
	})

	target := 42 // a value the rig almost certainly does not have
	if orig == strconv.Itoa(target) {
		target = 41
	}
	detail, err := p.Set(ctx, "max-players", strconv.Itoa(target))
	if err != nil {
		t.Fatalf("Set(max-players, %d): %v", target, err)
	}
	if !strings.Contains(detail, "applied live") {
		t.Errorf("detail = %q, want a live-apply note", detail)
	}

	// The file must hold the new value.
	if got := propsValue(t, dir, "max-players"); got != strconv.Itoa(target) {
		t.Errorf("server.properties max-players = %q, want %d", got, target)
	}

	// The running server must report it on its own pong endpoint (the
	// acceptance proof that the hot apply took effect, not just the file).
	if got := pongProbe(t, ctx).MaxPlayers; got != target {
		t.Errorf("running server MaxPlayers = %d, want %d (live apply did not take effect)", got, target)
	}

	// Audit trail: verb config with the after value.
	evs, err := tr.Recent(10)
	if err != nil {
		t.Fatalf("trail recent: %v", err)
	}
	found := false
	for _, ev := range evs {
		if ev.Verb == "config" && ev.Params["key"] == "max-players" &&
			ev.Params["after"] == strconv.Itoa(target) && ev.Result == "ok" {
			found = true
		}
	}
	if !found {
		t.Errorf("audit trail missing the config event: %+v", evs)
	}
}

func TestIntegration_Props_ColdQueuedNotLive(t *testing.T) {
	// server-name is a cold key the rig definitely carries (it names the
	// scratch server) and it shows up in the pong as the MOTD — so we can
	// prove the cold semantics both ways: queued in the file, absent from
	// the running server until a restart.
	skipIfRigDown(t)
	p, _ := rigProps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dir := rigServerDir()
	orig := propsValue(t, dir, "server-name")
	if orig == "" {
		t.Skip("rig has no server-name key; skipping to avoid leaving one behind")
	}
	t.Cleanup(func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rcancel()
		if _, werr := p.Set(rctx, "server-name", orig); werr != nil {
			t.Logf("cleanup restore: %v", werr)
		}
	})

	tag := "compass-cold-queued"
	detail, err := p.Set(ctx, "server-name", tag)
	if err != nil {
		t.Fatalf("Set(server-name, %s): %v", tag, err)
	}
	if want := "queued — applies on restart"; detail != want {
		t.Errorf("detail = %q, want %q (cold keys queue, they do not apply live)", detail, want)
	}
	if got := propsValue(t, dir, "server-name"); got != tag {
		t.Errorf("server.properties server-name = %q, want %q (queued)", got, tag)
	}

	// And the running server must NOT have changed: a cold key has no live
	// effect (BDS reads server.properties at startup only).
	if got := pongProbe(t, ctx).MOTD; got == tag {
		t.Errorf("running server already reports %q — cold keys must not apply live", tag)
	}
}

func TestIntegration_Props_ColdAppliedOnRestart(t *testing.T) {
	skipIfRigDown(t)
	p, _ := rigProps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dir := rigServerDir()
	orig := propsValue(t, dir, "server-name")
	if orig == "" {
		t.Skip("rig has no server-name key; skipping to avoid leaving one behind")
	}
	// Never leave the rig stopped if we fail mid-test.
	defer func() {
		if out, err := execOut(context.Background(), "systemctl", "is-active", rigUnit()); err == nil &&
			strings.TrimSpace(string(out)) != "active" {
			t.Logf("rig left not-active; starting it back")
			_, _ = execOut(context.Background(), "systemctl", "start", rigUnit())
		}
	}()
	// Restore the original motd and apply it (one more restart) so the rig
	// is left as found.
	t.Cleanup(func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer rcancel()
		if _, werr := p.Set(rctx, "server-name", orig); werr != nil {
			t.Logf("cleanup restore: %v", werr)
			return
		}
		if rerr := p.Restart(rctx); rerr != nil {
			t.Logf("cleanup restart: %v", rerr)
			return
		}
		settleConsole(t)
	})

	tag := "compass-cold-restart"
	if _, err := p.Set(ctx, "server-name", tag); err != nil {
		t.Fatalf("Set(server-name, %s): %v", tag, err)
	}
	if err := p.Restart(ctx); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	settleConsole(t)

	// After the restart the running server reports the queued value — the
	// acceptance proof that the cold change took effect at the restart tier.
	if got := pongProbe(t, ctx).MOTD; got != tag {
		t.Errorf("running server name after restart = %q, want %q", got, tag)
	}
}
