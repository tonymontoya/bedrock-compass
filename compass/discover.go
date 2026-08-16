package compass

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Proc is one entry of the process table.
type Proc struct {
	PID  int
	Name string // comm (executable name)
	Cwd  string // working directory
}

// Discovered is the result of autodiscovery.
type Discovered struct {
	PID  int
	Dir  string
	Unit string // "" when no matching systemd unit exists
}

// Discoverer performs autodiscovery over injectable sources so tests can
// supply their own process table.
type Discoverer struct {
	Procs      func(ctx context.Context) ([]Proc, error)
	UnitForDir func(ctx context.Context, dir string) (string, error)
}

const serverProcName = "bedrock_server"

// Discover locates the single running bedrock_server:
// process -> working dir -> (optionally) owning systemd unit.
func (d *Discoverer) Discover(ctx context.Context) (*Discovered, error) {
	if d.Procs == nil {
		return nil, errors.New("Discoverer.Procs is nil")
	}
	procs, err := d.Procs(ctx)
	if err != nil {
		return nil, fmt.Errorf("read process table: %w", err)
	}
	var servers []Proc
	for _, p := range procs {
		if p.Name == serverProcName {
			servers = append(servers, p)
		}
	}
	switch len(servers) {
	case 0:
		return nil, errors.New("no running bedrock_server found")
	case 1:
		// exactly one, as expected
	default:
		var pids []string
		for _, s := range servers {
			pids = append(pids, strconv.Itoa(s.PID))
		}
		return nil, fmt.Errorf("multiple bedrock_server processes running (pids %s); v1 is single-server, pin server-dir in the config", strings.Join(pids, ", "))
	}

	s := servers[0]
	if s.Cwd == "" {
		return nil, fmt.Errorf("cannot determine the working directory of pid %d", s.PID)
	}
	out := &Discovered{PID: s.PID, Dir: s.Cwd}
	if d.UnitForDir != nil {
		unit, err := d.UnitForDir(ctx, out.Dir)
		if err != nil {
			return nil, fmt.Errorf("lookup unit for %s: %w", out.Dir, err)
		}
		out.Unit = unit
	}
	return out, nil
}

// Discover runs the production wiring: the /proc process table (Linux) and
// a systemd unit lookup.
func Discover(ctx context.Context) (*Discovered, error) {
	return (&Discoverer{
		Procs:      procsFromProc,
		UnitForDir: unitForDir,
	}).Discover(ctx)
}

func procsFromProc(ctx context.Context) ([]Proc, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var procs []Proc
	for _, e := range entries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm := readProcFile(pid, "comm")
		procs = append(procs, Proc{
			PID:  pid,
			Name: strings.TrimSpace(comm),
			Cwd:  procCwd(pid),
		})
	}
	return procs, nil
}

func readProcFile(pid int, what string) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), what))
	if err != nil {
		return ""
	}
	return string(b)
}

func procCwd(pid int) string {
	wd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err != nil {
		return ""
	}
	return wd
}

// unitForDir asks systemd which running service unit has WorkingDirectory ==
// dir. Absence of systemd (or no match) yields an empty string, not an
// error: tmux/nohup deployments are first-class (ADR-0001).
func unitForDir(ctx context.Context, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "list-units", "--type=service", "--state=running", "--no-legend", "--plain").Output()
	if err != nil {
		return "", nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		unit := fields[0]
		wd, err := exec.CommandContext(ctx, "systemctl", "show", unit, "-p", "WorkingDirectory", "--value").Output()
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(wd)) == dir {
			return unit, nil
		}
	}
	return "", nil
}
