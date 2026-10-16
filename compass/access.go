// Package compass: the Access service (ticket #7). It is the TUI-free domain
// core behind the Access screen — Bedrock's deny mechanism as a first-class,
// tiered surface. Four capabilities, all read-only until an explicit action:
//
//   - the allowlist gate (server.properties allow-list=), toggled with a
//     prominent "rejects every player not on the list" consequence; the tool
//     never flips it without an explicit, confirmed action (D3a);
//   - allowlist add / remove / reload by gamertag or XUID — written directly
//     to allowlist.json atomically (temp+rename) and reloaded into the running
//     server, then verified against the server's own `allowlist list`;
//   - quick-add of a recently-connected player in one action;
//   - per-IP host-firewall blocks with graceful permission degradation: when
//     unprivileged the exact command is surfaced instead of applied.
//
// Every mutating action is audit-logged (D11). The service never imports the
// TUI (the depguard test enforces it); the Access screen renders this.
package compass

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// AllowEntry is one row of allowlist.json. BDS writes exactly these two
// fields; name is a gamertag or XUID (the server resolves gamertags to XUIDs
// on first join — D3a, so no XUID knowledge is required up front).
// ignoresPlayerLimit is always false in practice but is preserved verbatim.
type AllowEntry struct {
	Name               string `json:"name"`
	IgnoresPlayerLimit bool   `json:"ignoresPlayerLimit"`
}

// GateState is the allowlist gate's durable setting: the server.properties
// allow-list= key (the value a restart enforces). Source names where it was
// read from; empty when the file could not be read.
type GateState struct {
	Enabled bool
	Source  string
}

// IPBlockResult reports one firewall block/unblock attempt. Command is always
// populated — it is what the operator needs to copy-paste when we lack the
// permission to apply it ourselves (graceful degradation, ticket #7). Applied
// is true only when the tool actually changed the host firewall; otherwise
// Reason explains why and Command carries the exact command.
type IPBlockResult struct {
	Applied bool
	Command string
	Reason  string // "" when applied; else why not (e.g. "requires root")
}

// Access is the ticket #7 service over already-resolved seams (the Stack's).
// Cfg supplies ServerDir + Port; Console applies changes to a running server
// (nil = no channel, e.g. a stopped server — file writes still work and apply
// on next start); Trail records every mutation (D11).
type Access struct {
	Cfg     *Config
	Console *Console
	Trail   *Trail

	// euid returns the effective uid; overridable in tests so the root vs.
	// unprivileged firewall paths are both exercisable offline. Default is
	// os.Geteuid.
	euid func() int
	// run executes a host command (the firewall tool); overridable in tests.
	// Default is realRunCmd.
	run func(ctx context.Context, cmd string) (string, error)
}

// NewAccess wires the service over already-resolved seams. A nil cfg or an
// unresolved ServerDir still yields a usable Access: file-backed operations
// fail with a clear "server dir not resolved" error rather than panicking.
func NewAccess(cfg *Config, console *Console, trail *Trail) *Access {
	return &Access{Cfg: cfg, Console: console, Trail: trail, euid: os.Geteuid, run: realRunCmd}
}

// serverDir returns the resolved server directory or a clear error.
func (a *Access) serverDir() (string, error) {
	if a.Cfg == nil || a.Cfg.ServerDir == "" {
		return "", errors.New("server dir not resolved")
	}
	return a.Cfg.ServerDir, nil
}

// propsPath is the path to server.properties in the server dir.
func (a *Access) propsPath() string { return filepath.Join(a.Cfg.ServerDir, "server.properties") }

// allowlistPath is the path to allowlist.json in the server dir.
func (a *Access) allowlistPath() string { return filepath.Join(a.Cfg.ServerDir, "allowlist.json") }

// Gate reads the durable gate setting from server.properties (read-only). It
// never touches the console: this is the on-disk truth a restart enforces.
func (a *Access) Gate() (GateState, error) {
	dir, err := a.serverDir()
	if err != nil {
		return GateState{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "server.properties"))
	if err != nil {
		return GateState{}, fmt.Errorf("read server.properties: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		v, ok := cutKey(t, "allow-list=")
		if !ok {
			continue
		}
		return GateState{Enabled: strings.EqualFold(strings.TrimSpace(v), "true"), Source: filepath.Join(dir, "server.properties")}, nil
	}
	// Key absent: BDS defaults the allowlist gate off.
	return GateState{Enabled: false, Source: filepath.Join(dir, "server.properties")}, nil
}

// List reads the current allowlist straight from allowlist.json (read-only).
// An empty or null file yields an empty slice, not an error.
func (a *Access) List() ([]AllowEntry, error) {
	dir, err := a.serverDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "allowlist.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // no file yet = empty allowlist
		}
		return nil, fmt.Errorf("read allowlist.json: %w", err)
	}
	return parseAllowlist(data)
}

