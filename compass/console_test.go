package compass

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fastPolicy() ConsolePolicy {
	return ConsolePolicy{Quiesce: 20 * time.Millisecond, Timeout: 500 * time.Millisecond, Poll: 5 * time.Millisecond}
}

func TestConsole_Send_HarvestsSettledOutput(t *testing.T) {
	var writes []string
	call := 0
	c := &Console{
		Write:    func(ctx context.Context, line string) error { writes = append(writes, line); return nil },
		Baseline: func(ctx context.Context) (string, error) { return "tok", nil },
		NewSince: func(ctx context.Context, token string) ([]string, error) {
			call++
			if call == 1 {
				return []string{"line1"}, nil
			}
			return []string{"line1", "line2"}, nil
		},
		Policy: fastPolicy(),
	}
	out, err := c.Send(context.Background(), "help")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if out != "line1\nline2" {
		t.Fatalf("out=%q", out)
	}
	if len(writes) != 1 || writes[0] != "help" {
		t.Fatalf("writes=%q, want exactly the one command", writes)
	}
}

func TestConsole_Send_TimeoutReturnsPartial(t *testing.T) {
	call := 0
	c := &Console{
		Write:    func(ctx context.Context, line string) error { return nil },
		Baseline: func(ctx context.Context) (string, error) { return "tok", nil },
		NewSince: func(ctx context.Context, token string) ([]string, error) {
			call++
			lines := make([]string, call)
			for i := range lines {
				lines[i] = "streaming"
			}
			return lines, nil
		},
		Policy: ConsolePolicy{Quiesce: 5 * time.Millisecond, Timeout: 40 * time.Millisecond, Poll: 5 * time.Millisecond},
	}
	out, err := c.Send(context.Background(), "slowcmd")
	if err == nil {
		t.Fatal("Send must fail when output never settles")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err=%v, want ErrTimeout", err)
	}
	if out == "" {
		t.Fatal("partial output must be returned on timeout")
	}
}

func TestConsole_Send_WriteFailureStopsCleanly(t *testing.T) {
	reads := 0
	c := &Console{
		Write:    func(ctx context.Context, line string) error { return errors.New("fifo: no such file or directory") },
		Baseline: func(ctx context.Context) (string, error) { return "tok", nil },
		NewSince: func(ctx context.Context, token string) ([]string, error) { reads++; return nil, nil },
		Policy:   fastPolicy(),
	}
	start := time.Now()
	_, err := c.Send(context.Background(), "help")
	if err == nil || !strings.Contains(err.Error(), "fifo") {
		t.Fatalf("err=%v, want the write failure", err)
	}
	if reads != 0 {
		t.Fatalf("must not harvest after a failed write (reads=%d)", reads)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("failure path took %v", time.Since(start))
	}
}

func TestConsole_Send_CtxCancelReturnsPartial(t *testing.T) {
	c := &Console{
		Write:    func(ctx context.Context, line string) error { return nil },
		Baseline: func(ctx context.Context) (string, error) { return "tok", nil },
		NewSince: func(ctx context.Context, token string) ([]string, error) {
			return []string{"partial"}, nil
		},
		Policy: ConsolePolicy{Quiesce: 5 * time.Second, Timeout: 5 * time.Second, Poll: 5 * time.Millisecond},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(25 * time.Millisecond)
		cancel()
	}()
	out, err := c.Send(ctx, "help")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if out != "partial" {
		t.Fatalf("out=%q, want the partial output", out)
	}
}

// TestConsole_Send_NoFIFOReader_CleanErrorNoHang exercises the real FIFO
// open path: a console FIFO with no attached server must fail immediately
// (ENXIO), never hang.
func TestConsole_Send_NoFIFOReader_CleanErrorNoHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO test")
	}
	fifo := filepath.Join(t.TempDir(), "console.fifo")
	if err := syscall.Mkfifo(fifo, 0o666); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	c := &Console{
		Write:    fifoWrite(fifo),
		Baseline: func(ctx context.Context) (string, error) { return "tok", nil },
		NewSince: func(ctx context.Context, token string) ([]string, error) { return nil, nil },
		Policy:   ConsolePolicy{Quiesce: 50 * time.Millisecond, Timeout: 300 * time.Millisecond, Poll: 5 * time.Millisecond},
	}
	start := time.Now()
	_, err := c.Send(context.Background(), "help")
	if err == nil {
		t.Fatal("Send against a reader-less FIFO must fail")
	}
	if !errors.Is(err, syscall.ENXIO) && !os.IsNotExist(err) {
		t.Fatalf("err=%v, want the FIFO open failure (no reader)", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("failure path took %v; must not hang", time.Since(start))
	}
	t.Logf("clean error: %v", err)
}

func TestConsole_NewTmuxConsole_WritesCommandAndEnter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim test")
	}
	argsFile := installFakeTmux(t, "", 0)
	c := NewTmuxConsole("bedrock:0.0", fastPolicy())
	if err := c.Write(context.Background(), "list"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// The shim records one arg per line; joined, the two invocations must be
	// the literal command line, then Enter.
	joined := strings.Join(readArgs(t, argsFile), " ")
	want := "send-keys -t bedrock:0.0 -l -- list send-keys -t bedrock:0.0 Enter"
	if joined != want {
		t.Fatalf("tmux calls=%q, want %q", joined, want)
	}
}
