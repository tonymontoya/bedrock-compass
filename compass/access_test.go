package compass

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests exercise the Access service (#7) offline against a temp server
// dir, covering the gate read/write, allowlist add/remove/reload round-trips,
// and the firewall graceful-degradation paths. Console behavior is scripted
// (newScriptedConsole); no rig or root required.

// newAccessFixture builds a temp server dir with a minimal server.properties
// and an optional initial allowlist.json, plus a resolved Config pointing at
// it. Returns the Access service (no console) and the dir path.
func newAccessFixture(t *testing.T, props string, allowlist []byte) (*Access, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.properties"), []byte(props), 0o644); err != nil {
		t.Fatalf("write server.properties: %v", err)
	}
	if allowlist != nil {
		if err := os.WriteFile(filepath.Join(dir, "allowlist.json"), allowlist, 0o644); err != nil {
			t.Fatalf("write allowlist.json: %v", err)
		}
	}
	cfg := &Config{ServerDir: dir, Port: 19132}
	return NewAccess(cfg, nil, nil), dir
}

func readAllowFile(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "allowlist.json"))
	if err != nil {
		t.Fatalf("read allowlist.json: %v", err)
	}
	return b
}

func readPropsLine(t *testing.T, dir string, key string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "server.properties"))
	if err != nil {
		t.Fatalf("read server.properties: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		v, ok := cutKey(strings.TrimSpace(line), key+"=")
		if ok {
			return v
		}
	}
	return ""
}

func TestAccess_Gate_ReadsPersistentValue(t *testing.T) {
	a, dir := newAccessFixture(t, "allow-list=true\nlevel-name=World\n", nil)
	g, err := a.Gate()
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if !g.Enabled {
		t.Error("gate = false, want true (server.properties allow-list=true)")
	}
	if g.Source == "" || !strings.HasSuffix(g.Source, "server.properties") {
		t.Errorf("gate source = %q; want the server.properties path", g.Source)
	}

	a2, _ := newAccessFixture(t, "allow-list=false\n", nil)
	g2, err := a2.Gate()
	if err != nil || g2.Enabled {
		t.Errorf("Gate on allow-list=false = %+v / %v; want disabled", g2, err)
	}

	a3, _ := newAccessFixture(t, "level-name=World\n", nil) // key absent
	g3, err := a3.Gate()
	if err != nil || g3.Enabled {
		t.Errorf("Gate with no allow-list key = %+v / %v; want disabled (BDS default)", g3, err)
	}

	a4, _ := newAccessFixture(t, "allow-list=TRUE\n", nil) // case-insensitive
	g4, err := a4.Gate()
	if err != nil || !g4.Enabled {
		t.Errorf("Gate on allow-list=TRUE = %+v / %v; want enabled (case-insensitive)", g4, err)
	}
	_ = dir
}

func TestAccess_Gate_MissingServerDir_Errors(t *testing.T) {
	a := NewAccess(&Config{}, nil, nil) // no ServerDir
	if _, err := a.Gate(); err == nil {
		t.Error("Gate with unresolved server dir must error")
	}
}

func TestAccess_SetGate_WritesAndPreservesOtherLines(t *testing.T) {
	a, dir := newAccessFixture(t, "allow-list=false\nlevel-name=World\nserver-port=19132\n", nil)
	if _, err := a.SetGate(context.Background(), true); err != nil {
		t.Fatalf("SetGate(true): %v", err)
	}
	if got := readPropsLine(t, dir, "allow-list"); got != "true" {
		t.Errorf("allow-list = %q; want true", got)
	}
	// Other lines preserved verbatim.
	b, _ := os.ReadFile(filepath.Join(dir, "server.properties"))
	for _, want := range []string{"level-name=World", "server-port=19132"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("server.properties lost line %q after SetGate:\n%s", want, b)
		}
	}

	// Toggle back off.
	if _, err := a.SetGate(context.Background(), false); err != nil {
		t.Fatalf("SetGate(false): %v", err)
	}
	if got := readPropsLine(t, dir, "allow-list"); got != "false" {
		t.Errorf("allow-list = %q; want false after toggle off", got)
	}
}

func TestAccess_SetGate_AppendsWhenKeyAbsent(t *testing.T) {
	a, dir := newAccessFixture(t, "level-name=World\n", nil)
	if _, err := a.SetGate(context.Background(), true); err != nil {
		t.Fatalf("SetGate(true): %v", err)
	}
	if got := readPropsLine(t, dir, "allow-list"); got != "true" {
		t.Errorf("allow-list = %q; want true (appended when absent)", got)
	}
}