// SetGate flips the gate to enabled. It writes server.properties atomically
// (temp+rename, preserving every other line) AND sends `allowlist on|off` so
// a running server applies it immediately without a restart — keeping the
// durable and runtime layers in sync. The returned detail is a toast note;
// err is non-nil only when the durable write itself failed (a live-apply
// failure degrades to a note, since the setting is already persisted).
func (a *Access) SetGate(ctx context.Context, enabled bool) (string, error) {
	if _, err := a.serverDir(); err != nil {
		a.audit("gate", map[string]string{"enabled": bstr(enabled)}, err)
		return "", err
	}
	if err := a.writeGate(enabled); err != nil {
		a.audit("gate", map[string]string{"enabled": bstr(enabled)}, err)
		return "", err
	}
	cmd := "allowlist off"
	if enabled {
		cmd = "allowlist on"
	}
	var detail string
	switch {
	case a.Console == nil:
		detail = "persisted; applies on next start (no console)"
	default:
		if _, cerr := a.Console.Send(ctx, cmd); cerr != nil {
			detail = "persisted; live apply failed (" + cerr.Error() + ") — applies on next start"
		} else {
			detail = "gate " + bstr(enabled) + ", applied live and persisted"
		}
	}
	a.audit("gate", map[string]string{"enabled": bstr(enabled)}, nil)
	return detail, nil
}

// Add puts identity (a gamertag or XUID) on the allowlist. It reads the file,
// appends the entry, writes atomically (temp+rename), reloads it into a
// running server, and verifies against the server's own `allowlist list`. A
// duplicate is rejected before any write. The returned detail is a toast note.
func (a *Access) Add(ctx context.Context, identity string) (string, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return "", errors.New("empty identity")
	}
	dir, err := a.serverDir()
	if err != nil {
		a.audit("allow-add", map[string]string{"identity": identity}, err)
		return "", err
	}
	entries, err := readAllowlistFile(dir)
	if err != nil {
		a.audit("allow-add", map[string]string{"identity": identity}, err)
		return "", err
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name, identity) {
			err := fmt.Errorf("%s is already on the allowlist", identity)
			a.audit("allow-add", map[string]string{"identity": identity}, err)
			return "", err
		}
	}
	entries = append(entries, AllowEntry{Name: identity})
	if err := writeAllowlistFile(dir, entries); err != nil {
		a.audit("allow-add", map[string]string{"identity": identity}, err)
		return "", err
	}
	detail, verr := a.reloadAndVerify(ctx, identity, true)
	if verr != nil {
		a.audit("allow-add", map[string]string{"identity": identity}, verr)
		return "", verr
	}
	a.audit("allow-add", map[string]string{"identity": identity}, nil)
	return detail, nil
}

// Remove takes identity (a gamertag or XUID) off the allowlist. It reads the
// file, drops the matching entry (case-insensitive), writes atomically,
// reloads into a running server, and verifies it is gone from `allowlist
// list`. Removing an absent identity is rejected before any write.
func (a *Access) Remove(ctx context.Context, identity string) (string, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return "", errors.New("empty identity")
	}
	dir, err := a.serverDir()
	if err != nil {
		a.audit("allow-remove", map[string]string{"identity": identity}, err)
		return "", err
	}
	entries, err := readAllowlistFile(dir)
	if err != nil {
		a.audit("allow-remove", map[string]string{"identity": identity}, err)
		return "", err
	}
	var kept []AllowEntry
	found := false
	for _, e := range entries {
		if strings.EqualFold(e.Name, identity) {
			found = true
			continue
		}
		kept = append(kept, e)
	}
	if !found {
		err := fmt.Errorf("%s is not on the allowlist", identity)
		a.audit("allow-remove", map[string]string{"identity": identity}, err)
		return "", err
	}
	if err := writeAllowlistFile(dir, kept); err != nil {
		a.audit("allow-remove", map[string]string{"identity": identity}, err)
		return "", err
	}
	detail, verr := a.reloadAndVerify(ctx, identity, false)
	if verr != nil {
		a.audit("allow-remove", map[string]string{"identity": identity}, verr)
		return "", verr
	}
	a.audit("allow-remove", map[string]string{"identity": identity}, nil)
	return detail, nil
}

