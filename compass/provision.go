package compass

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SourceMode selects where the bedrock-server zip comes from (ticket #14):
// an operator-provided local file, or a direct download by explicit version.
type SourceMode string

const (
	// SourceZip uses an existing zip on this host (ZipPath).
	SourceZip SourceMode = "zip"
	// SourceVersion downloads the zip for Version from the official
	// endpoint (best-effort; a failure points back to SourceZip).
	SourceVersion SourceMode = "version"
)

// ProvisionInput is a complete provision request (ticket #14). It names the
// deployment, where it goes, how it is supervised, where the server binary
// comes from, and (for systemd) which user runs it.
type ProvisionInput struct {
	// Name is the deployment name: the systemd unit base name or the tmux
	// session name. Lowercase letters, digits, dashes; must not start with a
	// digit or dash.
	Name string
	// ServerDir is the absolute directory the server is installed into.
	ServerDir string
	// Supervisor is "systemd" or "tmux".
	Supervisor string
	// Source selects the zip acquisition mode; ZipPath or Version carries
	// the detail for the selected mode.
	Source SourceMode
	// ZipPath is the local zip file (SourceZip).
	ZipPath string
	// Version is the exact BDS version to download (SourceVersion).
	Version string
	// User runs the systemd unit. Empty with a non-root provisioner means
	// the current user; empty with a root provisioner is an error (root
	// must name the user explicitly).
	User string
	// Port is the initial server-port written to the fresh
	// server.properties. Zero means "leave the BDS default".
	Port int
}

// ProvisionEnv describes the host context a provision runs in: whether the
// provisioner has root, and who the current user is. Production wiring
// resolves it from the OS (ResolveProvisionEnv); tests inject a fixed one.
type ProvisionEnv struct {
	IsRoot      bool
	CurrentUser string
}

// ResolveProvisionEnv reads the host context from the OS.
func ResolveProvisionEnv() *ProvisionEnv {
	env := &ProvisionEnv{IsRoot: os.Geteuid() == 0}
	if u, err := user.Current(); err == nil {
		env.CurrentUser = u.Username
	}
	return env
}

// Provision plans and applies server provisioning (ticket #14). It is
// TUI-free (D15): the TUI renders Plan and calls Apply. The seams are
// injectable so the whole flow is testable offline: Exec for host
// commands, Fetch for the download, Verify for the joinability probe.
type Provision struct {
	Env    *ProvisionEnv
	Exec   ExecFunc
	Fetch  func(ctx context.Context, url string) (string, error)
	Verify func(ctx context.Context, addr string) error
	Trail  *Trail
	// UnitDir is where systemd units are installed (default
	// /etc/systemd/system). Injectable so tests never touch the real one.
	UnitDir string

	// VerifyDeadline bounds the joinability retry loop (default 90s: a
	// first launch generates the world and takes a while).
	VerifyDeadline time.Duration
	// VerifyInterval is the gap between joinability probes (default 2s).
	VerifyInterval time.Duration
}

var (
	nameRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	versionRe = regexp.MustCompile(`^\d{1,2}\.\d{1,2}\.\d{1,2}(\.\d{1,3})?$`)
)

// Validate checks a provision request against the input contract. It is
// pure (no host access) apart from reading p.Env.
func (p *Provision) Validate(in ProvisionInput) error {
	env := p.env()
	if in.Name == "" {
		return fmt.Errorf("name: required")
	}
	if !nameRe.MatchString(in.Name) {
		return fmt.Errorf("name: %q must be lowercase letters, digits, dashes (max 63, starting with a letter)", in.Name)
	}
	if in.ServerDir == "" {
		return fmt.Errorf("server dir: required")
	}
	if !strings.HasPrefix(in.ServerDir, "/") {
		return fmt.Errorf("server dir: %q must be an absolute path", in.ServerDir)
	}
	switch in.Supervisor {
	case "systemd":
	case "tmux":
	default:
		return fmt.Errorf("supervisor: must be \"systemd\" or \"tmux\" (got %q)", in.Supervisor)
	}
	switch in.Source {
	case SourceZip:
		if in.ZipPath == "" {
			return fmt.Errorf("zip path: required for zip source")
		}
	case SourceVersion:
		if in.Version == "" {
			return fmt.Errorf("version: required for version source")
		}
		if !versionRe.MatchString(in.Version) {
			return fmt.Errorf("version: %q is malformed (want e.g. 1.26.45.1)", in.Version)
		}
	default:
		return fmt.Errorf("source: must be %q or %q", SourceZip, SourceVersion)
	}
	if in.Supervisor == "systemd" && in.User == "" {
		if env.IsRoot {
			return fmt.Errorf("user: required when provisioning a systemd unit as root (name the user the server runs as)")
		}
		// Non-root: defaults to the current user; Validate does not
		// enforce the default, Plan and Apply resolve it.
	}
	if in.Port != 0 && (in.Port < 1024 || in.Port > 65535) {
		return fmt.Errorf("port: %d must be in range 1024-65535 (0 means the BDS default)", in.Port)
	}
	return nil
}

// env returns the provisioner's host context, resolving it from the OS when
// the caller did not inject one.
func (p *Provision) env() *ProvisionEnv {
	if p.Env != nil {
		return p.Env
	}
	return ResolveProvisionEnv()
}

// exec returns the provisioner's command runner, defaulting to the real one.
func (p *Provision) exec() ExecFunc {
	if p.Exec != nil {
		return p.Exec
	}
	return Exec
}

// bdsDefaultPort is the BDS default game port, used in the config snippet
// when the operator did not pin one.
const bdsDefaultPort = 19132

// tmuxStartCmd is the command the tmux pane runs: bedrock_server with its
// bundled libraries (the same invocation the systemd unit uses).
const tmuxStartCmd = "LD_LIBRARY_PATH=. ./bedrock_server"

// ProvisionPlan is the rendered provision: what will be done (Action +
// Steps), the exact artifacts that will be created (UnitService/UnitSocket
// for systemd, TmuxCommands for tmux), prerequisites the operator must
// satisfy first, and the config snippet that points the tool at the new
// deployment. It is a pure rendering — nothing is written until Apply.
type ProvisionPlan struct {
	Action        string
	Input         ProvisionInput
	EffectiveUser string
	Steps         []string
	UnitService   string
	UnitSocket    string
	TmuxCommands  []string
	Prerequisites []string
	ConfigSnippet string
}

// Provision plan actions.
const (
	// PlanActionProvision: fresh host state; full provision.
	PlanActionProvision = "provision"
	// PlanActionWire: server dir is prepared but the supervisor is missing;
	// wire the supervisor only (completes a partial state).
	PlanActionWire = "wire-supervisor"
	// PlanActionNoop: already provisioned; Apply changes nothing.
	PlanActionNoop = "noop"
	// PlanActionInconsistent: supervisor present but the server dir is not
	// prepared; report, change nothing.
	PlanActionInconsistent = "inconsistent"
)

// Plan renders the provision for in: detects the current state (server dir,
// supervisor), decides the action, and renders every artifact verbatim so
// the operator can confirm exactly what will be created. Pure apart from
// state detection (os.Stat of the server dir, one exec probe for tmux).
func (p *Provision) Plan(in ProvisionInput) (ProvisionPlan, error) {
	if err := p.Validate(in); err != nil {
		return ProvisionPlan{}, err
	}
	env := p.env()
	user := in.User
	if user == "" && env.CurrentUser != "" {
		user = env.CurrentUser
	}
	exec := p.exec()

	dirPrepared := p.dirPrepared(in.ServerDir)
	supPresent := p.supervisorPresent(in, exec)

	var action string
	switch {
	case dirPrepared && supPresent:
		action = PlanActionNoop
	case dirPrepared && !supPresent:
		action = PlanActionWire
	case !dirPrepared && supPresent:
		action = PlanActionInconsistent
	default:
		action = PlanActionProvision
	}

	plan := ProvisionPlan{
		Action:        action,
		Input:         in,
		EffectiveUser: user,
	}
	plan.Steps = p.stepsFor(action, in, user)
	switch in.Supervisor {
	case "systemd":
		plan.UnitService = renderServerServiceUnit(in.Name, user, in.ServerDir)
		plan.UnitSocket = renderServerSocketUnit(in.Name)
		plan.Prerequisites = p.systemdPrerequisites(user, exec)
	default:
		plan.TmuxCommands = tmuxCommands(in)
		plan.Prerequisites = p.tmuxPrerequisites(exec)
	}
	plan.ConfigSnippet = renderConfigSnippet(in)
	return plan, nil
}

// dirPrepared reports whether the server dir holds a usable installation:
// the server binary and an accepted EULA.
func (p *Provision) dirPrepared(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "bedrock_server")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "eula.txt")); err != nil {
		return false
	}
	return true
}