func TestAccess_SetGate_LiveApplyViaConsole(t *testing.T) {
	a, _ := newAccessFixture(t, "allow-list=false\n", nil)
	c, sent := newScriptedConsole(t, map[string]string{
		"allowlist on":  "Turned on the allowlist.",
		"allowlist off": "Turned off the allowlist.",
	})
	a.Console = c

	if _, err := a.SetGate(context.Background(), true); err != nil {
		t.Fatalf("SetGate(true): %v", err)
	}
	foundOn := false
	for _, s := range *sent {
		if s == "allowlist on" {
			foundOn = true
		}
	}
	if !foundOn {
		t.Errorf("SetGate(true) did not send 'allowlist on'; sent: %v", *sent)
	}

	// A live-apply failure degrades to a note, not an error (the durable write
	// already succeeded). A console whose Write always errors makes Send fail.
	failing := &Console{
		Write:    func(ctx context.Context, line string) error { return os.ErrPermission },
		Baseline: func(ctx context.Context) (string, error) { return "tok", nil },
	}
	a.Console = failing
	detail, err := a.SetGate(context.Background(), false)
	if err != nil {
		t.Fatalf("SetGate with failing console must not error (durable write ok): %v", err)
	}
	if !strings.Contains(detail, "applies on next start") {
		t.Errorf("detail = %q; want the degraded 'applies on next start' note", detail)
	}
}

func TestAccess_List_ParsesNullAndEntries(t *testing.T) {
	a, _ := newAccessFixture(t, "", []byte("null"))
	e, err := a.List()
	if err != nil || len(e) != 0 {
		t.Errorf("List on null = %v / %v; want empty", e, err)
	}

	a2, _ := newAccessFixture(t, "", []byte(`[{"ignoresPlayerLimit":false,"name":"Alice"},{"ignoresPlayerLimit":false,"name":"Bob"}]`))
	e2, err := a2.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(e2) != 2 || e2[0].Name != "Alice" || e2[1].Name != "Bob" {
		t.Errorf("List = %+v; want Alice, Bob", e2)
	}

	a3, _ := newAccessFixture(t, "", nil) // no file at all
	e3, err := a3.List()
	if err != nil || len(e3) != 0 {
		t.Errorf("List with missing file = %v / %v; want empty (no error)", e3, err)
	}
}

func TestAccess_Add_WritesAndVerifies(t *testing.T) {
	a, dir := newAccessFixture(t, "", []byte("null"))
	c, _ := newScriptedConsole(t, map[string]string{
		"allowlist reload": "Allowlist reloaded from file.",
		"allowlist list":   `###* {"command":"allowlist","result":[{"ignoresPlayerLimit":false,"name":"Alice"}]}`,
	})
	a.Console = c

	detail, err := a.Add(context.Background(), "Alice")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !strings.Contains(detail, "reloaded") {
		t.Errorf("detail = %q; want the reloaded note", detail)
	}
	got := string(readAllowFile(t, dir))
	if !strings.Contains(got, `"name":"Alice"`) || strings.Contains(got, "null") {
		t.Errorf("allowlist.json after Add = %s; want an Alice entry (not null)", got)
	}

	// Duplicate rejected before any write.
	if _, err := a.Add(context.Background(), "alice"); err == nil {
		t.Error("Add of a duplicate (case-insensitive) must fail")
	} else if !strings.Contains(err.Error(), "already on the allowlist") {
		t.Errorf("duplicate Add error = %q; want 'already on the allowlist'", err)
	}

	// Empty identity rejected.
	if _, err := a.Add(context.Background(), "  "); err == nil {
		t.Error("Add of empty identity must fail")
	}
}

func TestAccess_Add_NoConsole_DegradesToNote(t *testing.T) {
	a, dir := newAccessFixture(t, "", []byte("null")) // no console
	detail, err := a.Add(context.Background(), "Alice")
	if err != nil {
		t.Fatalf("Add (no console): %v", err)
	}
	if !strings.Contains(detail, "applies on next start") {
		t.Errorf("detail = %q; want the no-console note", detail)
	}
	got := string(readAllowFile(t, dir))
	if !strings.Contains(got, `"name":"Alice"`) {
		t.Errorf("allowlist.json after Add (no console) = %s; want Alice entry", got)
	}
}

func TestAccess_Add_ServerDoesNotReflect_Fails(t *testing.T) {
	a, _ := newAccessFixture(t, "", []byte("null"))
	c, _ := newScriptedConsole(t, map[string]string{
		"allowlist reload": "Allowlist reloaded from file.",
		"allowlist list":   `###* {"command":"allowlist","result":null}`, // server shows nothing
	})
	a.Console = c
	if _, err := a.Add(context.Background(), "Alice"); err == nil {
		t.Fatal("Add must fail when the server does not reflect the entry")
	} else if !strings.Contains(err.Error(), "did not reflect") {
		t.Errorf("error = %q; want 'did not reflect'", err)
	}
}

