package main

import (
	"context"
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

// run is the entry point. It splits off a leading subcommand (e.g.
// `backup-now`) from the flags and dispatches. With no subcommand it runs
// the TUI, as before. It returns a process exit code.
func run(args []string) int {
	cmd := ""
	rest := args[1:]
	for len(rest) > 0 {
		if strings.HasPrefix(rest[0], "-") {
			break
		}
		cmd = rest[0]
		rest = rest[1:]
		break
	}

	switch cmd {
	case "backup-now":
		return runBackupNow(rest)
	case "":
		// TUI path (the default).
		return runTUI(rest)
	default:
		fmt.Fprintf(os.Stderr, "bedrock-compass: unknown command %q\n", cmd)
		fmt.Fprintln(os.Stderr, "usage: bedrock-compass [backup-now] [--version]")
		return 2
	}
}

// runBackupNow runs one backup headlessly and returns a process exit code.
// It is the single execution path the scheduled-backup timer service calls
// (see compass/timer.go's rendered service unit), so the scheduled and
// interactive backups share exactly one code path.
func runBackupNow(rest []string) int {
	fs := flag.NewFlagSet("backup-now", flag.ContinueOnError)
	mode := fs.String("mode", compass.ModeLive, "backup mode: live (running, save protocol) or stopped (cold copy)")
	config := fs.String("config", "", "config file path (default: the standard path)")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(rest); err != nil {
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
		fmt.Fprintf(os.Stderr, "bedrock-compass backup-now: config: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := compass.NewStack(ctx, cfg, version, compass.StackOpts{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "bedrock-compass backup-now: wiring: %v\n", err)
		return 1
	}

	id, err := st.BackupNow(ctx, *mode)
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
	// so it must be swapped there — index 1 is the Players placeholder.
	stack, werr := compass.NewStack(ctx, cfg, version, compass.StackOpts{})
	if werr != nil {
		fmt.Fprintln(os.Stderr, "bedrock-compass: backup wiring degraded:", werr)
	} else {
		app.Screens[3] = tui.NewBackups(app, stack)
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