// supervisorPresent reports whether the deployment's supervisor already
// exists: the systemd unit files on disk, or a live tmux session.
func (p *Provision) supervisorPresent(in ProvisionInput, exec ExecFunc) bool {
	switch in.Supervisor {
	case "systemd":
		unitDir := p.unitDir()
		if _, err := os.Stat(filepath.Join(unitDir, in.Name+".service")); err != nil {
			return false
		}
		_, err := os.Stat(filepath.Join(unitDir, in.Name+".socket"))
		return err == nil
	default:
		_, err := exec(context.Background(), "tmux", "has-session", "-t", in.Name)
		return err == nil
	}
}

// stepsFor renders the human-readable step list for the plan's action.
func (p *Provision) stepsFor(action string, in ProvisionInput, user string) []string {
	if action == PlanActionNoop {
		return []string{"Already provisioned: server dir and " + in.Supervisor + " state both present. Apply changes nothing."}
	}
	if action == PlanActionInconsistent {
		return []string{"Inconsistent state: the " + in.Supervisor + " deployment exists but the server dir is not prepared. Inspect manually before re-running."}
	}
	var steps []string
	if action == PlanActionProvision {
		steps = append(steps,
			"Install the server into "+in.ServerDir+" (extract the zip, mark bedrock_server executable)",
			"Write eula.txt (accept the EULA)",
		)
	}
	if in.Port != 0 {
		steps = append(steps, fmt.Sprintf("Set server-port=%d in server.properties", in.Port))
	}
	if in.Supervisor == "systemd" {
		unitDir := p.unitDir()
		steps = append(steps,
			fmt.Sprintf("Install %s and %s (unit user: %s)", unitDir+"/"+in.Name+".service", unitDir+"/"+in.Name+".socket", user),
			fmt.Sprintf("systemctl daemon-reload, then start %s.socket, then start %s", in.Name, in.Name),
		)
	} else {
		steps = append(steps,
			fmt.Sprintf("Start the tmux session %q running the server", in.Name),
			fmt.Sprintf("Pipe the pane output to %s/console.log", in.ServerDir),
		)
	}
	steps = append(steps, "Verify the server is joinable (pong on the game port)")
	return steps
}

