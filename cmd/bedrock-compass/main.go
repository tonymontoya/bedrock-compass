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
		app.SetStatusProvider(t.Latest)
	} else {
		fmt.Fprintln(os.Stderr, "bedrock-compass: telemetry degraded:", terr)
	}

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
