package compass

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func installFakeTmux(t *testing.T, stdout string, exit int) (argsFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell shim test")
	}
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	stdoutFile := filepath.Join(dir, "stdout")
	if err := os.WriteFile(stdoutFile, []byte(stdout), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> \"$TMUX_ARGS_FILE\"\n" +
		"cat \"$TMUX_STDOUT_FILE\"\n" +
		"exit " + strconv.Itoa(exit) + "\n"
	fake := filepath.Join(dir, "tmux")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_ARGS_FILE", argsFile)
	t.Setenv("TMUX_STDOUT_FILE", stdoutFile)
	return argsFile
}

// setFakeTmuxOutput changes the canned tmux output between calls.
func setFakeTmuxOutput(t *testing.T, argsFile, stdout string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(argsFile), "stdout"), []byte(stdout), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPane_Capture_Args(t *testing.T) {
	argsFile := installFakeTmux(t, "line\n", 0)
	p := &Pane{Target: "bedrock:0.0"}
	if _, err := p.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	joined := strings.Join(readArgs(t, argsFile), " ")
	if joined != "capture-pane -t bedrock:0.0 -p -S -" {
		t.Fatalf("args=%q, want %q", joined, "capture-pane -t bedrock:0.0 -p -S -")
	}
}

func TestPane_BaselineThenNewSince(t *testing.T) {
	argsFile := installFakeTmux(t, "boot line 1\nboot line 2\nready\n", 0)
	p := &Pane{Target: "bedrock:0.0"}
	token, err := p.Baseline(context.Background())
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	// Server appends its command output.
	setFakeTmuxOutput(t, argsFile, "boot line 1\nboot line 2\nready\nThere are 0/15 players online:\n")
	lines, err := p.NewSince(context.Background(), token)
	if err != nil {
		t.Fatalf("NewSince: %v", err)
	}
	if len(lines) != 1 || lines[0] != "There are 0/15 players online:" {
		t.Fatalf("lines=%q, want the single appended line", lines)
	}
}

func TestPane_NewSince_NoNewLines(t *testing.T) {
	installFakeTmux(t, "a\nb\n", 0)
	p := &Pane{Target: "bedrock:0.0"}
	token, err := p.Baseline(context.Background())
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	lines, err := p.NewSince(context.Background(), token)
	if err != nil {
		t.Fatalf("NewSince: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("lines=%q, want empty", lines)
	}
}

func TestPane_NewSince_PaneCleared(t *testing.T) {
	argsFile := installFakeTmux(t, "a\nb\n", 0)
	p := &Pane{Target: "bedrock:0.0"}
	token, err := p.Baseline(context.Background())
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	setFakeTmuxOutput(t, argsFile, "totally\ndifferent\n")
	if _, err := p.NewSince(context.Background(), token); err == nil {
		t.Fatal("cleared pane must fail cleanly, not return a guess")
	}
}