func TestAccess_Remove_WritesAndVerifies(t *testing.T) {
	a, dir := newAccessFixture(t, "", []byte(`[{"ignoresPlayerLimit":false,"name":"Alice"},{"ignoresPlayerLimit":false,"name":"Bob"}]`))
	c, _ := newScriptedConsole(t, map[string]string{
		"allowlist reload": "Allowlist reloaded from file.",
		"allowlist list":   `###* {"command":"allowlist","result":[{"ignoresPlayerLimit":false,"name":"Bob"}]}`,
	})
	a.Console = c

	detail, err := a.Remove(context.Background(), "Alice")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !strings.Contains(detail, "reloaded") {
		t.Errorf("detail = %q; want the reloaded note", detail)
	}
	got := string(readAllowFile(t, dir))
	if strings.Contains(got, "Alice") {
		t.Errorf("allowlist.json after Remove still has Alice: %s", got)
	}
	if !strings.Contains(got, `"name":"Bob"`) {
		t.Errorf("allowlist.json lost Bob after removing Alice: %s", got)
	}

	// Removing an absent identity is rejected.
	if _, err := a.Remove(context.Background(), "Zed"); err == nil {
		t.Error("Remove of an absent identity must fail")
	} else if !strings.Contains(err.Error(), "not on the allowlist") {
		t.Errorf("absent Remove error = %q; want 'not on the allowlist'", err)
	}
}