// Reload re-reads allowlist.json into a running server (`allowlist reload`)
// and reports the resulting entry count. It is how an operator picks up edits
// made outside the tool. With no console it degrades to a note (the file is
// already authoritative on disk).
func (a *Access) Reload(ctx context.Context) (string, error) {
	if _, err := a.serverDir(); err != nil {
		a.audit("reload", nil, err)
		return "", err
	}
	detail, verr := a.reloadAndVerify(ctx, "", false)
	if verr != nil {
		a.audit("reload", nil, verr)
		return "", verr
	}
	a.audit("reload", nil, nil)
	return detail, nil
}

// reloadAndVerify sends `allowlist reload` (when a console exists) and then
// confirms the running server agrees with what we wrote: for wantPresent it
// requires identity to appear in `allowlist list`, otherwise to be absent.
// With no console it verifies against the file instead and notes that the
// change applies on next start. Returns a short toast detail.
func (a *Access) reloadAndVerify(ctx context.Context, identity string, wantPresent bool) (string, error) {
	if a.Console == nil {
		n, err := a.countEntries()
		if err != nil {
			return "", fmt.Errorf("verify after write: %w", err)
		}
		return fmt.Sprintf("%d entr%s on allowlist (applies on next start)", n, plural(n)), nil
	}
	if _, err := a.Console.Send(ctx, "allowlist reload"); err != nil {
		return "", fmt.Errorf("reload: %w", err)
	}
	out, lerr := a.Console.Send(ctx, "allowlist list")
	if lerr != nil {
		return "", fmt.Errorf("verify (allowlist list): %w", lerr)
	}
	present := identity != "" && strings.Contains(out, identity)
	switch {
	case wantPresent && !present:
		return "", fmt.Errorf("server did not reflect the add (%s missing from allowlist list)", identity)
	case !wantPresent && present:
		return "", fmt.Errorf("server still lists %s after remove", identity)
	}
	n, cerr := a.countEntries()
	if cerr != nil {
		return "", fmt.Errorf("verify count: %w", cerr)
	}
	return fmt.Sprintf("%d entr%s on allowlist (reloaded)", n, plural(n)), nil
}

// countEntries reads the file and returns how many entries it holds.
func (a *Access) countEntries() (int, error) {
	dir, err := a.serverDir()
	if err != nil {
		return 0, err
	}
	e, err := readAllowlistFile(dir)
	if err != nil {
		return 0, err
	}
	return len(e), nil
}

// BlockIP adds a host-firewall deny rule for ip on the server's UDP port. When
// unprivileged (the common case — euid != 0) it does not attempt the command;
// it returns Applied=false with Reason and the exact Command so the operator
// can run it themselves. When privileged it executes the command and reports
// success or a degraded result on failure. Audit-logged either way.
func (a *Access) BlockIP(ctx context.Context, ip string) (*IPBlockResult, error) {
	return a.firewall(ctx, "block", ip)
}

// UnblockIP removes a previously-added host-firewall deny rule for ip. Same
// graceful-degradation contract as BlockIP.
func (a *Access) UnblockIP(ctx context.Context, ip string) (*IPBlockResult, error) {
	return a.firewall(ctx, "unblock", ip)
}

// firewall is the shared block/unback implementation: validate the IP, build
// the exact command, then apply it when privileged or surface it otherwise.
func (a *Access) firewall(ctx context.Context, action, ip string) (*IPBlockResult, error) {
	ip = strings.TrimSpace(ip)
	if net.ParseIP(ip) == nil {
		err := fmt.Errorf("not a valid IP address: %q", ip)
		a.audit(action+"-ip", map[string]string{"ip": ip}, err)
		return nil, err
	}
	port := 0
	if a.Cfg != nil {
		port = a.Cfg.Port
	}
	cmd := firewallCommand(action, ip, port)
	res := &IPBlockResult{Command: cmd}

	if a.euid() != 0 {
		res.Applied = false
		res.Reason = "requires root (run as root or via sudo)"
		a.audit(action+"-ip", map[string]string{"ip": ip, "applied": "false"}, nil)
		return res, nil
	}

	out, err := a.run(ctx, cmd)
	if err != nil {
		res.Applied = false
		res.Reason = "command failed: " + strings.TrimSpace(out)
		a.audit(action+"-ip", map[string]string{"ip": ip, "applied": "false"}, errors.New(res.Reason))
		return res, nil
	}
	res.Applied = true
	a.audit(action+"-ip", map[string]string{"ip": ip, "applied": "true"}, nil)
	return res, nil
}

