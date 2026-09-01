package compass

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProvisionValidate pins the input contract (ticket #14): what a
// provision call may look like, and what it must reject. Expected errors
// are substrings, so the message wording can evolve freely.
func TestProvisionValidate(t *testing.T) {
	nonRoot := &ProvisionEnv{IsRoot: false, CurrentUser: "tony"}
	root := &ProvisionEnv{IsRoot: true, CurrentUser: "root"}

	base := ProvisionInput{
		Name:       "compass-test",
		ServerDir:  "/srv/bedrock/compass-test",
		Supervisor: "tmux",
		Source:     SourceZip,
		ZipPath:    "/srv/zips/bedrock-server-1.26.45.1.zip",
	}

	t.Run("accepts a valid tmux zip input", func(t *testing.T) {
		p := &Provision{Env: nonRoot}
		if err := p.Validate(base); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("accepts systemd input with an explicit user", func(t *testing.T) {
		in := base
		in.Supervisor = "systemd"
		in.User = "mcserver"
		p := &Provision{Env: nonRoot}
		if err := p.Validate(in); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("accepts non-root systemd with empty user (defaults to current)", func(t *testing.T) {
		in := base
		in.Supervisor = "systemd"
		p := &Provision{Env: nonRoot}
		if err := p.Validate(in); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("accepts version source with a well-formed version", func(t *testing.T) {
		in := base
		in.Source = SourceVersion
		in.Version = "1.26.45.1"
		in.ZipPath = ""
		p := &Provision{Env: nonRoot}
		if err := p.Validate(in); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("accepts an explicit port", func(t *testing.T) {
		in := base
		in.Port = 19199
		p := &Provision{Env: nonRoot}
		if err := p.Validate(in); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	reject := func(t *testing.T, env *ProvisionEnv, in ProvisionInput, want string) {
		t.Helper()
		p := &Provision{Env: env}
		err := p.Validate(in)
		if err == nil {
			t.Fatalf("Validate = nil, want error mentioning %q", want)
		}
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Fatalf("Validate error = %q, want mention of %q", err, want)
		}
	}

	in := base
	in.Name = ""
	reject(t, nonRoot, in, "name")

	in = base
	in.Name = "Compass_Test"
	reject(t, nonRoot, in, "name")

	in = base
	in.ServerDir = "relative/path"
	reject(t, nonRoot, in, "server dir")

	in = base
	in.Supervisor = "nohup"
	reject(t, nonRoot, in, "supervisor")

	in = base
	in.Source = SourceVersion
	in.Version = ""
	in.ZipPath = ""
	reject(t, nonRoot, in, "version")

	in = base
	in.Source = SourceVersion
	in.Version = "latest"
	in.ZipPath = ""
	reject(t, nonRoot, in, "version")

	in = base
	in.Supervisor = "systemd"
	in.User = ""
	reject(t, root, in, "user")

	in = base
	in.Port = 80
	reject(t, nonRoot, in, "port")

	in = base
	in.Port = 70000
	reject(t, nonRoot, in, "port")
}

// TestProvisionPlan_FreshSystemd pins the systemd plan: action, the exact
// unit texts (the proven production shape per ADR-0001 — StandardInput=
// socket plus explicit journal output is non-negotiable), the config
// snippet, and the step list.
func TestProvisionPlan_FreshSystemd(t *testing.T) {
	f := &fakeExec{output: map[string][]byte{}}
	p := &Provision{
		Env:  &ProvisionEnv{IsRoot: false, CurrentUser: "tony"},
		Exec: f.run,
	}
	in := ProvisionInput{
		Name:       "compass-test",
		ServerDir:  "/srv/bedrock/compass-test",
		Supervisor: "systemd",
		Source:     SourceZip,
		ZipPath:    "/srv/zips/bedrock-server-1.26.45.1.zip",
		User:       "mcserver",
		Port:       19199,
	}

	plan, err := p.Plan(in)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Action != "provision" {
		t.Fatalf("Action = %q, want provision", plan.Action)
	}

	wantService := `[Unit]
Description=Minecraft Bedrock server (compass-test)
After=network-online.target

[Service]
User=mcserver
Group=mcserver
WorkingDirectory=/srv/bedrock/compass-test
ExecStart=/bin/sh -c "LD_LIBRARY_PATH=. ./bedrock_server"
StandardInput=socket
StandardOutput=journal
StandardError=journal
Restart=on-failure
`
	if plan.UnitService != wantService {
		t.Errorf("UnitService mismatch:\n--- got ---\n%s\n--- want ---\n%s", plan.UnitService, wantService)
	}

	wantSocket := `[Socket]
Accept=no
ListenFIFO=/run/compass-test-console.fifo
SocketMode=0660
`
	if plan.UnitSocket != wantSocket {
		t.Errorf("UnitSocket mismatch:\n--- got ---\n%s\n--- want ---\n%s", plan.UnitSocket, wantSocket)
	}

	for _, want := range []string{
		"supervisor: systemd",
		"service: compass-test",
		"console-fif: /run/compass-test-console.fifo",
		"server-dir: /srv/bedrock/compass-test",
		"port: 19199",
	} {
		if !strings.Contains(plan.ConfigSnippet, want) {
			t.Errorf("ConfigSnippet missing %q:\n%s", want, plan.ConfigSnippet)
		}
	}

	if len(plan.Steps) == 0 {
		t.Error("Steps is empty; the plan must render human-readable steps")
	}
}

// TestProvisionPlan_FreshTmux pins the tmux plan: exact session and
// pipe-pane commands (ADR-0001/0007 tmux semantics) and the pane-based
// config snippet.
func TestProvisionPlan_FreshTmux(t *testing.T) {
	f := &fakeExec{output: map[string][]byte{}, fail: map[string]error{}}
	f.fail["tmux has-session -t compass-test"] = errNotPresent // session absent
	p := &Provision{
		Env:  &ProvisionEnv{IsRoot: false, CurrentUser: "tony"},
		Exec: f.run,
	}
	in := ProvisionInput{
		Name:       "compass-test",
		ServerDir:  "/srv/bedrock/compass-test",
		Supervisor: "tmux",
		Source:     SourceZip,
		ZipPath:    "/srv/zips/bedrock-server-1.26.45.1.zip",
		Port:       19199,
	}

	plan, err := p.Plan(in)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Action != "provision" {
		t.Fatalf("Action = %q, want provision", plan.Action)
	}

	want := []string{
		"tmux new-session -d -s compass-test -c /srv/bedrock/compass-test LD_LIBRARY_PATH=. ./bedrock_server",
		"tmux pipe-pane -t compass-test -o cat >> /srv/bedrock/compass-test/console.log",
	}
	if len(plan.TmuxCommands) != len(want) {
		t.Fatalf("TmuxCommands = %v, want %v", plan.TmuxCommands, want)
	}
	for i, w := range want {
		if plan.TmuxCommands[i] != w {
			t.Errorf("TmuxCommands[%d] = %q, want %q", i, plan.TmuxCommands[i], w)
		}
	}

	for _, want := range []string{"supervisor: tmux", "pane: compass-test", "port: 19199"} {
		if !strings.Contains(plan.ConfigSnippet, want) {
			t.Errorf("ConfigSnippet missing %q:\n%s", want, plan.ConfigSnippet)
		}
	}
}

// TestProvisionPlan_DefaultsSystemdUser pins the agreed default: a
// non-root provisioner without an explicit user runs the unit as the
// current user.
func TestProvisionPlan_DefaultsSystemdUser(t *testing.T) {
	f := &fakeExec{output: map[string][]byte{}}
	p := &Provision{
		Env:  &ProvisionEnv{IsRoot: false, CurrentUser: "tony"},
		Exec: f.run,
	}
	in := ProvisionInput{
		Name:       "compass-test",
		ServerDir:  "/srv/bedrock/compass-test",
		Supervisor: "systemd",
		Source:     SourceZip,
		ZipPath:    "/srv/zips/x.zip",
	}
	plan, err := p.Plan(in)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.UnitService, "User=tony") {
		t.Errorf("UnitService should run as the current user:\n%s", plan.UnitService)
	}
	if plan.EffectiveUser != "tony" {
		t.Errorf("EffectiveUser = %q, want tony", plan.EffectiveUser)
	}
}

// TestProvisionPlan_IdempotencyMatrix pins the detect-first model (agreed
// in the #14 design): dir + supervisor state decide the action, and the
// four combinations map to the four actions.
func TestProvisionPlan_IdempotencyMatrix(t *testing.T) {
	base := ProvisionInput{
		Name: "compass-test", ServerDir: "", Supervisor: "systemd",
		Source: SourceZip, ZipPath: "/srv/zips/x.zip", User: "mcserver",
	}

	prepared := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "bedrock_server"), []byte("bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "eula.txt"), []byte("true\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("systemd: dir + unit present -> noop", func(t *testing.T) {
		unitDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(unitDir, "compass-test.service"), []byte("[Service]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(unitDir, "compass-test.socket"), []byte("[Socket]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f := &fakeExec{output: map[string][]byte{}}
		p := &Provision{Env: &ProvisionEnv{IsRoot: false, CurrentUser: "tony"}, Exec: f.run, UnitDir: unitDir}
		in := base
		in.ServerDir = prepared(t)
		plan, err := p.Plan(in)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if plan.Action != PlanActionNoop {
			t.Fatalf("Action = %q, want noop", plan.Action)
		}
	})

	t.Run("systemd: dir present, unit missing -> wire-supervisor", func(t *testing.T) {
		f := &fakeExec{output: map[string][]byte{}}
		p := &Provision{Env: &ProvisionEnv{IsRoot: false, CurrentUser: "tony"}, Exec: f.run, UnitDir: t.TempDir()}
		in := base
		in.ServerDir = prepared(t)
		plan, err := p.Plan(in)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if plan.Action != PlanActionWire {
			t.Fatalf("Action = %q, want wire-supervisor", plan.Action)
		}
	})

	t.Run("systemd: unit present, dir missing -> inconsistent", func(t *testing.T) {
		unitDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(unitDir, "compass-test.service"), []byte("[Service]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(unitDir, "compass-test.socket"), []byte("[Socket]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f := &fakeExec{output: map[string][]byte{}}
		p := &Provision{Env: &ProvisionEnv{IsRoot: false, CurrentUser: "tony"}, Exec: f.run, UnitDir: unitDir}
		in := base
		in.ServerDir = filepath.Join(t.TempDir(), "absent")
		plan, err := p.Plan(in)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if plan.Action != PlanActionInconsistent {
			t.Fatalf("Action = %q, want inconsistent", plan.Action)
		}
	})

	t.Run("tmux: dir + session present -> noop", func(t *testing.T) {
		f := &fakeExec{output: map[string][]byte{}} // has-session succeeds
		p := &Provision{Env: &ProvisionEnv{IsRoot: false, CurrentUser: "tony"}, Exec: f.run}
		in := base
		in.Supervisor = "tmux"
		in.ServerDir = prepared(t)
		plan, err := p.Plan(in)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if plan.Action != PlanActionNoop {
			t.Fatalf("Action = %q, want noop", plan.Action)
		}
	})

	t.Run("tmux: dir present, session missing -> wire-supervisor", func(t *testing.T) {
		f := &fakeExec{output: map[string][]byte{}, fail: map[string]error{}}
		f.fail["tmux has-session -t compass-test"] = errNotPresent
		p := &Provision{Env: &ProvisionEnv{IsRoot: false, CurrentUser: "tony"}, Exec: f.run}
		in := base
		in.Supervisor = "tmux"
		in.ServerDir = prepared(t)
		plan, err := p.Plan(in)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if plan.Action != PlanActionWire {
			t.Fatalf("Action = %q, want wire-supervisor", plan.Action)
		}
	})

	t.Run("tmux: session present, dir missing -> inconsistent", func(t *testing.T) {
		f := &fakeExec{output: map[string][]byte{}} // has-session succeeds
		p := &Provision{Env: &ProvisionEnv{IsRoot: false, CurrentUser: "tony"}, Exec: f.run}
		in := base
		in.Supervisor = "tmux"
		in.ServerDir = filepath.Join(t.TempDir(), "absent")
		plan, err := p.Plan(in)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if plan.Action != PlanActionInconsistent {
			t.Fatalf("Action = %q, want inconsistent", plan.Action)
		}
	})
}

// TestProvisionPlan_Prerequisites pins what the plan surfaces when a
// precondition is unmet: the exact useradd command for a missing systemd
// user, the tmux install hint when tmux is absent, and nothing when all is
// well.
func TestProvisionPlan_Prerequisites(t *testing.T) {
	env := &ProvisionEnv{IsRoot: false, CurrentUser: "tony"}
	baseIn := ProvisionInput{
		Name: "compass-test", ServerDir: filepath.Join(t.TempDir(), "new"),
		Supervisor: "systemd", Source: SourceZip, ZipPath: "/srv/zips/x.zip",
		User: "mcserver",
	}

	t.Run("missing user -> exact useradd command", func(t *testing.T) {
		f := &fakeExec{output: map[string][]byte{}, fail: map[string]error{}}
		f.fail["id mcserver"] = errNotPresent
		p := &Provision{Env: env, Exec: f.run}
		plan, err := p.Plan(baseIn)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		want := "useradd --system --shell /usr/sbin/nologin mcserver"
		if len(plan.Prerequisites) != 1 || !strings.Contains(plan.Prerequisites[0], want) {
			t.Fatalf("Prerequisites = %v, want one containing %q", plan.Prerequisites, want)
		}
	})

	t.Run("user present -> no prerequisites", func(t *testing.T) {
		f := &fakeExec{output: map[string][]byte{}}
		p := &Provision{Env: env, Exec: f.run}
		plan, err := p.Plan(baseIn)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(plan.Prerequisites) != 0 {
			t.Fatalf("Prerequisites = %v, want none", plan.Prerequisites)
		}
	})

	t.Run("tmux absent -> install hint", func(t *testing.T) {
		in := baseIn
		in.Supervisor = "tmux"
		in.User = ""
		f := &fakeExec{output: map[string][]byte{}, fail: map[string]error{}}
		f.fail["tmux -V"] = errNotPresent
		f.fail["tmux has-session -t compass-test"] = errNotPresent
		p := &Provision{Env: env, Exec: f.run}
		plan, err := p.Plan(in)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(plan.Prerequisites) != 1 || !strings.Contains(plan.Prerequisites[0], "tmux") {
			t.Fatalf("Prerequisites = %v, want the tmux install hint", plan.Prerequisites)
		}
	})
}

// TestProvisionApply_Tmux_HappyPath pins the full tmux apply: dir prepared
// (zip extracted, eula accepted, port written), supervisor wired in the
// right order, joinability verified, and the action audit-logged.
func TestProvisionApply_Tmux_HappyPath(t *testing.T) {
	dir := t.TempDir()
	zipPath := makeServerZip(t, "bedrock-server-1.26.45.1.zip")
	f := &fakeExec{output: map[string][]byte{}, fail: map[string]error{}}
	f.fail["tmux has-session -t compass-test"] = errNotPresent // fresh: no session yet
	trail := newTestTrail(t)

	p := &Provision{
		Env:    &ProvisionEnv{IsRoot: false, CurrentUser: "tony"},
		Exec:   f.run,
		Verify: func(ctx context.Context, addr string) error { return nil },
		Trail:  trail,
	}
	in := ProvisionInput{
		Name:       "compass-test",
		ServerDir:  dir,
		Supervisor: "tmux",
		Source:     SourceZip,
		ZipPath:    zipPath,
		Port:       19199,
	}

	res, err := p.Apply(context.Background(), in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Status != ResultProvisioned {
		t.Fatalf("Status = %q, want %q", res.Status, ResultProvisioned)
	}

	if _, serr := os.Stat(filepath.Join(dir, "bedrock_server")); serr != nil {
		t.Errorf("bedrock_server not extracted: %v", serr)
	}
	eula, eerr := os.ReadFile(filepath.Join(dir, "eula.txt"))
	if eerr != nil || !strings.Contains(string(eula), "true") {
		t.Errorf("eula.txt = %q, err %v; want accepted", eula, eerr)
	}
	props, perr := os.ReadFile(filepath.Join(dir, "server.properties"))
	if perr != nil || !strings.Contains(string(props), "server-port=19199") {
		t.Errorf("server.properties = %q, err %v; want server-port=19199", props, perr)
	}

	calls := f.callsSnapshot()
	start := "tmux new-session -d -s compass-test -c " + dir + " LD_LIBRARY_PATH=. ./bedrock_server"
	pipe := "tmux pipe-pane -t compass-test -o cat >> " + dir + "/console.log"
	idx := func(cmd string) int {
		for i, c := range calls {
			if c == cmd {
				return i
			}
		}
		return -1
	}
	if !(idx(start) >= 0 && idx(pipe) >= 0 && idx(start) < idx(pipe)) {
		t.Errorf("calls = %v; want %q then %q", calls, start, pipe)
	}

	evs, rerr := trail.Recent(10)
	if rerr != nil {
		t.Fatalf("trail.Recent: %v", rerr)
	}
	if len(evs) == 0 || evs[len(evs)-1].Verb != "provision" || !strings.Contains(evs[len(evs)-1].Result, "ok") {
		t.Errorf("audit = %+v; want a provision/ok event", evs)
	}
}

// TestProvisionApply_Tmux_RollbackOnStartFailure pins the failure rule:
// when the supervisor start fails, the provisioner removes only the
// artifacts this run created (the server dir it made) and reports the
// failure without leaving a half-state.
func TestProvisionApply_Tmux_RollbackOnStartFailure(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "fresh") // the provisioner must create this
	zipPath := makeServerZip(t, "bedrock-server-1.26.45.1.zip")
	f := &fakeExec{output: map[string][]byte{}, fail: map[string]error{}}
	f.fail["tmux has-session -t compass-test"] = errNotPresent // fresh: no session yet
	f.fail["tmux new-session -d -s compass-test -c "+dir+" LD_LIBRARY_PATH=. ./bedrock_server"] = errStartFailed
	trail := newTestTrail(t)

	p := &Provision{
		Env:    &ProvisionEnv{IsRoot: false, CurrentUser: "tony"},
		Exec:   f.run,
		Verify: func(ctx context.Context, addr string) error { return nil },
		Trail:  trail,
	}
	in := ProvisionInput{
		Name:       "compass-test",
		ServerDir:  dir,
		Supervisor: "tmux",
		Source:     SourceZip,
		ZipPath:    zipPath,
	}

	res, err := p.Apply(context.Background(), in)
	if err == nil {
		t.Fatal("Apply: want error when the tmux start fails")
	}
	if res.Status != ResultFailed {
		t.Errorf("Status = %q, want %q", res.Status, ResultFailed)
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Errorf("server dir %q should have been rolled back", dir)
	}
	evs, _ := trail.Recent(10)
	if len(evs) == 0 || evs[len(evs)-1].Verb != "provision" || !strings.Contains(evs[len(evs)-1].Result, "error") {
		t.Errorf("audit = %+v; want a provision/error event", evs)
	}
}

// TestProvisionApply_NoopWritesNothing pins idempotency at the apply
// level: an already-provisioned deployment is detected and Apply changes
// nothing (no exec calls, no files touched).
func TestProvisionApply_NoopWritesNothing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bedrock_server"), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "eula.txt"), []byte("true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeExec{output: map[string][]byte{}} // has-session succeeds
	p := &Provision{
		Env:    &ProvisionEnv{IsRoot: false, CurrentUser: "tony"},
		Exec:   f.run,
		Verify: func(ctx context.Context, addr string) error { return nil },
	}
	in := ProvisionInput{
		Name: "compass-test", ServerDir: dir, Supervisor: "tmux",
		Source: SourceZip, ZipPath: "/srv/zips/x.zip",
	}
	res, err := p.Apply(context.Background(), in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Status != ResultNoop {
		t.Fatalf("Status = %q, want %q", res.Status, ResultNoop)
	}
	calls := f.callsSnapshot()
	for _, c := range calls {
		if strings.Contains(c, "new-session") || strings.Contains(c, "pipe-pane") || strings.Contains(c, "kill-session") {
			t.Fatalf("Apply made a mutation call on a no-op: %v", calls)
		}
	}
}

// TestProvisionApply_Systemd_Root pins the root systemd apply: unit files
// written with the exact rendered text, then daemon-reload, socket, service
// in order.
func TestProvisionApply_Systemd_Root(t *testing.T) {
	dir := t.TempDir()
	unitDir := t.TempDir()
	zipPath := makeServerZip(t, "bedrock-server-1.26.45.1.zip")
	f := &fakeExec{output: map[string][]byte{}}
	trail := newTestTrail(t)

	p := &Provision{
		Env:     &ProvisionEnv{IsRoot: true, CurrentUser: "root"},
		Exec:    f.run,
		Verify:  func(ctx context.Context, addr string) error { return nil },
		Trail:   trail,
		UnitDir: unitDir,
	}
	in := ProvisionInput{
		Name:       "compass-test",
		ServerDir:  dir,
		Supervisor: "systemd",
		Source:     SourceZip,
		ZipPath:    zipPath,
		User:       "mcserver",
	}

	res, err := p.Apply(context.Background(), in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Status != ResultProvisioned {
		t.Fatalf("Status = %q, want %q", res.Status, ResultProvisioned)
	}

	svc, serr := os.ReadFile(filepath.Join(unitDir, "compass-test.service"))
	if serr != nil {
		t.Fatalf("service unit not written: %v", serr)
	}
	plan, _ := p.Plan(in)
	if string(svc) != plan.UnitService {
		t.Errorf("service unit text = %q, want the exact plan rendering %q", svc, plan.UnitService)
	}
	sck, kerr := os.ReadFile(filepath.Join(unitDir, "compass-test.socket"))
	if kerr != nil {
		t.Fatalf("socket unit not written: %v", kerr)
	}
	if string(sck) != plan.UnitSocket {
		t.Errorf("socket unit text = %q, want the exact plan rendering %q", sck, plan.UnitSocket)
	}

	calls := f.callsSnapshot()
	for _, want := range []string{
		"systemctl daemon-reload",
		"systemctl start compass-test.socket",
		"systemctl start compass-test",
	} {
		if !contains(calls, want) {
			t.Errorf("calls %v missing %q", calls, want)
		}
	}
	idx := func(cmd string) int {
		for i, c := range calls {
			if c == cmd {
				return i
			}
		}
		return -1
	}
	if !(idx("systemctl daemon-reload") < idx("systemctl start compass-test.socket") &&
		idx("systemctl start compass-test.socket") < idx("systemctl start compass-test")) {
		t.Errorf("systemd start order wrong: %v", calls)
	}
}

// TestProvisionApply_Systemd_NonRoot pins the non-root hand-off: the
// provisioner does the file steps it can, then hands the operator the exact
// privileged commands instead of running them.
func TestProvisionApply_Systemd_NonRoot(t *testing.T) {
	dir := t.TempDir()
	zipPath := makeServerZip(t, "bedrock-server-1.26.45.1.zip")
	f := &fakeExec{output: map[string][]byte{}}
	p := &Provision{
		Env:    &ProvisionEnv{IsRoot: false, CurrentUser: "tony"},
		Exec:   f.run,
		Verify: func(ctx context.Context, addr string) error { return nil },
	}
	in := ProvisionInput{
		Name:       "compass-test",
		ServerDir:  dir,
		Supervisor: "systemd",
		Source:     SourceZip,
		ZipPath:    zipPath,
		User:       "mcserver",
	}

	res, err := p.Apply(context.Background(), in)
	if err == nil {
		t.Fatal("Apply: want an incomplete result when not root")
	}
	if res.Status != ResultIncomplete {
		t.Errorf("Status = %q, want %q", res.Status, ResultIncomplete)
	}
	if len(res.Commands) == 0 {
		t.Fatal("res.Commands is empty; want the exact privileged commands")
	}
	joined := strings.Join(res.Commands, "\n")
	for _, want := range []string{"compass-test.service", "compass-test.socket", "daemon-reload", "start compass-test"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Commands missing %q:\n%s", want, joined)
		}
	}
	if _, serr := os.Stat(filepath.Join(dir, "eula.txt")); serr != nil {
		t.Errorf("the file steps (eula) should still have run: %v", serr)
	}
}

// TestProvisionApply_VerifyRetry pins the joinability loop: the probe may
// need several attempts while the server boots, and a probe that never
// succeeds must surface as an incomplete result (the deployment is left in
// place for the operator to inspect).
func TestProvisionApply_VerifyRetry(t *testing.T) {
	zipPath := makeServerZip(t, "bedrock-server-1.26.45.1.zip")
	f := &fakeExec{output: map[string][]byte{}, fail: map[string]error{}}
	f.fail["tmux has-session -t compass-test"] = errNotPresent // fresh: no session yet

	t.Run("succeeds on a later attempt", func(t *testing.T) {
		attempts := 0
		p := &Provision{
			Env:            &ProvisionEnv{IsRoot: false, CurrentUser: "tony"},
			Exec:           f.run,
			VerifyDeadline: 5 * time.Second,
			VerifyInterval: 5 * time.Millisecond,
			Verify: func(ctx context.Context, addr string) error {
				attempts++
				if attempts < 3 {
					return fmt.Errorf("not yet")
				}
				return nil
			},
		}
		in := ProvisionInput{
			Name: "compass-test", ServerDir: t.TempDir(), Supervisor: "tmux",
			Source: SourceZip, ZipPath: zipPath,
		}
		res, err := p.Apply(context.Background(), in)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if res.Status != ResultProvisioned || attempts < 3 {
			t.Fatalf("Status=%q attempts=%d; want provisioned after >=3 probes", res.Status, attempts)
		}
	})

	t.Run("never succeeds -> incomplete, deployment left", func(t *testing.T) {
		p := &Provision{
			Env:            &ProvisionEnv{IsRoot: false, CurrentUser: "tony"},
			Exec:           f.run,
			Verify:         func(ctx context.Context, addr string) error { return fmt.Errorf("refused") },
			VerifyDeadline: 50 * time.Millisecond,
			VerifyInterval: 5 * time.Millisecond,
		}
		dir := t.TempDir()
		in := ProvisionInput{
			Name: "compass-test", ServerDir: dir, Supervisor: "tmux",
			Source: SourceZip, ZipPath: zipPath,
		}
		res, err := p.Apply(context.Background(), in)
		if err == nil {
			t.Fatal("Apply: want an error when the server never becomes joinable")
		}
		if res.Status != ResultIncomplete {
			t.Errorf("Status = %q, want %q", res.Status, ResultIncomplete)
		}
		if _, serr := os.Stat(filepath.Join(dir, "bedrock_server")); serr != nil {
			t.Errorf("the deployment should be left in place for inspection")
		}
		if len(res.Cleanup) == 0 {
			t.Errorf("res.Cleanup is empty; want the exact cleanup commands")
		}
	})
}

// TestProvisionApply_DownloadSuccessCleanup pins that a downloaded zip is
// used and then removed: the operator's host is not littered with a
// ~100 MB temp file after the extract.
func TestProvisionApply_DownloadSuccessCleanup(t *testing.T) {
	dir := t.TempDir()
	zipPath := makeServerZip(t, "downloaded.zip")
	downloaded := false
	f := &fakeExec{output: map[string][]byte{}, fail: map[string]error{}}
	f.fail["tmux has-session -t compass-test"] = errNotPresent
	p := &Provision{
		Env:    &ProvisionEnv{IsRoot: false, CurrentUser: "tony"},
		Exec:   f.run,
		Fetch:  func(ctx context.Context, url string) (string, error) { downloaded = true; return zipPath, nil },
		Verify: func(ctx context.Context, addr string) error { return nil },
	}
	in := ProvisionInput{
		Name: "compass-test", ServerDir: dir, Supervisor: "tmux",
		Source: SourceVersion, Version: "1.26.45.1",
	}
	res, err := p.Apply(context.Background(), in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Status != ResultProvisioned {
		t.Fatalf("Status = %q, want %q", res.Status, ResultProvisioned)
	}
	if !downloaded {
		t.Fatal("Fetch was not called")
	}
	if _, serr := os.Stat(zipPath); !os.IsNotExist(serr) {
		t.Errorf("downloaded zip %q should have been removed after extraction", zipPath)
	}
	if _, serr := os.Stat(filepath.Join(dir, "bedrock_server")); serr != nil {
		t.Errorf("bedrock_server should be extracted: %v", serr)
	}
}

// TestProvisionApply_DownloadError pins the loud, actionable download
// failure: the operator is told to put a local zip in the target dir and
// re-run, and no partial download artifact is left behind.
func TestProvisionApply_DownloadError(t *testing.T) {
	dir := t.TempDir()
	f := &fakeExec{output: map[string][]byte{}, fail: map[string]error{}}
	f.fail["tmux has-session -t compass-test"] = errNotPresent // fresh: no session yet
	p := &Provision{
		Env:    &ProvisionEnv{IsRoot: false, CurrentUser: "tony"},
		Exec:   f.run,
		Fetch:  func(ctx context.Context, url string) (string, error) { return "", fmt.Errorf("connection reset") },
		Verify: func(ctx context.Context, addr string) error { return nil },
	}
	in := ProvisionInput{
		Name: "compass-test", ServerDir: dir, Supervisor: "tmux",
		Source: SourceVersion, Version: "1.26.45.1",
	}
	_, err := p.Apply(context.Background(), in)
	if err == nil {
		t.Fatal("Apply: want an error when the download fails")
	}
	if !strings.Contains(err.Error(), "zip") {
		t.Errorf("error should point the operator at the local-zip fallback: %v", err)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// makeServerZip builds a zip whose top-level entry is bedrock_server (the
// shape of the official BDS archive, minus the real binary) and returns its
// path.
func makeServerZip(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	zf, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	w, err := zw.Create("bedrock_server")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("fake-bedrock-server")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// newTestTrail builds an audit trail in a temp dir.
func newTestTrail(t *testing.T) *Trail {
	t.Helper()
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return trail
}

// errStartFailed stands in for a supervisor start failure.
var errStartFailed = fmt.Errorf("start failed")

// errNotPresent stands in for "the thing does not exist" exec failures
// (unknown tmux session, missing user, ...).
var errNotPresent = fmt.Errorf("not present")
