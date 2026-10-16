//go:build integration

package compass

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests exercise the Players service (#6) against the scratch rig and
// self-skip when the rig is not active. The kick test uses a known-bad
// gamertag: Bedrock answers with one of its no-match failure lines, which
// verifies the full send → harvest → classify round-trip without needing a
// real client connected (live-verified on the rig host 2026-08-19). The exact wording
// varies by server version — "No targets matched selector" vs. "Could not
// find player …" — so the test accepts either, mirroring classifyReply.

// noMatchMarkers are the failure lines Bedrock emits when a player command
// matches nobody; the test accepts any of them (see classifyReply).
var noMatchMarkers = []string{
	"No targets matched selector",
	"Could not find player",
}

func hasNoMatchMarker(s string) bool {
	for _, m := range noMatchMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func rigPlayers(t *testing.T) (*Players, *Trail) {
	t.Helper()
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("trail: %v", err)
	}
	return NewPlayers(rigConsole(t), trail, &Journal{Unit: rigUnit()}), trail
}

func TestIntegration_Players_Roster(t *testing.T) {
	skipIfRigDown(t)
	p, _ := rigPlayers(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := p.Roster(ctx)
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	if r.Max <= 0 {
		t.Errorf("roster max = %d, want > 0 (rig world has a player cap)", r.Max)
	}
	if len(r.Online) != r.Count {
		t.Errorf("roster count = %d but parsed %d online rows", r.Count, len(r.Online))
	}
	for _, pl := range r.Online {
		if pl.Name == "" {
			t.Error("online row with empty name")
		}
	}
	t.Logf("roster: %d/%d online", r.Count, r.Max)
}

func TestIntegration_Players_KickUnknown(t *testing.T) {
	skipIfRigDown(t)
	p, trail := rigPlayers(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := p.Kick(ctx, Player{Name: "definitely-not-a-player"})
	if err == nil {
		t.Fatal("kick of an unknown player must fail")
	}
	if !hasNoMatchMarker(err.Error()) {
		t.Errorf("kick error = %q; want one of the server's no-match reports: %v", err, noMatchMarkers)
	}
	// The action is audit-logged with its failure.
	evs, rerr := trail.Recent(5)
	if rerr != nil {
		t.Fatalf("trail recent: %v", rerr)
	}
	found := false
	for _, ev := range evs {
		if ev.Verb == "player" && ev.Params["action"] == "kick" && hasNoMatchMarker(ev.Result) {
			found = true
		}
	}
	if !found {
		t.Errorf("audit trail missing the failed kick event: %+v", evs)
	}
}

func TestIntegration_Players_AllowlistRoundTrip(t *testing.T) {
	skipIfRigDown(t)
	p, _ := rigPlayers(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tag := "compass-integ-tester"

	// Idempotent start: drop a stale entry from an earlier run.
	if out, rerr := rigConsole(t).Send(ctx, "allowlist remove "+tag); rerr != nil {
		t.Logf("pre-clean allowlist remove: %v (%q)", rerr, out)
	}

	detail, err := p.Allow(ctx, Player{Name: tag})
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !strings.Contains(detail, "to the allowlist") {
		t.Errorf("allow detail = %q; want the server's success line", detail)
	}

	// Cleanup is guaranteed even if the assertions below fail.
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer ccancel()
		if out, rerr := rigConsole(t).Send(cctx, "allowlist remove "+tag); rerr != nil {
			t.Logf("cleanup allowlist remove: %v (%q)", rerr, out)
		}
	})

	out, err := rigConsole(t).Send(ctx, "allowlist list")
	if err != nil {
		t.Fatalf("allowlist list: %v", err)
	}
	if !strings.Contains(out, tag) {
		t.Errorf("allowlist list missing the added entry:\\n%s", out)
	}
}

func TestIntegration_Players_Recent(t *testing.T) {
	skipIfRigDown(t)
	p, _ := rigPlayers(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rec, err := p.Recent(ctx)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	for _, rp := range rec {
		if rp.Name == "" {
			t.Error("recent entry with empty name")
		}
	}
	t.Logf("recent players: %d", len(rec))
}
