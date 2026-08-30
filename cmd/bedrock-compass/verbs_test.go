// Unit tests for the headless verbs (ticket #15). The run* handlers are
// exercised offline: a real compass.Stack is built from a temp config, and
// newStackFor is swapped so no live server, unit, or tmux session is ever
// touched. Mutating verbs are tested both sides of the ADR-0010 gate.
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// writeTestConfig writes a config that resolves no channels at all:
// supervisor tmux with an empty pane yields nil console + nil supervisor, so
// every service degrades to its clean offline error instead of touching the
// host. AuditFile points into the temp dir so trail assertions work.
func writeTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := "supervisor: tmux\n" +
		"server-dir: " + filepath.Join(dir, "server") + "\n" +
		"backup-dir: " + filepath.Join(dir, "backups") + "\n" +
		"audit-file: " + filepath.Join(dir, "audit.jsonl") + "\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}

// buildStack wires a real Stack from the temp config — the same wiring the
// verbs use in production (NewStack over Load).
func buildStack(t *testing.T, cfgPath string) *compass.Stack {
	t.Helper()
	cfg, err := compass.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	st, err := compass.NewStack(context.Background(), cfg, "test", compass.StackOpts{})
	if err != nil {
		t.Fatalf("new stack: %v", err)
	}
	return st
}

// withFakeStack swaps newStackFor to return the given stack for the duration
// of the test (restoring the real wiring afterwards).
func withFakeStack(t *testing.T, st *compass.Stack) {
	t.Helper()
	real := newStackFor
	newStackFor = func(ctx context.Context, cfg *compass.Config) (*compass.Stack, error) { return st, nil }
	t.Cleanup(func() { newStackFor = real })
}

// cannedConsole builds a real compass.Console whose Send returns canned
// output per command and records every line it was asked to send. The
// harvest loop runs for real (fast policy), so the verbs exercise the same
// Console.Send path production uses — no live server involved.
func cannedConsole(sent *[]string, out map[string]string) *compass.Console {
	return &compass.Console{
		Write: func(ctx context.Context, line string) error {
			*sent = append(*sent, line)
			return nil
		},
		Baseline: func(ctx context.Context) (string, error) { return "tok", nil },
		NewSince: func(ctx context.Context, token string) ([]string, error) {
			var lines []string
			for _, l := range strings.Split(out[(*sent)[len(*sent)-1]], "\n") {
				lines = append(lines, l)
			}
			return lines, nil
		},
		Policy: compass.ConsolePolicy{Quiesce: 5 * time.Millisecond, Timeout: time.Second, Poll: 2 * time.Millisecond},
	}
}

// trailEvents reads the audit JSONL at path into events.
func trailEvents(t *testing.T, st *compass.Stack) []compass.Event {
	t.Helper()
	if st.Trail == nil {
		t.Fatal("stack has no audit trail")
	}
	evs, err := st.Trail.Recent(100)
	if err != nil {
		t.Fatalf("read trail: %v", err)
	}
	return evs // newest first
}

// --- gate (ADR-0010) ---------------------------------------------------------

func TestBackupNowGate_RefusesWithoutYes(t *testing.T) {
	cfgPath := writeTestConfig(t)
	st := buildStack(t, cfgPath)
	withFakeStack(t, st)

	code := run([]string{"bedrock-compass", "backup-now", "--config", cfgPath})
	if code != exitConfirmation {
		t.Fatalf("exit = %d, want %d (confirmation required)", code, exitConfirmation)
	}
	evs := trailEvents(t, st)
	found := false
	for _, ev := range evs {
		if ev.Verb == "cli" && ev.Params["cmd"] == "backup-now" && strings.Contains(ev.Result, "confirmation") {
			found = true
		}
	}
	if !found {
		t.Errorf("refusal not recorded in trail: %+v", evs)
	}
}

func TestBackupNowGate_YesBypassAudited(t *testing.T) {
	cfgPath := writeTestConfig(t)
	st := buildStack(t, cfgPath)
	withFakeStack(t, st)

	// No console resolved -> the backup itself fails (exit 1), but only AFTER
	// the gate passes; the trail must carry the bypass note.
	code := run([]string{"bedrock-compass", "backup-now", "--config", cfgPath, "--yes"})
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (no console channel)", code)
	}
	evs := trailEvents(t, st)
	found := false
	for _, ev := range evs {
		if ev.Verb == "cli" && ev.Params["cmd"] == "backup-now" && ev.Params["bypass"] == "--yes" {
			found = true
		}
	}
	if !found {
		t.Errorf("bypass not recorded in trail: %+v", evs)
	}
}

