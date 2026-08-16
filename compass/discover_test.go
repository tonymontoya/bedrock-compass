package compass

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiscover_NoServer_Errors(t *testing.T) {
	d := &Discoverer{Procs: func(ctx context.Context) ([]Proc, error) { return nil, nil }}
	if _, err := d.Discover(context.Background()); err == nil {
		t.Fatal("no running server must be an error")
	}
}

func TestDiscover_FindsProcess(t *testing.T) {
	d := &Discoverer{
		Procs: func(ctx context.Context) ([]Proc, error) {
			return []Proc{
				{PID: 100, Name: "systemd", Cwd: "/"},
				{PID: 200, Name: "bedrock_server", Cwd: "/srv/bedrock"},
				{PID: 300, Name: "sshd", Cwd: "/"},
			}, nil
		},
	}
	got, err := d.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.PID != 200 || got.Dir != "/srv/bedrock" {
		t.Errorf("got %+v", got)
	}
}

func TestDiscover_Ambiguous_Errors(t *testing.T) {
	d := &Discoverer{
		Procs: func(ctx context.Context) ([]Proc, error) {
			return []Proc{
				{PID: 11, Name: "bedrock_server", Cwd: "/a"},
				{PID: 22, Name: "bedrock_server", Cwd: "/b"},
			}, nil
		},
	}
	_, err := d.Discover(context.Background())
	if err == nil {
		t.Fatal("two servers must be an error (v1 is single-server)")
	}
	for _, want := range []string{"11", "22"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name both pids, got: %v", err)
		}
	}
}

func TestDiscover_WithUnit(t *testing.T) {
	d := &Discoverer{
		Procs: func(ctx context.Context) ([]Proc, error) {
			return []Proc{{PID: 200, Name: "bedrock_server", Cwd: "/srv/bedrock"}}, nil
		},
		UnitForDir: func(ctx context.Context, dir string) (string, error) {
			if dir != "/srv/bedrock" {
				t.Errorf("UnitForDir called with %q", dir)
			}
			return "minecraft-bedrock.service", nil
		},
	}
	got, err := d.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.Unit != "minecraft-bedrock.service" {
		t.Errorf("Unit = %q", got.Unit)
	}
}

func TestDiscover_NoUnitIsFine(t *testing.T) {
	d := &Discoverer{
		Procs: func(ctx context.Context) ([]Proc, error) {
			return []Proc{{PID: 200, Name: "bedrock_server", Cwd: "/srv/bedrock"}}, nil
		},
	}
	got, err := d.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover without unit lookup must succeed (tmux/nohup): %v", err)
	}
	if got.Unit != "" {
		t.Errorf("Unit = %q, want empty", got.Unit)
	}
}

// TestDiscover_Live spawns a stand-in process named bedrock_server in a temp
// dir and verifies the real /proc-backed discovery finds it.
func TestDiscover_Live(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bedrock_server")
	src, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleep binary: %v", err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read sleep: %v", err)
	}
	if err := os.WriteFile(bin, data, 0o755); err != nil {
		t.Fatalf("copy sleep: %v", err)
	}
	cmd := exec.Command(bin, "300")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stand-in: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.Dir != dir {
		t.Errorf("Dir = %q, want %q", got.Dir, dir)
	}
	if got.PID != cmd.Process.Pid {
		t.Errorf("PID = %d, want %d", got.PID, cmd.Process.Pid)
	}
}
