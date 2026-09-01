package tui

import (
	"context"
	"errors"
	"testing"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// TestCapture_ProvisionWizard is a manual visual capture (run with -v) that
// prints the exact rendered View() of the provision wizard at each stage. It is
// a verification aid for the ready-for-human handoff, not an assertion.
func TestCapture_ProvisionWizard(t *testing.T) {
	capture := func(label string, build func() *Server) {
		t.Logf("\n=== %s ===\n%s", label, build().View())
	}

	// Tmux-mode plan (verbatim tmux commands).
	capture("TMUX PLAN (after wizard completes)", func() *Server {
		stack, _ := newProvRig(t, nil)
		s := NewServer(&fakeServerHost{}, stack)
		provDriveTmuxForm(s, "/srv/bedrock", "/opt/minecraft/server/bedrock-server.zip")
		return s
	})

	// Systemd-mode plan (staged units + handoff commands), non-root.
	capture("SYSTEMD PLAN (non-root, staged units + handoff)", func() *Server {
		fe := &fakeExec{}
		stack := &compass.Stack{Provision: &compass.Provision{
			Env:    &compass.ProvisionEnv{IsRoot: false, CurrentUser: "tony"},
			Exec:   fe.run,
			Verify: func(ctx context.Context, addr string) error { return errors.New("n/a") },
		}}
		s := NewServer(&fakeServerHost{}, stack)
		s.Update(keyMsg('p'))
		provType(s, "compass-prov")
		provType(s, "/srv/bedrock")
		provType(s, "s") // systemd
		provType(s, "z") // zip
		provType(s, "/opt/minecraft/server/bedrock-server.zip")
		provType(s, "tony")
		provType(s, "") // port empty = default 19132
		if s.provIdx != pfComplete || s.provPlan == nil {
			t.Fatalf("wizard did not reach the systemd plan: idx=%d plan=%v", s.provIdx, s.provPlan)
		}
		return s
	})

	// The error state (invalid supervisor input).
	capture("WIZARD ERROR STATE (invalid supervisor)", func() *Server {
		stack, _ := newProvRig(t, nil)
		s := NewServer(&fakeServerHost{}, stack)
		s.Update(keyMsg('p'))
		provType(s, "compass-prov")
		provType(s, "/srv/bedrock")
		s.Update(keyMsg('x')) // invalid supervisor
		s.Update(enterMsg())  // commit -> error
		return s
	})
}