// systemdPrerequisites lists what must be true before a systemd apply can
// succeed: the unit user exists (otherwise the exact useradd command).
func (p *Provision) systemdPrerequisites(user string, exec ExecFunc) []string {
	if user == "" {
		return nil
	}
	if _, err := exec(context.Background(), "id", user); err != nil {
		return []string{fmt.Sprintf("Create the user first: useradd --system --shell /usr/sbin/nologin %s", user)}
	}
	return nil
}

// tmuxPrerequisites lists what must be true before a tmux apply can
// succeed: tmux itself installed (otherwise the exact install hint).
func (p *Provision) tmuxPrerequisites(exec ExecFunc) []string {
	if _, err := exec(context.Background(), "tmux", "-V"); err != nil {
		return []string{"Install tmux first (e.g. apt install tmux)"}
	}
	return nil
}

// renderServerServiceUnit renders the exact .service unit text (the proven
// production shape, ADR-0001: StandardInput=socket plus explicit journal
// output are non-negotiable).
func renderServerServiceUnit(name, user, dir string) string {
	return fmt.Sprintf(`[Unit]
Description=Minecraft Bedrock server (%s)
After=network-online.target

[Service]
User=%s
Group=%s
WorkingDirectory=%s
ExecStart=/bin/sh -c "LD_LIBRARY_PATH=. ./bedrock_server"
StandardInput=socket
StandardOutput=journal
StandardError=journal
Restart=on-failure
`, name, user, user, dir)
}

// renderServerSocketUnit renders the exact .socket unit text: the console FIFO
// (ADR-0001).
func renderServerSocketUnit(name string) string {
	return fmt.Sprintf(`[Socket]
Accept=no
ListenFIFO=/run/%s-console.fifo
SocketMode=0660
`, name)
}

