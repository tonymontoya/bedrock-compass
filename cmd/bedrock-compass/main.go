package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

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

	prog := tea.NewProgram(tui.NewApp(), tea.WithAltScreen())
	if _, err := prog.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "bedrock-compass:", err)
		os.Exit(1)
	}
}
