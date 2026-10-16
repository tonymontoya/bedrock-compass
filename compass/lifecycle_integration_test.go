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

// These tests drive the lifecycle service (ticket #13) against the scratch
// rig: real systemd unit, real console FIFO, real pong endpoint. They
// self-skip when the rig is not active and are opt-in: -tags integration,
// shipped via make integ RIG_HOST=<target>.
//
// The adopt test is different: it adopts a deployment the tool does not yet
// know (the operator's own case — a running server, verified via pong + a
// console round-trip before it is claimed). It self-skips when the host
// cannot satisfy its preconditions — most notably more than one running
// bedrock_server, since v1 discovery is single-server and the operator
// cannot stop the extras from the tool's unprivileged seat. The Adopt
// logic itself (detect → verify → persist, including the pong gate and
// the console roster gate) is fully covered offline
// (compass/lifecycle_test.go).

// rigPort reads the scratch server's UDP port from server.properties
// (default 19132). The pong probe needs it.
func rigPort(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(rigServerDir(), "server.properties"))
	if err != nil {
		return 19132
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "server-port="); ok {
			if p, err := strconv.Atoi(v); err == nil && p > 0 {
				return p
			}
		}
	}
	return 19132
}

// rigLifecycle wires the Lifecycle service over the scratch rig's real
// supervisor and console, with a fresh audit trail.
func rigLifecycle(t *testing.T) (*Lifecycle, *Trail) {
	t.Helper()
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		ServerDir:  rigServerDir(),
		Supervisor: "systemd",
		Service:    rigUnit(),
		Port:       rigPort(t),
		ConsoleFif: rigFIFO(),
	}
	sup := NewSystemdSupervisor(rigUnit(), nil)
	console := rigConsole(t)
	return NewLifecycle(cfg, sup, console, trail), trail
}

func auditActions(t *testing.T, tr *Trail) []string {
	t.Helper()
	evs, err := tr.Recent(50)
	if err != nil {
		t.Fatalf("trail Recent: %v", err)
	}
	var actions []string
	for _, e := range evs {
		if e.Verb == "lifecycle" {
			actions = append(actions, e.Params["action"]+"="+e.Result)
		}
	}
	return actions
}

