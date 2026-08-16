package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
	"github.com/tonymontoya/bedrock-compass/internal/tui"
)

// version is stamped at release time via -ldflags "-X main.version=vX.Y.Z".
var version = "0.1.0-dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	headless := flag.Bool("headless", false, "reserved for headless verbs (ticket #15)")
	flag.Parse()

	if *showVersion {
		fmt.Println("bedrock-compass " + version)
		return
	}
	if *headless {
		fmt.Println("bedrock-compass " + version + " — headless verbs arrive with ticket #15.")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := compass.LoadStandard()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bedrock-compass: config:", err)
		os.Exit(1)
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

	prog := tea.NewProgram(app, tea.WithAltScreen())
	if _, runErr := prog.Run(); runErr != nil {
		cancel()
		if telemetry != nil {
			<-telemetry.Wait()
		}
		fmt.Fprintln(os.Stderr, "bedrock-compass:", runErr)
		os.Exit(1)
	}
	cancel()
	if telemetry != nil {
		<-telemetry.Wait()
	}
}