func TestAccess_Remove_EmptyList_WritesNull(t *testing.T) {
	a, dir := newAccessFixture(t, "", []byte(`[{"ignoresPlayerLimit":false,"name":"Alice"}]`))
	c, _ := newScriptedConsole(t, map[string]string{
		"allowlist reload": "Allowlist reloaded from file.",
		"allowlist list":   `###* {"command":"allowlist","result":null}`,
	})
	a.Console = c
	if _, err := a.Remove(context.Background(), "Alice"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got := strings.TrimSpace(string(readAllowFile(t, dir)))
	if got != "null" {
		t.Errorf("allowlist.json after removing the last entry = %q; want literal null (BDS format)", got)
	}
}

func TestAccess_Reload_NoConsole_CountsEntries(t *testing.T) {
	a, _ := newAccessFixture(t, "", []byte(`[{"ignoresPlayerLimit":false,"name":"Alice"},{"ignoresPlayerLimit":false,"name":"Bob"}]`))
	detail, err := a.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !strings.Contains(detail, "2 entries") || !strings.Contains(detail, "applies on next start") {
		t.Errorf("detail = %q; want '2 entries ... applies on next start'", detail)
	}
}

func TestAccess_Reload_WithConsole(t *testing.T) {
	a, _ := newAccessFixture(t, "", []byte(`[{"ignoresPlayerLimit":false,"name":"Alice"}]`))
	c, sent := newScriptedConsole(t, map[string]string{
		"allowlist reload": "Allowlist reloaded from file.",
		"allowlist list":   `###* {"command":"allowlist","result":[{"ignoresPlayerLimit":false,"name":"Alice"}]}`,
	})
	a.Console = c
	detail, err := a.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	sentReload := false
	for _, s := range *sent {
		if s == "allowlist reload" {
			sentReload = true
		}
	}
	if !sentReload {
		t.Errorf("Reload did not send 'allowlist reload'; sent: %v", *sent)
	}
	if !strings.Contains(detail, "1 entry") || !strings.Contains(detail, "reloaded") {
		t.Errorf("detail = %q; want '1 entry ... reloaded'", detail)
	}
}

func TestAccess_BlockIP_Unprivileged_ShowsCommand(t *testing.T) {
	a, _ := newAccessFixture(t, "", nil)
	a.euid = func() int { return 1000 } // non-root
	res, err := a.BlockIP(context.Background(), "203.0.113.7")
	if err != nil {
		t.Fatalf("BlockIP: %v", err)
	}
	if res.Applied {
		t.Error("BlockIP as non-root must not apply (graceful degradation)")
	}
	want := "ufw deny from 203.0.113.7 to any port 19132 proto udp"
	if res.Command != want {
		t.Errorf("command = %q; want %q", res.Command, want)
	}
	if !strings.Contains(res.Reason, "root") {
		t.Errorf("reason = %q; want a root/permission note", res.Reason)
	}
}

func TestAccess_UnblockIP_Unprivileged_ShowsCommand(t *testing.T) {
	a, _ := newAccessFixture(t, "", nil)
	a.euid = func() int { return 1000 }
	res, err := a.UnblockIP(context.Background(), "203.0.113.7")
	if err != nil {
		t.Fatalf("UnblockIP: %v", err)
	}
	want := "ufw delete deny from 203.0.113.7 to any port 19132 proto udp"
	if res.Command != want {
		t.Errorf("command = %q; want %q", res.Command, want)
	}
	if res.Applied || !strings.Contains(res.Reason, "root") {
		t.Errorf("unblock as non-root: applied=%v reason=%q; want not-applied + root note", res.Applied, res.Reason)
	}
}

func TestAccess_BlockIP_InvalidIP_Errors(t *testing.T) {
	a, _ := newAccessFixture(t, "", nil)
	if _, err := a.BlockIP(context.Background(), "not-an-ip"); err == nil {
		t.Error("BlockIP of an invalid IP must fail")
	} else if !strings.Contains(err.Error(), "valid IP") {
		t.Errorf("error = %q; want 'valid IP'", err)
	}
}

func TestAccess_BlockIP_Privileged_Applies(t *testing.T) {
	a, _ := newAccessFixture(t, "", nil)
	a.euid = func() int { return 0 } // root
	var ran string
	a.run = func(ctx context.Context, cmd string) (string, error) {
		ran = cmd
		return "Added user rule", nil
	}
	res, err := a.BlockIP(context.Background(), "203.0.113.7")
	if err != nil {
		t.Fatalf("BlockIP: %v", err)
	}
	if !res.Applied {
		t.Error("BlockIP as root must apply the command")
	}
	want := "ufw deny from 203.0.113.7 to any port 19132 proto udp"
	if ran != want {
		t.Errorf("executed = %q; want %q", ran, want)
	}
}

func TestAccess_BlockIP_Privileged_FailureDegrades(t *testing.T) {
	a, _ := newAccessFixture(t, "", nil)
	a.euid = func() int { return 0 }
	a.run = func(ctx context.Context, cmd string) (string, error) {
		return "ERROR: problem running", os.ErrPermission
	}
	res, err := a.BlockIP(context.Background(), "203.0.113.7")
	if err != nil {
		t.Fatalf("BlockIP failure must degrade to a result, not an error: %v", err)
	}
	if res.Applied {
		t.Error("a failed firewall command must report Applied=false")
	}
	if !strings.Contains(res.Reason, "command failed") {
		t.Errorf("reason = %q; want 'command failed'", res.Reason)
	}
}

func TestAccess_Firewall_DefaultPortWhenUnset(t *testing.T) {
	a := NewAccess(&Config{ServerDir: t.TempDir()}, nil, nil) // Port 0
	a.euid = func() int { return 1000 }
	res, err := a.BlockIP(context.Background(), "203.0.113.7")
	if err != nil {
		t.Fatalf("BlockIP: %v", err)
	}
	want := "ufw deny from 203.0.113.7 to any port 19132 proto udp" // default BDS port
	if res.Command != want {
		t.Errorf("command = %q; want the default-port command %q", res.Command, want)
	}
}

func TestAccess_Audit_RecordsMutations(t *testing.T) {
	dir := t.TempDir()
	trail, err := NewTrail(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("trail: %v", err)
	}
	a, _ := newAccessFixture(t, "", []byte("null"))
	a.Trail = trail

	ctx := context.Background()
	if _, err := a.Add(ctx, "Alice"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := a.Remove(ctx, "Alice"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	evs, rerr := trail.Recent(10)
	if rerr != nil {
		t.Fatalf("recent: %v", rerr)
	}
	var sawAdd, sawRemove bool
	for _, ev := range evs {
		if ev.Verb != "access" {
			continue
		}
		switch ev.Params["action"] {
		case "allow-add":
			sawAdd = true
		case "allow-remove":
			sawRemove = true
		}
	}
	if !sawAdd || !sawRemove {
		t.Errorf("audit trail missing access events: add=%v remove=%v; got %+v", sawAdd, sawRemove, evs)
	}
}

func TestAccess_Audit_RecordsFailures(t *testing.T) {
	dir := t.TempDir()
	trail, err := NewTrail(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("trail: %v", err)
	}
	a, _ := newAccessFixture(t, "", []byte(`[{"ignoresPlayerLimit":false,"name":"Alice"}]`))
	a.Trail = trail

	// Duplicate add -> failure recorded.
	if _, err := a.Add(context.Background(), "Alice"); err == nil {
		t.Fatal("duplicate Add must fail")
	}
	evs, _ := trail.Recent(5)
	found := false
	for _, ev := range evs {
		if ev.Verb == "access" && ev.Params["action"] == "allow-add" && strings.HasPrefix(ev.Result, "error:") {
			found = true
		}
	}
	if !found {
		t.Errorf("audit trail missing the failed add event: %+v", evs)
	}
}
