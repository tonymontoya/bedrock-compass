//go:build integration

package compass

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestIntegration_Provision_Tmux_EndToEnd runs a real tmux provision against a
// local bedrock-server zip: it installs the server into a fresh scratch dir,
// accepts the EULA, and starts a tmux session that runs bedrock_server. It
// verifies the real exec worked — the server process is actually running out of
// the scratch dir. It self-skips when tmux or the zip is absent. Cleanup kills
// the session, the server process, and removes the scratch dir.
//
// The joinability (pong) check is intentionally NOT part of this e2e: on a
// fresh world the first launch generates the world, which is slow and variable,
// and would make the test flaky. The pong logic is covered by the offline
// VerifyRetry / happy-path tests; this e2e proves the real-world wiring.
// Opt-in: built with -tags integration and shipped via make integ RIG_HOST=bds.
func TestIntegration_Provision_Tmux_EndToEnd(t *testing.T) {
	zipPath := envOr("COMPASS_PROV_ZIP", "/opt/minecraft/server/bedrock-server-1.26.45.1.zip")
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	if _, err := os.Stat(zipPath); err != nil {
		t.Skipf("bedrock-server zip not present at %s: %v", zipPath, err)
	}

	name := envOr("COMPASS_PROV_NAME", "compass-prov-e2e")
	port := provPort(t, envOr("COMPASS_PROV_PORT", "19199"))

	// A scratch parent (auto-cleaned) and a fresh server dir the provision
	// will create.
	base := t.TempDir()
	serverDir := filepath.Join(base, "prov-e2e-server")

	// Clean slate: kill any pre-existing session with this name.
	exec.Command("tmux", "kill-session", "-t", name).Run()

	// Cleanup: stop the session, the server process, and remove the dir.
	defer func() {
		exec.Command("tmux", "kill-session", "-t", name).Run()
		killServerInDir(serverDir)
		os.RemoveAll(serverDir)
	}()

	in := ProvisionInput{
		Name:       name,
		ServerDir:  serverDir,
		Supervisor: "tmux",
		Source:     SourceZip,
		ZipPath:    zipPath,
		Port:       port,
	}

	// Verify the real exec worked: the bedrock_server process is running out of
	// the scratch dir. (Not a pong — see the test comment above.)
	p := &Provision{
		Env: ResolveProvisionEnv(),
		Verify: func(ctx context.Context, addr string) error {
			return serverProcRunning(serverDir)
		},
		VerifyDeadline: 90 * time.Second,
		VerifyInterval: 2 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	res, err := p.Apply(ctx, in)
	t.Logf("apply: status=%s detail=%q err=%v", res.Status, res.Detail, err)
	if res.Status != "provisioned" {
		t.Fatalf("provision status = %q, want provisioned (err=%v)", res.Status, err)
	}

	// The tmux session should be running.
	if out, e := exec.Command("tmux", "has-session", "-t", name).CombinedOutput(); e != nil {
		t.Fatalf("tmux has-session %s: %v (%s)", name, e, strings.TrimSpace(string(out)))
	}

	// The server binary and EULA should be in place.
	for _, f := range []string{"bedrock_server", "eula.txt"} {
		if _, e := os.Stat(filepath.Join(serverDir, f)); e != nil {
			t.Fatalf("%s not present after provision: %v", f, e)
		}
	}
}

// serverProcRunning reports whether a bedrock_server process whose working
// directory is dir is currently running. It scans /proc, so it works on the
// test host without any helper binary.
func serverProcRunning(dir string) error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return fmt.Errorf("read /proc: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a PID
		}
		comm, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err != nil || strings.TrimSpace(string(comm)) != "bedrock_server" {
			continue
		}
		cwd, err := os.Readlink("/proc/" + e.Name() + "/cwd")
		if err != nil {
			continue
		}
		if cwd == dir {
			return nil
		}
	}
	return fmt.Errorf("no bedrock_server process running with cwd %s", dir)
}

// killServerInDir kills any bedrock_server process running out of dir. Best
// effort: used only by test cleanup.
func killServerInDir(dir string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err != nil || strings.TrimSpace(string(comm)) != "bedrock_server" {
			continue
		}
		cwd, err := os.Readlink("/proc/" + e.Name() + "/cwd")
		if err != nil || cwd != dir {
			continue
		}
		p, err := os.FindProcess(pid)
		if err == nil {
			p.Kill()
		}
	}
}

func provPort(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("invalid port %q: %v", s, err)
	}
	return n
}