// tmuxCommands renders the exact tmux commands the plan will run: start the
// named session in the server dir, then pipe the pane to the console log.
func tmuxCommands(in ProvisionInput) []string {
	return []string{
		fmt.Sprintf("tmux new-session -d -s %s -c %s %s", in.Name, in.ServerDir, tmuxStartCmd),
		fmt.Sprintf("tmux pipe-pane -t %s -o cat >> %s/console.log", in.Name, in.ServerDir),
	}
}

// renderConfigSnippet renders the config block that points the tool at the
// new deployment (shown to the operator after a successful provision; the
// tool never writes the config itself — Adopt does that when the operator
// chooses).
func renderConfigSnippet(in ProvisionInput) string {
	port := in.Port
	if port == 0 {
		port = bdsDefaultPort
	}
	if in.Supervisor == "systemd" {
		return fmt.Sprintf(`# compass config for %s
server-dir: %s
supervisor: systemd
service: %s
console-fif: /run/%s-console.fifo
port: %d
`, in.Name, in.ServerDir, in.Name, in.Name, port)
	}
	return fmt.Sprintf(`# compass config for %s
server-dir: %s
supervisor: tmux
pane: %s
port: %d
`, in.Name, in.ServerDir, in.Name, port)
}

// Result statuses for a provision apply.
const (
	// ResultProvisioned: the deployment was created and is joinable.
	ResultProvisioned = "provisioned"
	// ResultNoop: already provisioned; nothing was changed.
	ResultNoop = "noop"
	// ResultIncomplete: the server-side work is done but the operator must
	// finish privileged steps (non-root systemd hand-off) or the server did
	// not become joinable in time (left in place for inspection).
	ResultIncomplete = "incomplete"
	// ResultFailed: a step failed and this-run artifacts were rolled back.
	ResultFailed = "failed"
)

// ProvisionResult reports what Apply did: its status, an operator-facing
// detail, any exact commands the operator must run (hand-off), and any
// exact cleanup commands when the deployment was left in place.
type ProvisionResult struct {
	Status   string
	Detail   string
	Commands []string
	Cleanup  []string
}

// errPrivilegedHandoff signals that the file steps succeeded but the
// privileged steps were not run because the provisioner lacks root.
var errPrivilegedHandoff = fmt.Errorf("privileged steps require root; the exact commands were returned")