// TestIntegration_Lifecycle_Status reflects the supervisor's truth: the
// scratch rig active ⇒ running + main pid + uptime. The pong probe is an
// optional enrichment — the documented contract (lifecycle.go) degrades to
// the supervisor truth alone when the server does not answer it — so a
// missing pong must NOT fail this test, only a wrong supervisor truth does.
func TestIntegration_Lifecycle_Status(t *testing.T) {
	skipIfRigDown(t)
	lc, _ := rigLifecycle(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st := lc.Status(ctx)
	if st.State != "running" {
		t.Fatalf("State = %q, want running (rig is active)", st.State)
	}
	if st.Source != "systemd" {
		t.Errorf("Source = %q, want systemd", st.Source)
	}
	if st.PID <= 0 {
		t.Errorf("PID = %d, want the rig's main pid", st.PID)
	}
	if st.Uptime <= 0 {
		t.Errorf("Uptime = %v, want > 0", st.Uptime)
	}
	// Pong is best-effort: if the server answers it, the port must be
	// consistent; if it does not (documented degrade), the supervisor
	// truth above is what this test is verifying.
	if st.Pong != nil && st.Pong.GamePort != rigPort(t) {
		t.Errorf("pong GamePort = %d, want %d", st.Pong.GamePort, rigPort(t))
	} else {
		t.Logf("pong: %v (optional enrichment; absent is the documented degrade)", st.Pong)
	}
}

// TestIntegration_Lifecycle_StartStopRestart runs the full mutation cycle
// through the Lifecycle service (the same code path the TUI's Server
// screen and the headless verbs use) and verifies each step's effect on
// the real unit, plus the audit trail.
func TestIntegration_Lifecycle_StartStopRestart(t *testing.T) {
	skipIfRigDown(t)
	lc, tr := rigLifecycle(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	// Never leave the rig stopped if we fail mid-test.
	defer func() {
		if out, err := execOut(context.Background(), "systemctl", "is-active", rigUnit()); err == nil &&
			strings.TrimSpace(string(out)) != "active" {
			t.Logf("rig left not-active; starting it back")
			_, _ = execOut(context.Background(), "systemctl", "start", rigUnit())
		}
	}()

	// stop → unit inactive
	if err := lc.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if out, _ := execOut(ctx, "systemctl", "is-active", rigUnit()); strings.TrimSpace(string(out)) == "active" {
		t.Fatalf("unit still active after Stop")
	}
	st := lc.Status(ctx)
	if st.State != "stopped" {
		t.Errorf("after Stop, Status.State = %q, want stopped", st.State)
	}

	// start → unit active, console answering
	if err := lc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	settleConsole(t)
	if out, _ := execOut(ctx, "systemctl", "is-active", rigUnit()); strings.TrimSpace(string(out)) != "active" {
		t.Fatalf("unit not active after Start")
	}
	st = lc.Status(ctx)
	if st.State != "running" || st.PID <= 0 {
		t.Errorf("after Start, Status = %+v, want running with a pid", st)
	}

	// restart → active again, single audited action
	if err := lc.Restart(ctx); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	settleConsole(t)
	if out, _ := execOut(ctx, "systemctl", "is-active", rigUnit()); strings.TrimSpace(string(out)) != "active" {
		t.Fatalf("unit not active after Restart")
	}

	// Every action audit-logged (acceptance: every lifecycle action is
	// audit-logged).
	actions := auditActions(t, tr)
	for _, want := range []string{"stop=ok", "start=ok", "restart=ok"} {
		found := false
		for _, a := range actions {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Errorf("audit trail %v missing %q", actions, want)
		}
	}
}

// countBedrockServers returns how many bedrock_server processes are running
// on this host, without reading any per-process details (so it works even
// when some are owned by other users).
func countBedrockServers(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		if strings.TrimSpace(string(mustRead(t, filepath.Join("/proc", e.Name(), "comm")))) == serverProcName {
			n++
		}
	}
	return n
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

// TestIntegration_Lifecycle_Adopt is the operator's own case: point the
// tool at a running deployment it does not yet know, verify it via pong +
// a console round-trip over the deployment's own channel, and persist the
// association to a config.
//
// Preconditions (skipped, with the reason, when unmet on this host):
//   - a deployment to adopt must be active, and
//   - exactly one bedrock_server must be running (v1 discovery is
//     single-server; the operator cannot stop the extras from the tool's
//     unprivileged seat on a shared host).
//
// On a host where more than one server runs and the operator cannot stop
// the extras, the live round-trip is impossible from the tool's seat, so
// the test skips; the Adopt logic itself (including the pong + console
// verify gates and the persist round-trip) is exercised offline.
func TestIntegration_Lifecycle_Adopt(t *testing.T) {
	if countBedrockServers(t) > 1 {
		t.Skipf("%d bedrock_server processes are running; v1 adoption is single-server and the extra cannot be stopped from this user — the live adopt round-trip is impossible on this host (Adopt logic is covered offline)", countBedrockServers(t))
	}
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Port: rigPort(t)} // the tool does not know this deployment yet
	lc := NewLifecycle(cfg, nil, nil, trail)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	a, err := lc.Adopt(ctx, 0)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	// detect: process → dir → unit
	if a.Discovery.PID <= 0 {
		t.Errorf("Discovery.PID = %d, want the adopted server pid", a.Discovery.PID)
	}
	if a.ServerDir == "" {
		t.Error("ServerDir is empty, want the adopted server dir")
	}

	// verify: pong (mandatory) + a real roster line from the console
	// round-trip (mandatory)
	if a.Pong.GamePort != a.Port {
		t.Errorf("pong GamePort = %d, want %d", a.Pong.GamePort, a.Port)
	}
	if !containsPlayersLine(a.Console) {
		t.Errorf("Console = %q, want a roster line from the verification round-trip", a.Console)
	}

	// persist the association, then reload it
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := lc.PersistAdopt(path, a); err != nil {
		t.Fatalf("PersistAdopt: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after persist: %v", err)
	}
	if loaded.ServerDir != a.ServerDir {
		t.Errorf("persisted config dir = %q, want %q", loaded.ServerDir, a.ServerDir)
	}
	if loaded.Port != a.Port {
		t.Errorf("persisted Port = %d, want %d", loaded.Port, a.Port)
	}
	t.Logf("adopted: %s %s dir=%s port=%d fifo=%s", a.Supervisor, a.Service, a.ServerDir, a.Port, a.ConsoleFif)
}

// execOut runs a command and returns its stdout (ignoring stderr), for
// read-only systemctl checks.
func execOut(ctx context.Context, name string, args ...string) ([]byte, error) {
	return Exec(ctx, name, args...)
}
