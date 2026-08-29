//go:build integration

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests drive the headless verbs (#15, ADR-0010) end-to-end against the
// scratch rig: they exec the shipped bedrock-compass binary with a config that
// points at the rig's unit/FIFO/server dir and assert on exit codes, output,
// files, and the audit trail. They self-skip when the rig is not active and
// are opt-in: -tags integration, shipped via make integ RIG_HOST=<target>.

func verbBin() string {
	if v := os.Getenv("COMPASS_TEST_BIN"); v != "" {
		return v
	}
	return "/tmp/bedrock-compass"
}

func verbUnit() string {
	if v := os.Getenv("COMPASS_TEST_UNIT"); v != "" {
		return v
	}
	return "bedrock-compass-test"
}

func verbFIFO() string {
	if v := os.Getenv("COMPASS_TEST_FIFO"); v != "" {
		return v
	}
	return "/run/bedrock-compass-test.fifo"
}

func verbServerDir() string {
	if v := os.Getenv("COMPASS_TEST_SERVER_DIR"); v != "" {
		return v
	}
	return "/opt/test-bedrock"
}

// skipIfRigDown skips when the scratch rig unit is not active, then settles:
// a freshly started server reports "active" before its console stdin is ready,
// so probe with a read-only verb until it answers (mirrors the compass suite's
// settleConsole). A missing binary is also a skip — make integ ships it.
func skipIfRigDown(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(verbBin()); err != nil {
		t.Skipf("binary %s not present on this host (make integ ships it)", verbBin())
	}
	out, err := exec.Command("systemctl", "is-active", verbUnit()).Output()
	if err != nil || strings.TrimSpace(string(out)) != "active" {
		t.Skipf("scratch rig is not active (unit %s); start it to run these tests", verbUnit())
	}
	cfg := verbConfig(t)
	deadline := time.Now().Add(30 * time.Second)
	for {
		code, out, _ := runVerb(t, "roster", "--config", cfg)
		if code == 0 && strings.Contains(out, "players online") {
			return // console is live and answering
		}
		if time.Now().After(deadline) {
			t.Skipf("rig is active but the console is not answering within 30s (last exit %d: %q); start the server and retry", code, out)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// verbConfig writes a config pointing at the scratch rig with isolated backup
// and audit paths, so no test state leaks into the operator's real dirs.
func verbConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := strings.Join([]string{
		"server-dir: " + verbServerDir(),
		"supervisor: systemd",
		"service: " + verbUnit(),
		"console-fif: " + verbFIFO(),
		"backup-dir: " + filepath.Join(dir, "backups"),
		"audit-file: " + filepath.Join(dir, "audit.jsonl"),
	}, "\n") + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write rig config: %v", err)
	}
	return cfgPath
}

// runVerb execs the shipped binary with a verb and returns (exitCode, stdout,
// stderr). A missing binary is a skip, not a failure — make integ ships it.
func runVerb(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	bin := verbBin()
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("binary %s not present on this host (make integ ships it)", bin)
	}
	cmd := exec.Command(bin, args...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("exec %s: %v", bin, err)
	}
	return code, outBuf.String(), errBuf.String()
}

// TestIntegration_Verbs_HelpListsAllVerbs needs no rig: the generated help
// must name every verb so an operator can discover them.
func TestIntegration_Verbs_HelpListsAllVerbs(t *testing.T) {
	code, out, _ := runVerb(t, "--help")
	if code != 0 {
		t.Fatalf("--help exit = %d, want 0", code)
	}
	for _, verb := range []string{"roster", "logs", "allowlist", "restore", "audit", "backup-now", "status"} {
		if !strings.Contains(out, verb) {
			t.Errorf("help output missing verb %q:\n%s", verb, out)
		}
	}
}

// TestIntegration_Verbs_Roster runs the live console round-trip through the
// binary in both text and JSON form.
func TestIntegration_Verbs_Roster(t *testing.T) {
	skipIfRigDown(t)
	cfg := verbConfig(t)

	code, out, errOut := runVerb(t, "roster", "--config", cfg)
	if code != 0 {
		t.Fatalf("roster exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	if !strings.Contains(out, "players online") {
		t.Errorf("roster output missing the roster line:\n%s", out)
	}

	code, jout, errOut := runVerb(t, "roster", "--json", "--config", cfg)
	if code != 0 {
		t.Fatalf("roster --json exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	var rj map[string]any
	if err := json.Unmarshal([]byte(jout), &rj); err != nil {
		t.Fatalf("roster --json is not valid JSON: %v\n%s", err, jout)
	}
	if _, ok := rj["count"]; !ok {
		t.Errorf("roster JSON missing count field:\n%s", jout)
	}
}

// TestIntegration_Verbs_Logs reads the rig unit's journal through the binary.
func TestIntegration_Verbs_Logs(t *testing.T) {
	skipIfRigDown(t)
	cfg := verbConfig(t)

	code, out, errOut := runVerb(t, "logs", "--lines", "20", "--config", cfg)
	if code != 0 {
		t.Fatalf("logs exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	if strings.TrimSpace(out) == "" {
		t.Errorf("logs printed nothing; the rig journal should have lines")
	}

	code, jout, errOut := runVerb(t, "logs", "--json", "--lines", "5", "--config", cfg)
	if code != 0 {
		t.Fatalf("logs --json exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(jout), &arr); err != nil {
		t.Fatalf("logs --json is not a JSON array: %v\n%s", err, jout)
	}

	// A bad level is a usage error (exit 2), decided before any wiring.
	code, _, _ = runVerb(t, "logs", "--level", "LOUD", "--config", cfg)
	if code != 2 {
		t.Errorf("logs --level LOUD exit = %d, want 2", code)
	}
}

// TestIntegration_Verbs_AllowlistGate exercises the ADR-0010 gate end-to-end:
// refused without --yes (exit 3, nothing written), executed with it (file +
// server agree), then removed again. The audit trail must carry both the
// service events and the CLI invocation events with the bypass noted.
func TestIntegration_Verbs_AllowlistGate(t *testing.T) {
	skipIfRigDown(t)
	cfg := verbConfig(t)
	tag := "compass-verb-integ"

	allowFile := filepath.Join(verbServerDir(), "allowlist.json")
	readAllow := func() string {
		data, err := os.ReadFile(allowFile)
		if err != nil {
			return ""
		}
		return string(data)
	}

	// Idempotent start: drop a stale entry from an earlier run.
	runVerb(t, "allowlist", "remove", tag, "--yes", "--config", cfg)

	// 1. Without --yes: refused, exit 3, file untouched.
	before := readAllow()
	code, _, errOut := runVerb(t, "allowlist", "add", tag, "--config", cfg)
	if code != 3 {
		t.Fatalf("allowlist add without --yes exit = %d, want 3 (stderr: %s)", code, errOut)
	}
	if !strings.Contains(errOut, "refusing without --yes") {
		t.Errorf("refusal message missing:\n%s", errOut)
	}
	if readAllow() != before {
		t.Errorf("allowlist.json changed despite the refusal")
	}

	// 2. With --yes: applied to file and live server.
	code, out, errOut := runVerb(t, "allowlist", "add", tag, "--yes", "--config", cfg)
	if code != 0 {
		t.Fatalf("allowlist add --yes exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	if !strings.Contains(readAllow(), `"name":"`+tag+`"`) {
		t.Errorf("allowlist.json missing the added entry:\n%s", readAllow())
	}
	t.Logf("add output: %s", strings.TrimSpace(out))

	// 3. Remove it again; file must be clean afterwards.
	code, _, errOut = runVerb(t, "allowlist", "remove", tag, "--yes", "--config", cfg)
	if code != 0 {
		t.Fatalf("allowlist remove --yes exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	if strings.Contains(readAllow(), tag) {
		t.Errorf("allowlist.json still holds the removed entry:\n%s", readAllow())
	}

	// 4. The audit trail must carry the service events and the CLI events
	// (with the bypass noted on the confirmed mutation).
	data, err := os.ReadFile(filepath.Dir(cfg) + "/audit.jsonl")
	if err != nil {
		t.Fatalf("read audit trail: %v", err)
	}
	var sawAdd, sawRemove, sawBypass bool
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev struct {
			Verb   string            `json:"verb"`
			Params map[string]string `json:"params"`
			Result string            `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("audit line is not valid JSON: %v\n%s", err, line)
		}
		switch {
		case ev.Verb == "access" && ev.Params["action"] == "allow-add":
			sawAdd = true
		case ev.Verb == "access" && ev.Params["action"] == "allow-remove":
			sawRemove = true
		case ev.Verb == "cli" && ev.Params["cmd"] == "allowlist add" && ev.Params["bypass"] == "--yes":
			sawBypass = true
		}
	}
	if !sawAdd || !sawRemove {
		t.Errorf("audit trail missing service events (add=%v remove=%v)", sawAdd, sawRemove)
	}
	if !sawBypass {
		t.Errorf("audit trail missing the CLI event with bypass=--yes")
	}
}

// TestIntegration_Verbs_RestoreGate proves a refused restore never stops the
// server (exit 3), and an executed restore of a nonexistent backup fails at
// verification — before any stop — leaving the rig running.
func TestIntegration_Verbs_RestoreGate(t *testing.T) {
	skipIfRigDown(t)
	cfg := verbConfig(t)

	code, _, errOut := runVerb(t, "restore", "19700101-000000", "--config", cfg)
	if code != 3 {
		t.Fatalf("restore without --yes exit = %d, want 3 (stderr: %s)", code, errOut)
	}
	if !strings.Contains(errOut, "refusing without --yes") {
		t.Errorf("refusal message missing:\n%s", errOut)
	}

	code, _, errOut = runVerb(t, "restore", "19700101-000000", "--yes", "--config", cfg)
	if code != 1 {
		t.Fatalf("restore of a nonexistent backup exit = %d, want 1 (stderr: %s)", code, errOut)
	}

	// The rig must still be up and answering.
	out, err := exec.Command("systemctl", "is-active", verbUnit()).Output()
	if err != nil || strings.TrimSpace(string(out)) != "active" {
		t.Fatalf("rig unit not active after restore attempts: %q (%v)", out, err)
	}
	code2, _, _ := runVerb(t, "roster", "--config", cfg)
	if code2 != 0 {
		t.Errorf("console not answering after restore attempts (roster exit = %d)", code2)
	}
}

// TestIntegration_Verbs_Audit prints the trail most-recent-first and must show
// this test's own prior invocation.
func TestIntegration_Verbs_Audit(t *testing.T) {
	skipIfRigDown(t)
	cfg := verbConfig(t)

	if code, _, errOut := runVerb(t, "roster", "--config", cfg); code != 0 {
		t.Fatalf("setup roster exit = %d (stderr: %s)", code, errOut)
	}

	code, out, errOut := runVerb(t, "audit", "--config", cfg)
	if code != 0 {
		t.Fatalf("audit exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	if !strings.Contains(out, "roster") {
		t.Errorf("audit output missing the roster invocation:\n%s", out)
	}

	code, jout, _ := runVerb(t, "audit", "--json", "--config", cfg)
	if code != 0 {
		t.Fatalf("audit --json exit = %d, want 0", code)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(jout), &arr); err != nil {
		t.Fatalf("audit --json is not a JSON array: %v\n%s", err, jout)
	}
	if len(arr) == 0 {
		t.Errorf("audit --json printed an empty trail")
	}
}

// TestIntegration_Verbs_BackupNowGate proves the scheduled-backup path's flag
// requirement at the CLI level: without --yes the verb refuses (exit 3) and
// takes no backup. The live/stopped execution itself is covered by the
// compass suite (TestIntegration_Backup_Live / Restore_RoundTrip).
func TestIntegration_Verbs_BackupNowGate(t *testing.T) {
	skipIfRigDown(t)
	cfg := verbConfig(t)

	code, _, errOut := runVerb(t, "backup-now", "--config", cfg)
	if code != 3 {
		t.Fatalf("backup-now without --yes exit = %d, want 3 (stderr: %s)", code, errOut)
	}
	if !strings.Contains(errOut, "refusing without --yes") {
		t.Errorf("refusal message missing:\n%s", errOut)
	}

	code, _, _ = runVerb(t, "backup-now", "--mode", "bogus", "--config", cfg)
	if code != 3 {
		t.Errorf("backup-now with a bad mode still hits the gate first (exit %d), want 3", code)
	}
}

// TestIntegration_Verbs_StatusJSON prints the lifecycle status as JSON.
func TestIntegration_Verbs_StatusJSON(t *testing.T) {
	skipIfRigDown(t)
	cfg := verbConfig(t)

	code, out, errOut := runVerb(t, "status", "--json", "--config", cfg)
	if code != 0 {
		t.Fatalf("status --json exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("status --json is not valid JSON: %v\n%s", err, out)
	}
	state, _ := st["State"].(string)
	if state == "" {
		t.Errorf("status JSON missing a State value:\n%s", out)
	}

	code2, tout, errOut := runVerb(t, "status", "--config", cfg)
	if code2 != 0 {
		t.Fatalf("status exit = %d, want 0 (stderr: %s)", code2, errOut)
	}
	if !strings.Contains(tout, "state") {
		t.Errorf("status text output missing the state line:\n%s", tout)
	}
}