// Apply executes the provision for in. It is the single mutation path:
// prepare the server dir, wire the supervisor, and verify the server is
// joinable. It never destroys an operator-provided artifact, rolls back
// only this run's artifacts on failure, and audit-logs the action (D11).
func (p *Provision) Apply(ctx context.Context, in ProvisionInput) (ProvisionResult, error) {
	plan, err := p.Plan(in)
	if err != nil {
		return ProvisionResult{}, err
	}

	if plan.Action == PlanActionNoop {
		p.auditProvision(in, plan.Action, ResultNoop, nil)
		return ProvisionResult{Status: ResultNoop, Detail: "already provisioned"}, nil
	}

	if plan.Action == PlanActionInconsistent {
		msg := "inconsistent state: the supervisor exists but the server dir is not prepared; inspect before re-running"
		p.auditProvision(in, plan.Action, ResultFailed, fmt.Errorf("%s", msg))
		return ProvisionResult{Status: ResultFailed, Detail: msg}, fmt.Errorf("%s", msg)
	}

	// Remember whether the operator already owns the server dir so rollback
	// never deletes something that pre-existed this run.
	dirExisted := pathExists(in.ServerDir)

	// 1. Prepare the server dir (idempotent; a prepared dir is skipped).
	if !p.dirPrepared(in.ServerDir) {
		if err := p.prepareDir(ctx, in); err != nil {
			p.rollbackDir(in.ServerDir, dirExisted)
			p.auditProvision(in, plan.Action, ResultFailed, err)
			return ProvisionResult{Status: ResultFailed, Detail: err.Error()}, err
		}
	}
	if in.Port != 0 {
		if err := p.writePort(in); err != nil {
			p.rollbackDir(in.ServerDir, dirExisted)
			p.auditProvision(in, plan.Action, ResultFailed, err)
			return ProvisionResult{Status: ResultFailed, Detail: err.Error()}, err
		}
	}

	// 2. Non-root systemd: do the file steps, then hand off the exact
	//    privileged commands instead of running them.
	if in.Supervisor == "systemd" && !p.env().IsRoot {
		cmds, err := p.systemdHandoff(in, plan)
		if err != nil {
			p.auditProvision(in, plan.Action, ResultFailed, err)
			return ProvisionResult{Status: ResultFailed, Detail: err.Error()}, err
		}
		p.auditProvision(in, plan.Action, ResultIncomplete, errPrivilegedHandoff)
		return ProvisionResult{
			Status:   ResultIncomplete,
			Detail:   "not root: the file steps are done; run the returned commands as root to finish",
			Commands: cmds,
		}, errPrivilegedHandoff
	}

	// 3. Wire the supervisor.
	if err := p.wireSupervisor(ctx, in, plan); err != nil {
		p.rollbackSupervisor(ctx, in)
		p.rollbackDir(in.ServerDir, dirExisted)
		p.auditProvision(in, plan.Action, ResultFailed, err)
		return ProvisionResult{Status: ResultFailed, Detail: err.Error()}, err
	}

	// 4. Verify the server is joinable. A failure leaves the deployment in
	//    place (the operator may want to inspect it) and reports cleanup.
	if err := p.verifyJoinable(ctx, in); err != nil {
		cleanup := p.cleanupCommands(in)
		p.auditProvision(in, plan.Action, ResultIncomplete, err)
		return ProvisionResult{
			Status:  ResultIncomplete,
			Detail:  err.Error(),
			Cleanup: cleanup,
		}, err
	}

	p.auditProvision(in, plan.Action, ResultProvisioned, nil)
	return ProvisionResult{
		Status: ResultProvisioned,
		Detail: fmt.Sprintf("%s deployment %q is running and joinable", in.Supervisor, in.Name),
	}, nil
}

// auditProvision appends one audit event for a provision action (D11).
// Best effort: a missing trail must not break the action.
func (p *Provision) auditProvision(in ProvisionInput, action, status string, err error) {
	if p.Trail == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error: " + err.Error()
	}
	ev := Event{
		Verb: "provision",
		Params: map[string]string{
			"name":       in.Name,
			"server-dir": in.ServerDir,
			"supervisor": in.Supervisor,
			"action":     action,
			"source":     string(in.Source),
			"status":     status,
		},
		Result: result,
	}
	_ = p.Trail.Append(ev)
}

// pathExists reports whether path exists (file or directory).
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// prepareDir installs the server into in.ServerDir: creates the dir,
// extracts the server binary from the zip (local or downloaded), accepts
// the EULA, and (the caller) sets the port. Idempotent: present artifacts
// are left untouched.
func (p *Provision) prepareDir(ctx context.Context, in ProvisionInput) error {
	if err := os.MkdirAll(in.ServerDir, 0o755); err != nil {
		return fmt.Errorf("create server dir: %w", err)
	}

	bin := filepath.Join(in.ServerDir, "bedrock_server")
	if _, err := os.Stat(bin); err != nil {
		zipPath, cleanup, err := p.obtainZip(ctx, in)
		if err != nil {
			return err
		}
		defer cleanup()
		if err := extractZip(zipPath, in.ServerDir); err != nil {
			return err
		}
		if _, err := os.Stat(bin); err != nil {
			return fmt.Errorf("zip %s did not produce bedrock_server", zipPath)
		}
		if err := os.Chmod(bin, 0o755); err != nil {
			return fmt.Errorf("mark bedrock_server executable: %w", err)
		}
	}

	eula := filepath.Join(in.ServerDir, "eula.txt")
	if _, err := os.Stat(eula); err != nil {
		if err := os.WriteFile(eula, []byte("true\n"), 0o644); err != nil {
			return fmt.Errorf("write eula.txt: %w", err)
		}
	}
	return nil
}

