package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
	"github.com/tonymontoya/bedrock-compass/internal/tui"
)

// version is stamped at release time via -ldflags "-X main.version=vX.Y.Z".
var version = "0.1.0-dev"

func main() {
	if code := run(os.Args); code != 0 {
		os.Exit(code)
	}
}

// run is the entry point. It splits off a subcommand (e.g. `backup-now`) from
// the flags and dispatches. Leading global flags before the subcommand
// (`--config X roster`) are collected and passed through to the verb, whose
// FlagSet re-parses them; with no subcommand it runs the TUI, as before. It
// returns a process exit code.
func run(args []string) int {
	cmd := ""
	var prefix []string // leading flags seen before the subcommand
	rest := args[1:]
	for len(rest) > 0 {
		if strings.HasPrefix(rest[0], "-") {
			p := rest[0]
			prefix = append(prefix, p)
			rest = rest[1:]
			// -config takes a value; keep it with the flag so it is not
			// mistaken for the subcommand.
			if (p == "--config" || p == "-config") && len(rest) > 0 {
				prefix = append(prefix, rest[0])
				rest = rest[1:]
			}
			continue
		}
		cmd = rest[0]
		rest = rest[1:]
		break
	}

	// Top-level help: no subcommand anywhere in the args. Other leading flags
	// (--version) still reach their own handlers via the TUI path below.
	if cmd == "" {
		for _, a := range append(append([]string{}, prefix...), rest...) {
			if a == "-h" || a == "--help" {
				fmt.Print(usageText())
				return 0
			}
		}
	}

	switch cmd {
	case "backup-now":
		return runBackupNow(append(prefix, rest...))
	case "start", "stop", "restart", "status", "adopt":
		return runLifecycle(cmd, append(prefix, rest...))
	case "roster":
		return runRoster(append(prefix, rest...))
	case "logs":
		return runLogs(append(prefix, rest...))
	case "allowlist":
		return runAllowlist(append(prefix, rest...))
	case "restore":
		return runRestore(append(prefix, rest...))
	case "audit":
		return runAudit(append(prefix, rest...))
	case "-h", "--help", "help":
		fmt.Print(usageText())
		return 0
	case "":
		// TUI path (the default).
		return runTUI(append(prefix, rest...))
	default:
		fmt.Fprintf(os.Stderr, "bedrock-compass: unknown command %q\n", cmd)
		fmt.Fprintln(os.Stderr, "usage: bedrock-compass [command] [--version] — see --help for the verb list")
		return 2
	}
}