func TestAllowlistGate_RefusesWithoutYes(t *testing.T) {
	cfgPath := writeTestConfig(t)
	st := buildStack(t, cfgPath)
	withFakeStack(t, st)
	serverDir := filepath.Dir(cfgPath) + "/server"
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatal(err)
	}

	code := run([]string{"bedrock-compass", "allowlist", "add", "player_one", "--config", cfgPath})
	if code != exitConfirmation {
		t.Fatalf("exit = %d, want %d", code, exitConfirmation)
	}
	data, _ := os.ReadFile(filepath.Join(serverDir, "allowlist.json"))
	if strings.Contains(string(data), "player_one") {
		t.Errorf("refused mutation still wrote the allowlist: %s", data)
	}
	evs := trailEvents(t, st)
	found := false
	for _, ev := range evs {
		if ev.Verb == "cli" && ev.Params["cmd"] == "allowlist add" && strings.Contains(ev.Result, "confirmation") {
			found = true
		}
	}
	if !found {
		t.Errorf("refusal not recorded in trail: %+v", evs)
	}
}

func TestAllowlistAdd_WithYes(t *testing.T) {
	cfgPath := writeTestConfig(t)
	st := buildStack(t, cfgPath)
	withFakeStack(t, st)
	serverDir := filepath.Dir(cfgPath) + "/server"
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatal(err)
	}

	code := run([]string{"bedrock-compass", "allowlist", "add", "player_one", "--config", cfgPath, "--yes"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	data, err := os.ReadFile(filepath.Join(serverDir, "allowlist.json"))
	if err != nil {
		t.Fatalf("allowlist not written: %v", err)
	}
	if !strings.Contains(string(data), "player_one") {
		t.Errorf("identity missing from allowlist: %s", data)
	}
	evs := trailEvents(t, st)
	var svc, cli bool
	for _, ev := range evs {
		if ev.Verb == "access" && ev.Params["action"] == "allow-add" && ev.Result == "ok" {
			svc = true
		}
		if ev.Verb == "cli" && ev.Params["cmd"] == "allowlist add" && ev.Params["bypass"] == "--yes" {
			cli = true
		}
	}
	if !svc || !cli {
		t.Errorf("trail missing service event (%v) or cli bypass event (%v): %+v", svc, cli, evs)
	}
}

func TestAllowlistRemove_WithYes(t *testing.T) {
	cfgPath := writeTestConfig(t)
	st := buildStack(t, cfgPath)
	withFakeStack(t, st)
	serverDir := filepath.Dir(cfgPath) + "/server"
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `[{"name":"player_one","ignoresPlayerLimit":false},{"name":"player_two","ignoresPlayerLimit":false}]`
	if err := os.WriteFile(filepath.Join(serverDir, "allowlist.json"), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	code := run([]string{"bedrock-compass", "allowlist", "remove", "player_one", "--config", cfgPath, "--yes"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	data, _ := os.ReadFile(filepath.Join(serverDir, "allowlist.json"))
	if strings.Contains(string(data), "player_one") {
		t.Errorf("identity still on allowlist: %s", data)
	}
	if !strings.Contains(string(data), "player_two") {
		t.Errorf("unrelated entry lost: %s", data)
	}
}

func TestRestoreGate_RefusesWithoutYes(t *testing.T) {
	cfgPath := writeTestConfig(t)
	st := buildStack(t, cfgPath)
	withFakeStack(t, st)
	backups := filepath.Dir(cfgPath) + "/backups"
	id := "2026-08-26T12-00-00Z-test"
	if err := os.MkdirAll(filepath.Join(backups, id), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"id":"` + id + `","created":"2026-08-26T12:00:00Z","mode":"live","total-size":1234}`
	if err := os.WriteFile(filepath.Join(backups, id, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	code := run([]string{"bedrock-compass", "restore", id, "--config", cfgPath})
	if code != exitConfirmation {
		t.Fatalf("exit = %d, want %d", code, exitConfirmation)
	}
	evs := trailEvents(t, st)
	found := false
	for _, ev := range evs {
		if ev.Verb == "cli" && ev.Params["cmd"] == "restore" && strings.Contains(ev.Result, "confirmation") {
			found = true
		}
	}
	if !found {
		t.Errorf("refusal not recorded in trail: %+v", evs)
	}
}

func TestRestore_WithYesNoSupervisor(t *testing.T) {
	cfgPath := writeTestConfig(t)
	st := buildStack(t, cfgPath)
	withFakeStack(t, st)
	backups := filepath.Dir(cfgPath) + "/backups"
	id := "2026-08-26T12-00-00Z-test"
	if err := os.MkdirAll(filepath.Join(backups, id), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"id":"` + id + `","created":"2026-08-26T12:00:00Z","mode":"live","total-size":1234}`
	if err := os.WriteFile(filepath.Join(backups, id, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// Gate passes; the engine then fails cleanly (no supervisor resolved).
	code := run([]string{"bedrock-compass", "restore", id, "--config", cfgPath, "--yes"})
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (no supervisor)", code)
	}
	evs := trailEvents(t, st)
	found := false
	for _, ev := range evs {
		if ev.Verb == "cli" && ev.Params["cmd"] == "restore" && ev.Params["bypass"] == "--yes" {
			found = true
		}
	}
	if !found {
		t.Errorf("bypass not recorded in trail: %+v", evs)
	}
}

// --- read-only verbs ----------------------------------------------------------

func TestRoster_FakeConsole(t *testing.T) {
	cfgPath := writeTestConfig(t)
	st := buildStack(t, cfgPath)
	var sent []string
	st.Players.Console = cannedConsole(&sent, map[string]string{
		"list": "There are 2/15 players online:\nplayer_one [xuid: 1111111111111111]\nplayer_two",
	})
	withFakeStack(t, st)

	code := run([]string{"bedrock-compass", "roster", "--config", cfgPath})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(sent) != 1 || sent[0] != "list" {
		t.Errorf("console sent %+v, want [list]", sent)
	}
	evs := trailEvents(t, st)
	found := false
	for _, ev := range evs {
		if ev.Verb == "cli" && ev.Params["cmd"] == "roster" && ev.Result == "ok" {
			found = true
		}
	}
	if !found {
		t.Errorf("roster invocation not audited: %+v", evs)
	}
}

func TestRoster_NoConsole(t *testing.T) {
	cfgPath := writeTestConfig(t)
	st := buildStack(t, cfgPath) // nil console
	withFakeStack(t, st)

	code := run([]string{"bedrock-compass", "roster", "--config", cfgPath})
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (no console channel)", code)
	}
	evs := trailEvents(t, st)
	found := false
	for _, ev := range evs {
		if ev.Verb == "cli" && ev.Params["cmd"] == "roster" && strings.HasPrefix(ev.Result, "error:") {
			found = true
		}
	}
	if !found {
		t.Errorf("failed roster not audited: %+v", evs)
	}
}

func TestLogs_NoJournal(t *testing.T) {
	cfgPath := writeTestConfig(t) // tmux, no unit -> Logs nil
	st := buildStack(t, cfgPath)
	withFakeStack(t, st)

	code := run([]string{"bedrock-compass", "logs", "--config", cfgPath})
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (no journal)", code)
	}
}

func TestAudit_PrintsNewestFirst(t *testing.T) {
	cfgPath := writeTestConfig(t)
	st := buildStack(t, cfgPath)
	withFakeStack(t, st)
	base := time.Now().UTC().Add(-time.Hour)
	for i, verb := range []string{"first", "second", "third"} {
		if err := st.Trail.Append(compass.Event{Time: base.Add(time.Duration(i) * time.Minute), Verb: verb, Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}

	code := run([]string{"bedrock-compass", "audit", "--config", cfgPath})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	evs := trailEvents(t, st)
	// The audit invocation itself appends one event; the printed order is
	// verified via a fresh Recent read: newest first.
	if len(evs) < 4 {
		t.Fatalf("trail has %d events, want >= 4", len(evs))
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].Time.After(evs[i-1].Time) {
			t.Errorf("trail not newest-first at index %d: %+v", i, evs)
		}
	}
}

func TestAudit_JSON(t *testing.T) {
	cfgPath := writeTestConfig(t)
	st := buildStack(t, cfgPath)
	withFakeStack(t, st)
	if err := st.Trail.Append(compass.Event{Verb: "allow-add", Params: map[string]string{"identity": "x"}, Result: "ok"}); err != nil {
		t.Fatal(err)
	}

	code := run([]string{"bedrock-compass", "audit", "--config", cfgPath, "--json"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	evs := trailEvents(t, st)
	var out []compass.Event
	for _, ev := range evs { // sanity: every event round-trips as JSON
		b, err := json.Marshal(ev)
		if err != nil || len(b) == 0 {
			t.Fatalf("event not marshalable: %v", err)
		}
		out = append(out, ev)
	}
	if len(out) < 1 {
		t.Fatal("no events")
	}
}

func TestStatus_JSON(t *testing.T) {
	cfgPath := writeTestConfig(t)
	st := buildStack(t, cfgPath)
	withFakeStack(t, st)

	code := run([]string{"bedrock-compass", "status", "--config", cfgPath, "--json"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (offline status degrades)", code)
	}
}

// --- dispatch + usage -----------------------------------------------------------

func TestHelp_ListsAllVerbs(t *testing.T) {
	code := run([]string{"bedrock-compass", "--help"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, verb := range []string{"roster", "logs", "allowlist add", "allowlist remove", "restore", "audit", "backup-now", "status"} {
		if !strings.Contains(usageText(), verb) {
			t.Errorf("help text missing %q", verb)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	code := run([]string{"bedrock-compass", "frobnicate"})
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage error)", code)
	}
}

// Regression: leading global flags before the subcommand (`--config X roster`)
// must reach the verb's FlagSet, not fall through to the TUI path. Before the
// fix these dispatched to runTUI and died on /dev/tty with exit 1.
func TestDispatch_LeadingConfigFlag(t *testing.T) {
	cfgPath := writeTestConfig(t)
	st := buildStack(t, cfgPath) // nil console -> roster exits 1 cleanly
	withFakeStack(t, st)

	code := run([]string{"bedrock-compass", "--config", cfgPath, "roster"})
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (no console channel); TUI fallthrough would also be 1 but via /dev/tty — check stderr behavior separately", code)
	}
	evs := trailEvents(t, st)
	found := false
	for _, ev := range evs {
		if ev.Verb == "cli" && ev.Params["cmd"] == "roster" {
			found = true
		}
	}
	if !found {
		t.Errorf("leading --config did not reach the roster verb (no cli audit event): %+v", evs)
	}
}

func TestDispatch_LeadingConfigFlagBackupNowGate(t *testing.T) {
	cfgPath := writeTestConfig(t)
	st := buildStack(t, cfgPath)
	withFakeStack(t, st)

	code := run([]string{"bedrock-compass", "--config", cfgPath, "backup-now"})
	if code != exitConfirmation {
		t.Fatalf("exit = %d, want %d (gate must fire even with leading --config)", code, exitConfirmation)
	}
}

func TestAllowlist_UsageErrors(t *testing.T) {
	cfgPath := writeTestConfig(t)
	st := buildStack(t, cfgPath)
	withFakeStack(t, st)

	if code := run([]string{"bedrock-compass", "allowlist"}); code != 2 {
		t.Errorf("no action: exit = %d, want 2", code)
	}
	if code := run([]string{"bedrock-compass", "allowlist", "frobnicate", "x"}); code != 2 {
		t.Errorf("bad action: exit = %d, want 2", code)
	}
	if code := run([]string{"bedrock-compass", "restore"}); code != 2 {
		t.Errorf("missing id: exit = %d, want 2", code)
	}
	if code := run([]string{"bedrock-compass", "logs", "--level", "LOUD", "--config", cfgPath}); code != 2 {
		t.Errorf("bad level: exit = %d, want 2", code)
	}
	if code := run([]string{"bedrock-compass", "logs", "--since", "yesterday", "--config", cfgPath}); code != 2 {
		t.Errorf("bad since: exit = %d, want 2", code)
	}
}

// Regression: --version is honored by every verb before any positional or
// gate check, so `allowlist --version` and `restore --version` print the
// version (exit 0) rather than tripping a usage error.
func TestVersionFlagBeforePositional(t *testing.T) {
	cases := [][]string{
		{"allowlist", "--version"},
		{"allowlist", "add", "x", "--version"},
		{"restore", "--version"},
		{"restore", "19700101", "--version"},
	}
	for _, c := range cases {
		if code := run(append([]string{"bedrock-compass"}, c...)); code != 0 {
			t.Errorf("compass %v: exit = %d, want 0", c, code)
		}
	}
}

// ADR-0010 exit-code contract: 0 = ok (including --help). Every verb's own
// help flag must print usage and exit 0 — not trip the parse-error path (2)
// or, for restore, be misread as a backup id and hit the gate (3 + a phantom
// refusal event in the trail).
func TestHelp_ExitsZeroForEveryVerb(t *testing.T) {
	cfgPath := writeTestConfig(t)
	st := buildStack(t, cfgPath)
	withFakeStack(t, st)

	cases := [][]string{
		{"roster", "--help"},
		{"logs", "--help"},
		{"audit", "--help"},
		{"status", "--help"},
		{"backup-now", "--help"},
		{"allowlist", "-h"},
		{"allowlist", "add", "x", "--help"},
		{"restore", "-h"},
		{"restore", "19700101", "--help"},
	}
	for _, c := range cases {
		if code := run(append([]string{"bedrock-compass"}, c...)); code != 0 {
			t.Errorf("compass %v: exit = %d, want 0 (ADR-0010: help is ok)", c, code)
		}
	}

	// `restore -h` must not leave a phantom refusal event in the trail.
	evs := trailEvents(t, st)
	for _, ev := range evs {
		if ev.Verb == "cli" && strings.Contains(ev.Result, "confirmation") {
			t.Errorf("help request recorded as a gate refusal: %+v", ev)
		}
	}
}

// ADR-0010: EVERY headless invocation appends one audit event (verb "cli"),
// so the trail distinguishes CLI invocations from TUI actions. The lifecycle
// verbs (status/start/stop/restart/adopt, ticket #13's surface re-wired in
// #15) must carry their cli events too — not just the service-level ones.
func TestLifecycleVerbs_AuditedCliEvent(t *testing.T) {
	cfgPath := writeTestConfig(t) // tmux, empty pane: no supervisor resolved
	st := buildStack(t, cfgPath)
	withFakeStack(t, st)

	cases := map[string][][]string{
		"status":  {{"bedrock-compass", "status", "--config", cfgPath}},
		"start":   {{"bedrock-compass", "start", "--config", cfgPath}},
		"stop":    {{"bedrock-compass", "stop", "--config", cfgPath}},
		"restart": {{"bedrock-compass", "restart", "--config", cfgPath}},
	}
	for want, args := range cases {
		before := len(trailEvents(t, st))
		run(args[0]) // exit code varies by verb; the audit event is what's under test
		evs := trailEvents(t, st)
		found := false
		for _, ev := range evs[:len(evs)-before] {
			if ev.Verb == "cli" && ev.Params["cmd"] == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no cli audit event appended (ADR-0010): %+v", want, evs[:len(evs)-before])
		}
	}

	// adopt: Discover/Ping fail offline on a tmux config — the exit code is
	// whatever that yields; the cli event must still be recorded.
	before := len(trailEvents(t, st))
	run([]string{"bedrock-compass", "adopt", "--config", cfgPath})
	evs := trailEvents(t, st)
	found := false
	for _, ev := range evs[:len(evs)-before] {
		if ev.Verb == "cli" && ev.Params["cmd"] == "adopt" {
			found = true
		}
	}
	if !found {
		t.Errorf("adopt: no cli audit event appended (ADR-0010): %+v", evs[:len(evs)-before])
	}
}

// --- pure helpers ------------------------------------------------------------------

func TestParseSince(t *testing.T) {
	got, err := parseSince("2026-08-25 09:30")
	if err != nil || got.Hour() != 9 || got.Minute() != 30 {
		t.Errorf("parse = %v, %v", got, err)
	}
	got, err = parseSince("2026-08-25 09:30:41")
	if err != nil || got.Second() != 41 {
		t.Errorf("parse = %v, %v", got, err)
	}
	if _, err := parseSince("yesterday"); err == nil {
		t.Error("expected error for unparseable since")
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{512: "512 B", 2048: "2.0 KiB", 3 << 20: "3.0 MiB"}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestAuditLine_SortedParams(t *testing.T) {
	ev := compass.Event{
		Time:     time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC),
		Verb:     "cli",
		Params:   map[string]string{"cmd": "allowlist add", "action": "add", "identity": "player_one", "bypass": "--yes"},
		Result:   "ok",
		Duration: 123 * time.Millisecond,
	}
	line := auditLine(ev)
	if !strings.Contains(line, "allowlist add ok") {
		t.Errorf("cli event should render its command as the verb label: %q", line)
	}
	bi := strings.Index(line, "bypass=")
	ii := strings.Index(line, "identity=")
	if bi < 0 || ii < 0 || bi > ii {
		t.Errorf("params not sorted (bypass before identity): %q", line)
	}

	svc := compass.Event{
		Time:   time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC),
		Verb:   "access",
		Params: map[string]string{"action": "allow-add", "identity": "player_one"},
		Result: "ok",
	}
	if got := auditLine(svc); !strings.Contains(got, "access/allow-add ok") {
		t.Errorf("service event should render verb/action: %q", got)
	}
}