// obtainZip resolves the zip to extract from. For the zip source it returns
// the operator's path; for the version source it downloads (via p.Fetch) and
// returns a temp path plus a cleanup that removes it.
func (p *Provision) obtainZip(ctx context.Context, in ProvisionInput) (string, func(), error) {
	if in.Source == SourceZip {
		if _, err := os.Stat(in.ZipPath); err != nil {
			return "", nil, fmt.Errorf("zip not found: %s — put the official bedrock-server zip where the tool can read it and re-run with that path", in.ZipPath)
		}
		return in.ZipPath, func() {}, nil
	}

	fetch := p.Fetch
	if fetch == nil {
		fetch = defaultFetch
	}
	url := "https://www.minecraft.net/bedrockdedicatedserver/bin-linux/bedrock-server-" + in.Version + ".zip"
	path, err := fetch(ctx, url)
	if err != nil {
		return "", nil, fmt.Errorf("download failed: %v — fallback: download the official zip for version %s yourself, place it where the tool can read it, and re-run with that local zip", err, in.Version)
	}
	return path, func() { os.Remove(path) }, nil
}

// defaultFetch downloads url to a temp file and returns its path.
func defaultFetch(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return "", fmt.Errorf("server returned %s", resp.Status)
	}
	f, err := os.CreateTemp("", "bedrock-server-*.zip")
	if err != nil {
		return "", fmt.Errorf("create temp zip: %w", err)
	}
	path := f.Name()
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("write zip: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// writePort sets server-port in the fresh server.properties (idempotent).
func (p *Provision) writePort(in ProvisionInput) error {
	path := filepath.Join(in.ServerDir, "server.properties")
	current, _ := os.ReadFile(path)
	if err := writeKey(path, "server-port", strconv.Itoa(in.Port), string(current)); err != nil {
		return fmt.Errorf("set server-port: %w", err)
	}
	return nil
}

// wireSupervisor brings the deployment's supervisor up.
func (p *Provision) wireSupervisor(ctx context.Context, in ProvisionInput, plan ProvisionPlan) error {
	if in.Supervisor == "tmux" {
		return p.wireTmux(ctx, in)
	}
	return p.wireSystemd(ctx, in, plan)
}

// wireTmux starts the named tmux session running the server, then pipes the
// pane to the console log. A failure kills the session this run created.
func (p *Provision) wireTmux(ctx context.Context, in ProvisionInput) error {
	exec := p.exec()
	if _, err := exec(ctx, "tmux", "new-session", "-d", "-s", in.Name, "-c", in.ServerDir, tmuxStartCmd); err != nil {
		return fmt.Errorf("tmux new-session %s: %w", in.Name, err)
	}
	if _, err := exec(ctx, "tmux", "pipe-pane", "-t", in.Name, "-o", "cat >> "+in.ServerDir+"/console.log"); err != nil {
		exec(ctx, "tmux", "kill-session", "-t", in.Name)
		return fmt.Errorf("tmux pipe-pane %s: %w", in.Name, err)
	}
	return nil
}

// wireSystemd installs the unit files, daemon-reloads, and starts the socket
// then the service (the ADR-0001 start order). Only called when the
// provisioner has root.
func (p *Provision) wireSystemd(ctx context.Context, in ProvisionInput, plan ProvisionPlan) error {
	exec := p.exec()
	unitDir := p.unitDir()
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return fmt.Errorf("create unit dir: %w", err)
	}
	svc := filepath.Join(unitDir, in.Name+".service")
	sck := filepath.Join(unitDir, in.Name+".socket")
	if err := os.WriteFile(svc, []byte(plan.UnitService), 0o644); err != nil {
		return fmt.Errorf("write service unit: %w", err)
	}
	if err := os.WriteFile(sck, []byte(plan.UnitSocket), 0o644); err != nil {
		os.Remove(svc)
		return fmt.Errorf("write socket unit: %w", err)
	}
	if _, err := exec(ctx, "systemctl", "daemon-reload"); err != nil {
		p.removeUnits(svc, sck)
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if _, err := exec(ctx, "systemctl", "start", in.Name+".socket"); err != nil {
		p.removeUnits(svc, sck)
		exec(ctx, "systemctl", "daemon-reload")
		return fmt.Errorf("systemctl start %s.socket: %w", in.Name, err)
	}
	if _, err := exec(ctx, "systemctl", "start", in.Name); err != nil {
		p.removeUnits(svc, sck)
		exec(ctx, "systemctl", "daemon-reload")
		return fmt.Errorf("systemctl start %s: %w", in.Name, err)
	}
	return nil
}

// systemdHandoff stages the exact unit files under the server dir and
// returns the privileged commands the operator must run as root.
func (p *Provision) systemdHandoff(in ProvisionInput, plan ProvisionPlan) ([]string, error) {
	stage := filepath.Join(in.ServerDir, "units")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return nil, fmt.Errorf("stage units: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stage, in.Name+".service"), []byte(plan.UnitService), 0o644); err != nil {
		return nil, fmt.Errorf("write service unit: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stage, in.Name+".socket"), []byte(plan.UnitSocket), 0o644); err != nil {
		return nil, fmt.Errorf("write socket unit: %w", err)
	}
	return []string{
		fmt.Sprintf("sudo cp %s /etc/systemd/system/", stage+"/"+in.Name+".service"),
		fmt.Sprintf("sudo cp %s /etc/systemd/system/", stage+"/"+in.Name+".socket"),
		"sudo systemctl daemon-reload",
		fmt.Sprintf("sudo systemctl start %s.socket", in.Name),
		fmt.Sprintf("sudo systemctl start %s", in.Name),
	}, nil
}

// removeUnits deletes the unit files this run wrote (best effort).
func (p *Provision) removeUnits(svc, sck string) {
	os.Remove(svc)
	os.Remove(sck)
}

// unitDir returns the systemd unit directory, defaulting to the system one.
func (p *Provision) unitDir() string {
	if p.UnitDir != "" {
		return p.UnitDir
	}
	return "/etc/systemd/system"
}

// rollbackSupervisor undoes the supervisor wiring this run created.
func (p *Provision) rollbackSupervisor(ctx context.Context, in ProvisionInput) {
	exec := p.exec()
	if in.Supervisor == "tmux" {
		exec(ctx, "tmux", "kill-session", "-t", in.Name)
	}
}

// rollbackDir removes the server dir, but only when this run created it.
func (p *Provision) rollbackDir(dir string, existedBefore bool) {
	if existedBefore {
		return
	}
	os.RemoveAll(dir)
}

// cleanupCommands returns the exact commands to tear down a deployment the
// operator may want to remove after an incomplete result.
func (p *Provision) cleanupCommands(in ProvisionInput) []string {
	var cmds []string
	if in.Supervisor == "systemd" {
		cmds = append(cmds,
			fmt.Sprintf("sudo systemctl disable --now %s.socket", in.Name),
			fmt.Sprintf("sudo systemctl disable --now %s", in.Name),
			fmt.Sprintf("sudo rm /etc/systemd/system/%s.service /etc/systemd/system/%s.socket", in.Name, in.Name),
			"sudo systemctl daemon-reload",
		)
	} else {
		cmds = append(cmds, fmt.Sprintf("tmux kill-session -t %s", in.Name))
	}
	cmds = append(cmds, fmt.Sprintf("rm -rf %s", in.ServerDir))
	return cmds
}

// verifyJoinable probes the game port until the server answers (pong) or the
// deadline passes. The server may take a while to become joinable while it
// generates the world on first launch.
func (p *Provision) verifyJoinable(ctx context.Context, in ProvisionInput) error {
	verify := p.Verify
	if verify == nil {
		return fmt.Errorf("no joinability probe configured")
	}
	port := in.Port
	if port == 0 {
		port = bdsDefaultPort
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)

	deadline := p.VerifyDeadline
	if deadline <= 0 {
		deadline = 90 * time.Second
	}
	interval := p.VerifyInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}

	var lastErr error
	deadlineAt := time.Now().Add(deadline)
	for {
		if err := verify(ctx, addr); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadlineAt) {
			return fmt.Errorf("server did not become joinable at %s within %v: %w", addr, deadline, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// extractZip extracts the archive into dest, guarding against zip-slip.
func extractZip(zipPath, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()
	cleanDest := filepath.Clean(dest)
	for _, f := range r.File {
		target := filepath.Join(cleanDest, filepath.Clean("/"+f.Name))
		if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) {
			continue // refuse paths that escape the destination
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		src, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode()&0o777)
		if err != nil {
			src.Close()
			return err
		}
		if _, err := io.Copy(out, src); err != nil {
			out.Close()
			src.Close()
			return err
		}
		out.Close()
		src.Close()
	}
	return nil
}