// runLifecycle runs one lifecycle action headlessly (ticket #13): start,
// stop, restart, status, or adopt. It is the single execution path the TUI's
// Server screen and the CLI share — both drive compass.Lifecycle, so a
// scheduled or scripted action behaves exactly like the interactive one.
// `status` is read-only; the others are mutating and exit non-zero on
// failure so scripts can branch on it.
func runLifecycle(cmd string, rest []string) int {
	start := time.Now()
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	config := fs.String("config", "", "config file path (default: the standard path)")
	showVersion := fs.Bool("version", false, "print version and exit")
	jsonOut := fs.Bool("json", false, "status only: print the status as JSON")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // ADR-0010: help is ok (exit 0); usage already printed
		}
		// flag already printed the error + usage.
		return 2
	}
	if *showVersion {
		fmt.Println("bedrock-compass " + version)
		return 0
	}

	cfgPath := *config
	if cfgPath == "" {
		cfgPath = compass.StandardPath()
	}
	cfg, err := compass.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bedrock-compass %s: config: %v\n", cmd, err)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := compass.NewStack(ctx, cfg, version, compass.StackOpts{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "bedrock-compass %s: wiring: %v\n", cmd, err)
		return 1
	}
	if st.Lifecycle == nil {
		fmt.Fprintf(os.Stderr, "bedrock-compass %s: no lifecycle resolved (config did not resolve a supervisor)\n", cmd)
		return 1
	}

	switch cmd {
	case "status":
		s := st.Lifecycle.Status(ctx)
		if *jsonOut {
			out, _ := json.MarshalIndent(s, "", "  ")
			fmt.Println(string(out))
		} else {
			fmt.Printf("state   %s\n", s.State)
			if s.Source != "" {
				fmt.Printf("source  %s\n", s.Source)
			}
			if s.Unit != "" {
				fmt.Printf("unit    %s\n", s.Unit)
			}
			if s.PID > 0 {
				fmt.Printf("pid     %d\n", s.PID)
			}
			if s.Uptime > 0 {
				fmt.Printf("uptime  %s\n", s.Uptime.Round(time.Second))
			}
			if p := s.Pong; p != nil {
				fmt.Printf("pong    %s %s (%d/%d players online)\n", p.Name, p.Version, p.Online, p.MaxPlayers)
			}
		}
		// ADR-0010: every headless invocation appends one audit event — the
		// trail is how a scripted `status` is told apart from a TUI action.
		auditCLI(st.Trail, "status", map[string]string{}, nil, start)
		return 0
	case "adopt":
		a, err := st.Lifecycle.Adopt(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bedrock-compass adopt: %v\n", err)
			auditCLI(st.Trail, "adopt", map[string]string{}, err, start)
			return 1
		}
		if err := st.Lifecycle.PersistAdopt(cfgPath, a); err != nil {
			fmt.Fprintf(os.Stderr, "bedrock-compass adopt: verified but persist: %v\n", err)
			auditCLI(st.Trail, "adopt", map[string]string{"supervisor": a.Supervisor}, err, start)
			return 1
		}
		auditCLI(st.Trail, "adopt", map[string]string{"supervisor": a.Supervisor, "service": a.Service}, nil, start)
		fmt.Printf("adopted %s %s (dir %s)\n", a.Supervisor, a.Service, a.ServerDir)
		return 0
	default: // start | stop | restart
		var run func(context.Context) error
		switch cmd {
		case "start":
			run = st.Lifecycle.Start
		case "stop":
			run = st.Lifecycle.Stop
		case "restart":
			run = st.Lifecycle.Restart
		}
		if err := run(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "bedrock-compass %s: %v\n", cmd, err)
			auditCLI(st.Trail, cmd, map[string]string{}, err, start)
			return 1
		}
		// ADR-0010: lifecycle verbs are the intentional --yes exception (they
		// shipped unattended under ticket #13), but they still audit — one cli
		// event per invocation plus the Lifecycle service's own events.
		auditCLI(st.Trail, cmd, map[string]string{}, nil, start)
		fmt.Printf("%s: ok\n", cmd)
		return 0
	}
}

// runBackupNow runs one backup headlessly and returns a process exit code.
// It is the single execution path the scheduled-backup timer service calls
// (see compass/timer.go's rendered service unit), so the scheduled and
// interactive backups share exactly one code path. Mutating, so it honors the
// ADR-0010 confirmation gate: without --yes it prints what would happen and
// exits 3; the timer's ExecStart carries --yes (creating the schedule is the
// operator's prior explicit decision).
func runBackupNow(rest []string) int {
	start := time.Now()
	fs := flag.NewFlagSet("backup-now", flag.ContinueOnError)
	mode := fs.String("mode", compass.ModeLive, "backup mode: live (running, save protocol) or stopped (cold copy)")
	config := fs.String("config", "", "config file path (default: the standard path)")
	showVersion := fs.Bool("version", false, "print version and exit")
	yes := fs.Bool("yes", false, "confirm this backup (required; ADR-0010)")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // ADR-0010: help is ok (exit 0); usage already printed
		}
		// flag already printed the error + usage.
		return 2
	}
	if *showVersion {
		fmt.Println("bedrock-compass " + version)
		return 0
	}

	st, ctx, cancel, code := wireStack("backup-now", *config)
	if code != 0 {
		return code
	}
	defer cancel()

	params := map[string]string{"mode": *mode}
	if code := gate("backup-now", *yes, "run one "+*mode+" backup (save hold/query/resume protocol)", st.Trail, params, start); code != 0 {
		return code
	}

	id, err := st.BackupNow(ctx, *mode)
	auditCLI(st.Trail, "backup-now", params, err, start)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bedrock-compass backup-now: %v\n", err)
		return 1
	}
	fmt.Printf("backup-now: %s (%s)\n", id, *mode)
	return 0
}

