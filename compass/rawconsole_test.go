package compass

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRawConsole builds a Console whose Send settles immediately with the
// given output, recording every command it is asked to send.
func fakeRawConsole(out string, writes *[]string) *Console {
	return &Console{
		Write:    func(ctx context.Context, line string) error { *writes = append(*writes, line); return nil },
		Baseline: func(ctx context.Context) (string, error) { return "tok", nil },
		NewSince: func(ctx context.Context, token string) ([]string, error) {
			if out == "" {
				return nil, nil
			}
			return strings.Split(out, "\n"), nil
		},
		Policy: fastPolicy(),
	}
}

func TestRawConsole_Send_VerbatimAndAudited(t *testing.T) {
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	var writes []string
	rc := &RawConsole{Console: fakeRawConsole("There are 0/15 players online:", &writes), Trail: trail}

	out, err := rc.Send(context.Background(), "list")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if out != "There are 0/15 players online:" {
		t.Fatalf("out=%q", out)
	}
	if len(writes) != 1 || writes[0] != "list" {
		t.Fatalf("writes=%q, want the command sent verbatim once", writes)
	}

	evs, err := trail.Recent(10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("want exactly one audit event, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Verb != "console" {
		t.Errorf("verb=%q, want \"console\"", ev.Verb)
	}
	if ev.Params["command"] != "list" {
		t.Errorf("params.command=%q, want the verbatim command", ev.Params["command"])
	}
	if ev.Result != "ok" {
		t.Errorf("result=%q, want ok", ev.Result)
	}
}

func TestRawConsole_Send_MultiLineOutput(t *testing.T) {
	var writes []string
	rc := &RawConsole{Console: fakeRawConsole("line one\nline two\nline three", &writes), Trail: nil}
	out, err := rc.Send(context.Background(), "help")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if out != "line one\nline two\nline three" {
		t.Fatalf("multi-line output must stream intact, got %q", out)
	}
}

func TestRawConsole_Send_FailureIsAudited(t *testing.T) {
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	call := 0
	rc := &RawConsole{
		Console: &Console{
			Write:    func(ctx context.Context, line string) error { return nil },
			Baseline: func(ctx context.Context) (string, error) { return "tok", nil },
			NewSince: func(ctx context.Context, token string) ([]string, error) {
				call++ // keep growing so the output never settles -> timeout
				lines := make([]string, call)
				for i := range lines {
					lines[i] = "streaming"
				}
				return lines, nil
			},
			Policy: ConsolePolicy{Quiesce: 5 * time.Millisecond, Timeout: 30 * time.Millisecond, Poll: 5 * time.Millisecond},
		},
		Trail: trail,
	}
	out, err := rc.Send(context.Background(), "slowcmd")
	if err == nil {
		t.Fatal("Send must fail when output never settles")
	}
	if out == "" {
		t.Fatal("partial output must be returned on timeout")
	}

	evs, _ := trail.Recent(10)
	if len(evs) != 1 {
		t.Fatalf("want one audit event for the failed command, got %d", len(evs))
	}
	if evs[0].Verb != "console" || !strings.HasPrefix(evs[0].Result, "error:") {
		t.Errorf("failed raw command must be audited with an error result, got %+v", evs[0])
	}
	if evs[0].Params["command"] != "slowcmd" {
		t.Errorf("params.command=%q, want the verbatim command", evs[0].Params["command"])
	}
}

func TestRawConsole_Send_NilChannel_CleanErrorStillAudited(t *testing.T) {
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	rc := &RawConsole{Console: nil, Trail: trail}
	out, err := rc.Send(context.Background(), "list")
	if err == nil || !strings.Contains(err.Error(), "no console channel") {
		t.Fatalf("err=%v, want the clean no-channel error", err)
	}
	if out != "" {
		t.Fatalf("out=%q, want empty on a failed send", out)
	}
	evs, _ := trail.Recent(10)
	if len(evs) != 1 || evs[0].Verb != "console" || !strings.HasPrefix(evs[0].Result, "error:") {
		t.Fatalf("a nil-channel attempt must still be audited, got %+v", evs)
	}
}

func TestRawConsole_Send_NilTrail_NoPanic(t *testing.T) {
	var writes []string
	rc := &RawConsole{Console: fakeRawConsole("ok", &writes), Trail: nil}
	if _, err := rc.Send(context.Background(), "list"); err != nil {
		t.Fatalf("Send with a nil trail must not fail: %v", err)
	}
}
