//go:build integration

package compass

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests exercise the Access service (#7) against the scratch rig and
// self-skip when the rig is not active. They verify the full round-trip the
// ticket demands: allowlist add/remove/reload written directly to
// allowlist.json (atomic temp+rename), reloaded into the running server, and
// verified against the server's own `allowlist list`; the gate toggle flipping
// both the durable (server.properties) and runtime (console) layers; and the
// per-IP firewall block degrading gracefully to an exact command when we lack
// root. Every test restores the rig's original state so it stays clean for the
// other suites.

func rigAccess(t *testing.T, trail *Trail) (*Access, string) {
	t.Helper()
	dir := rigServerDir()
	cfg := &Config{ServerDir: dir, Port: rigPort(t)} // real port so firewall cmds target it
	a := NewAccess(cfg, rigConsole(t), trail)
	return a, dir
}

// originalGate reads the durable gate value so a test can restore it.
func originalGate(t *testing.T, dir string) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "server.properties"))
	if err != nil {
		t.Fatalf("read server.properties: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		v, ok := cutKey(strings.TrimSpace(line), "allow-list=")
		if ok {
			return strings.EqualFold(strings.TrimSpace(v), "true")
		}
	}
	return false // key absent = gate off (BDS default)
}

func TestIntegration_Access_GateToggle(t *testing.T) {
	skipIfRigDown(t)
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("trail: %v", err)
	}
	a, dir := rigAccess(t, trail)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	orig := originalGate(t, dir)
	// Restore both layers (file + runtime) so the rig is left as found.
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		if _, werr := a.SetGate(cctx, orig); werr != nil {
			t.Logf("cleanup gate restore: %v", werr)
		}
	})

	target := !orig // flip to the opposite of whatever it is now
	detail, err := a.SetGate(ctx, target)
	if err != nil {
		t.Fatalf("SetGate(%v): %v", target, err)
	}
	t.Logf("gate detail: %s", detail)

	got, gerr := a.Gate()
	if gerr != nil {
		t.Fatalf("Gate read-back: %v", gerr)
	}
	if got.Enabled != target {
		t.Errorf("durable gate = %v, want %v (server.properties must reflect the toggle)", got.Enabled, target)
	}

	// The runtime layer must have been applied live too (the console answered).
	out, serr := rigConsole(t).Send(ctx, "allowlist list")
	if serr != nil {
		t.Fatalf("console not answering after gate toggle: %v", serr)
	}
	_ = out

	evs, rerr := trail.Recent(5)
	if rerr != nil {
		t.Fatalf("trail recent: %v", rerr)
	}
	found := false
	for _, ev := range evs {
		if ev.Verb == "access" && ev.Params["action"] == "gate" && ev.Result == "ok" {
			found = true
		}
	}
	if !found {
		t.Errorf("audit trail missing the gate event: %+v", evs)
	}
}

func TestIntegration_Access_AllowlistRoundTrip(t *testing.T) {
	skipIfRigDown(t)
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("trail: %v", err)
	}
	a, dir := rigAccess(t, trail)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	tag := "compass-access-integ"
	// Idempotent start: drop a stale entry from an earlier run.
	if out, rerr := rigConsole(t).Send(ctx, "allowlist remove "+tag); rerr != nil {
		t.Logf("pre-clean allowlist remove: %v (%q)", rerr, out)
	}

	detail, err := a.Add(ctx, tag)
	if err != nil {
		t.Fatalf("Add(%s): %v", tag, err)
	}
	t.Logf("add detail: %s", detail)

	// The file must hold the entry in BDS's exact format.
	data, rerr := os.ReadFile(filepath.Join(dir, "allowlist.json"))
	if rerr != nil {
		t.Fatalf("read allowlist.json after add: %v", rerr)
	}
	if !strings.Contains(string(data), `"name":"`+tag+`"`) {
		t.Errorf("allowlist.json missing the added entry:\n%s", string(data))
	}

	// The running server must agree (verified via its own `allowlist list`).
	out, lerr := rigConsole(t).Send(ctx, "allowlist list")
	if lerr != nil {
		t.Fatalf("allowlist list: %v", lerr)
	}
	if !strings.Contains(out, tag) {
		t.Errorf("server's allowlist list missing the added entry:\n%s", out)
	}

	// Remove it and verify it is gone from both file and server.
	detail2, err := a.Remove(ctx, tag)
	if err != nil {
		t.Fatalf("Remove(%s): %v", tag, err)
	}
	t.Logf("remove detail: %s", detail2)

	data2, rerr := os.ReadFile(filepath.Join(dir, "allowlist.json"))
	if rerr != nil {
		t.Fatalf("read allowlist.json after remove: %v", rerr)
	}
	if strings.Contains(string(data2), tag) {
		t.Errorf("allowlist.json still holds the removed entry:\n%s", string(data2))
	}

	out2, lerr := rigConsole(t).Send(ctx, "allowlist list")
	if lerr != nil {
		t.Fatalf("allowlist list (after remove): %v", lerr)
	}
	if strings.Contains(out2, tag) {
		t.Errorf("server's allowlist list still lists the removed entry:\n%s", out2)
	}

	evs, terr := trail.Recent(10)
	if terr != nil {
		t.Fatalf("trail recent: %v", terr)
	}
	var sawAdd, sawRemove bool
	for _, ev := range evs {
		switch ev.Params["action"] {
		case "allow-add":
			sawAdd = true
		case "allow-remove":
			sawRemove = true
		}
	}
	if !sawAdd || !sawRemove {
		t.Errorf("audit trail missing add/remove events (add=%v remove=%v): %+v", sawAdd, sawRemove, evs)
	}
}

func TestIntegration_Access_Reload(t *testing.T) {
	skipIfRigDown(t)
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("trail: %v", err)
	}
	a, _ := rigAccess(t, trail)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	detail, err := a.Reload(ctx)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	t.Logf("reload detail: %s", detail)
	if !strings.Contains(detail, "reloaded") && !strings.Contains(detail, "applies on next start") {
		t.Errorf("reload detail = %q; want a reload confirmation", detail)
	}
}

func TestIntegration_Access_FirewallDegrades(t *testing.T) {
	skipIfRigDown(t)
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("trail: %v", err)
	}
	a, _ := rigAccess(t, trail)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ip := "203.0.113.9" // TEST-NET-3, never a real host
	port := rigPort(t)
	res, err := a.BlockIP(ctx, ip)
	if err != nil {
		t.Fatalf("BlockIP: %v", err)
	}
	wantCmd := fmt.Sprintf("ufw deny from %s to any port %d proto udp", ip, port)
	if res.Command != wantCmd {
		t.Errorf("block command = %q, want %q", res.Command, wantCmd)
	}
	// On the rig we run unprivileged (no sudo), so it must degrade: not applied,
	// with a reason and the exact command surfaced.
	if os.Geteuid() != 0 {
		if res.Applied {
			t.Errorf("block should NOT be applied when unprivileged")
		}
		if res.Reason == "" {
			t.Errorf("degraded block must carry a reason, got empty")
		}
	}

	up, err := a.UnblockIP(ctx, ip)
	if err != nil {
		t.Fatalf("UnblockIP: %v", err)
	}
	wantUp := fmt.Sprintf("ufw delete deny from %s to any port %d proto udp", ip, port)
	if up.Command != wantUp {
		t.Errorf("unblock command = %q, want %q", up.Command, wantUp)
	}

	// An invalid IP is rejected before any command is built.
	if _, err := a.BlockIP(ctx, "999.1.1.1"); err == nil {
		t.Error("BlockIP of an invalid address must fail")
	}
}