func runTUI(rest []string) int {
	fs := flag.NewFlagSet("bedrock-compass", flag.ContinueOnError)
	showVersion := fs.Bool("version", false, "print version and exit")
	config := fs.String("config", "", "config file path (default: the standard path)")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println("bedrock-compass " + version)
		return 0
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfgPath := *config
	if cfgPath == "" {
		cfgPath = compass.StandardPath()
	}
	cfg, err := compass.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bedrock-compass: config:", err)
		return 1
	}

	app := tui.NewApp()
	var telemetry *compass.Telemetry
	if t, terr := compass.NewTelemetry(ctx, cfg, 2*time.Second); terr == nil {
		telemetry = t
		t.Start(ctx)
	} else {
		fmt.Fprintln(os.Stderr, "bedrock-compass: telemetry degraded:", terr)
	}

	// Status provider (ticket #4): one poller drives the status bar and is
	// broadcast to the active screen, so the dashboard tiles (ticket #5) and
	// the status bar always agree. Degrades to "stopped" if telemetry failed.
	statusProvider := func() compass.Status {
		if telemetry != nil {
			return telemetry.Latest()
		}
		return compass.Status{State: "stopped"}
	}
	app.SetStatusProvider(statusProvider)

	// Recent operations (D11): the audit trail, read-only. A missing trail just
	// means the dashboard panel shows "no operations yet".
	var recent func() []compass.Event
	if trail, err := compass.NewTrail(cfg.AuditFile); err == nil {
		recent = func() []compass.Event {
			evs, rerr := trail.Recent(10)
			if rerr != nil {
				return nil
			}
			return evs
		}
	}

	// Dashboard (ticket #5): swap the placeholder for the real screen.
	app.Screens[0] = tui.NewDashboard(app, recent)

	// Backups (ticket #10): wire the engine + timer so the Backups screen can
	// create/restore/prune backups and manage the scheduled timer. defaultScreens
	// places Backups at index 3 (D10 order: Dashboard Players Access Backups ...),
	// so it must be swapped there — index 1 is the Players placeholder, index 2
	// the Access placeholder.
	// The same resolved Stack also carries the Lifecycle service (ticket #13),
	// which the Server screen (index 7) drives for start/stop/restart/status/adopt,
	// the Players service (ticket #6), which the Players screen (index 1) drives
	// for roster + kick/op/deop/allow, and the Access service (ticket #7), which
	// the Access screen (index 2) drives for gate toggle + allowlist mgmt + IP blocks.
	// The Logs service (#8) is a pure journal reader; the Logs screen (index 4)
	// renders it — reader-only, so no host callbacks are needed. It degrades to
	// an "unavailable" state when no unit resolved (tmux without a unit name).
	// The RawConsole service (#12) is the verbatim passthrough; the Console
	// screen (index 6) drives it and returns to the previous tab on Esc, so it
	// needs the shell as its host for Toast + ExitConsole. It degrades to an
	// "unavailable" state when no channel resolved.
	// The Props service (#11) edits server.properties: the Config screen
	// (index 5) renders the keys with hot/cold markers + per-key allowed
	// values, applies hot keys live through the console (TierEnter), and
	// applies cold keys at the restart tier (TierYes) with the restart in the
	// same action.
	stack, werr := compass.NewStack(ctx, cfg, version, compass.StackOpts{})
	if werr != nil {
		fmt.Fprintln(os.Stderr, "bedrock-compass: backup wiring degraded:", werr)
	} else {
		app.Screens[1] = tui.NewPlayers(app, stack)
		app.Screens[2] = tui.NewAccess(app, stack)
		app.Screens[3] = tui.NewBackups(app, stack)
		app.Screens[4] = tui.NewLogs(stack)
		app.Screens[5] = tui.NewConfig(app, stack)
		app.Screens[6] = tui.NewConsole(app, stack)
		// The Server screen persists adoptions into the config file that was
		// actually loaded (--config path, or the standard path), so a
		// non-standard config keeps working after an adoption.
		srv := tui.NewServer(app, stack)
		srv.SetConfigPath(cfgPath)
		app.Screens[7] = srv
	}

	prog := tea.NewProgram(app, tea.WithAltScreen())
	if _, runErr := prog.Run(); runErr != nil {
		cancel()
		if telemetry != nil {
			<-telemetry.Wait()
		}
		fmt.Fprintln(os.Stderr, "bedrock-compass:", runErr)
		return 1
	}
	cancel()
	if telemetry != nil {
		<-telemetry.Wait()
	}
	return 0
}
