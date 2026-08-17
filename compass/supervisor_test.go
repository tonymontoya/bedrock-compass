package compass

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeExec scripts ExecFunc responses per command name and records calls.
type fakeExec struct {
	mu     sync.Mutex
	calls  []string
	output map[string][]byte
	fail   map[string]error
}

func (f *fakeExec) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	f.mu.Unlock()
	if err, ok := f.fail[name]; ok {
		return nil, err
	}
	if out, ok := f.output[name]; ok {
		return out, nil
	}
	return nil, nil
}

func (f *fakeExec) called(cmd string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == cmd {
			return true
		}
	}
	return false
}

func TestSupervisor_Systemd_StopStartRunning(t *testing.T) {
	f := &fakeExec{output: map[string][]byte{}}
	f.output["systemctl"] = []byte("active\n")
	s := NewSystemdSupervisor("minecraft-bedrock.service", f.run)
	ctx := context.Background()

	running, err := s.Running(ctx)
	if err != nil || !running {
		t.Fatalf("Running = %v, %v; want true, nil", running, err)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, want := range []string{
		"systemctl is-active minecraft-bedrock.service",
		"systemctl stop minecraft-bedrock.service",
		"systemctl start minecraft-bedrock.service",
	} {
		if !f.called(want) {
			t.Errorf("missing call %q; got %v", want, f.calls)
		}
	}
}

func TestSupervisor_Systemd_Inactive_IsNotRunning(t *testing.T) {
	f := &fakeExec{output: map[string][]byte{}, fail: map[string]error{}}
	f.output["systemctl"] = []byte("inactive\n")
	// systemctl is-active exits non-zero when the unit is inactive; an
	// inactive unit is a normal state, not an error.
	f.fail["systemctl"] = context.DeadlineExceeded
	s := NewSystemdSupervisor("u.service", f.run)
	running, err := s.Running(context.Background())
	if err != nil {
		t.Fatalf("Running on an inactive unit must not error: %v", err)
	}
	if running {
		t.Error("Running = true, want false for an inactive unit")
	}
}

func TestSupervisor_Systemd_StopErrorSurfaces(t *testing.T) {
	f := &fakeExec{fail: map[string]error{}}
	f.fail["systemctl"] = context.DeadlineExceeded
	s := NewSystemdSupervisor("u.service", f.run)
	if err := s.Stop(context.Background()); err == nil {
		t.Fatal("Stop must surface the systemctl failure")
	}
}

// newScriptedConsole builds a *Console whose NewSince answers each sent
// command from the respond map. It returns the console and a pointer to
// the list of sent lines.
func newScriptedConsole(t *testing.T, respond map[string]string) (*Console, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var sent []string
	c := &Console{
		Write: func(ctx context.Context, line string) error {
			mu.Lock()
			sent = append(sent, line)
			mu.Unlock()
			return nil
		},
		Baseline: func(ctx context.Context) (string, error) { return "tok", nil },
		NewSince: func(ctx context.Context, token string) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			if len(sent) == 0 {
				return nil, nil
			}
			return []string{respond[sent[len(sent)-1]]}, nil
		},
		Policy: ConsolePolicy{Quiesce: 2 * time.Millisecond, Timeout: 2 * time.Second, Poll: 2 * time.Millisecond},
	}
	return c, &sent
}

func containsLine(sent *[]string, line string) bool {
	for _, l := range *sent {
		if l == line {
			return true
		}
	}
	return false
}

func TestSupervisor_Tmux_Start_IsDeferred(t *testing.T) {
	c, _ := newScriptedConsole(t, nil)
	s := NewTmuxSupervisor(c, func(ctx context.Context) (bool, error) { return true, nil })
	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("tmux start must be deferred to ticket #13 with a loud error")
	}
	if !strings.Contains(err.Error(), "#13") {
		t.Errorf("error must name the deferral (ticket #13), got: %v", err)
	}
}

func TestSupervisor_Tmux_Stop_WaitsForProcessExit(t *testing.T) {
	c, sent := newScriptedConsole(t, map[string]string{"stop": "Server stopped."})
	var mu sync.Mutex
	calls := 0
	alive := 2 // report "still running" for the first two polls
	s := NewTmuxSupervisor(c, func(ctx context.Context) (bool, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return calls <= alive, nil
	})
	s.Poll = time.Millisecond
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !containsLine(sent, "stop") {
		t.Fatalf("console must receive the stop command, got %v", *sent)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls <= alive {
		t.Fatalf("Stop returned before the process exited (calls=%d)", calls)
	}
}

func TestSupervisor_Tmux_Stop_TimesOutWhenProcessLingers(t *testing.T) {
	c, _ := newScriptedConsole(t, map[string]string{"stop": "Saving..."})
	s := NewTmuxSupervisor(c, func(ctx context.Context) (bool, error) { return true, nil })
	s.Poll = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Stop(ctx); err == nil {
		t.Fatal("Stop must fail when the process never exits")
	}
}

func TestSupervisor_Tmux_RunningFallsBackToProcessCheck(t *testing.T) {
	c, _ := newScriptedConsole(t, nil)
	want := true
	s := NewTmuxSupervisor(c, func(ctx context.Context) (bool, error) { return want, nil })
	got, err := s.Running(context.Background())
	if err != nil || got != want {
		t.Fatalf("Running = %v, %v; want %v, nil", got, err, want)
	}
}