// audit appends one event per access action (D11: every mutating action is
// recorded). A nil trail (audit disabled) is a no-op.
func (a *Access) audit(action string, params map[string]string, err error) {
	if a.Trail == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error: " + err.Error()
	}
	p := map[string]string{"action": action}
	for k, v := range params {
		p[k] = v
	}
	_ = a.Trail.Append(Event{Verb: "access", Params: p, Result: result})
}

// --- server.properties gate read/write -------------------------------------

// cutKey returns the value after key in line (trimmed) and whether the line
// begins with that key. It is the tiny property-line parser shared by the
// gate readers/writers.
func cutKey(line, key string) (string, bool) {
	if !strings.HasPrefix(line, key) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, key)), true
}

// writeGate sets allow-list=<v> in server.properties atomically (temp+rename),
// preserving every other line verbatim and its order. If the key is absent it
// is appended at the end.
func (a *Access) writeGate(enabled bool) error {
	path := a.propsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read server.properties: %w", err)
	}
	val := "false"
	if enabled {
		val = "true"
	}
	var out []string
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "allow-list=") {
			out = append(out, "allow-list="+val)
			found = true
			continue
		}
		out = append(out, line)
	}
	if !found {
		out = append(out, "allow-list="+val)
	}
	return writeFileAtomic(path, []byte(strings.Join(out, "\n")))
}

// --- allowlist.json read/write ---------------------------------------------

// parseAllowlist decodes allowlist.json. BDS writes a literal `null` for an
// empty list and a compact array otherwise; both (plus `[]`) are accepted.
func parseAllowlist(data []byte) ([]AllowEntry, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	var entries []AllowEntry
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return nil, fmt.Errorf("parse allowlist.json: %w", err)
	}
	return entries, nil
}

// readAllowlistFile reads + parses the allowlist in dir (missing file = empty).
func readAllowlistFile(dir string) ([]AllowEntry, error) {
	data, err := os.ReadFile(filepath.Join(dir, "allowlist.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read allowlist.json: %w", err)
	}
	return parseAllowlist(data)
}

// writeAllowlistFile writes entries to dir/allowlist.json atomically
// (temp+rename). An empty slice is written as a literal `null` to match BDS's
// own on-disk format exactly.
func writeAllowlistFile(dir string, entries []AllowEntry) error {
	var data []byte
	if len(entries) == 0 {
		data = []byte("null")
	} else {
		d, err := json.Marshal(entries)
		if err != nil {
			return fmt.Errorf("marshal allowlist: %w", err)
		}
		data = d
	}
	return writeFileAtomic(filepath.Join(dir, "allowlist.json"), data)
}

// --- shared atomic file write ----------------------------------------------

// writeFileAtomic writes data to path via a temp file in the same directory +
// rename, so a crash mid-write never leaves a torn file (the ticket's
// "atomic: temp+rename" requirement). Mirrors backup.go's manifest writer.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename into place (%s): %w", filepath.Base(path), err)
	}
	return nil
}

// --- host firewall ----------------------------------------------------------

// defaultBedrockPort is the BDS UDP port used when the config does not pin one.
const defaultBedrockPort = 19132

// firewallCommand builds the exact ufw command for blocking/unblocking ip on
// the server's UDP port. ufw is preferred when present on the host. The command is
// always returned so it can be shown to the operator when we lack permission.
func firewallCommand(action, ip string, port int) string {
	if port <= 0 {
		port = defaultBedrockPort
	}
	switch action {
	case "block":
		return fmt.Sprintf("ufw deny from %s to any port %d proto udp", ip, port)
	default: // unblock
		return fmt.Sprintf("ufw delete deny from %s to any port %d proto udp", ip, port)
	}
}

// realRunCmd executes a host command and returns its combined output.
func realRunCmd(ctx context.Context, cmd string) (string, error) {
	parts := strings.Fields(cmd)
	out, err := exec.CommandContext(ctx, parts[0], parts[1:]...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// --- tiny helpers -----------------------------------------------------------

func bstr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
