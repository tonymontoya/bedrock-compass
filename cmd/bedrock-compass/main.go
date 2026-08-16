package main

import (
	"flag"
	"fmt"
)

// version is stamped at release time via -ldflags "-X main.version=vX.Y.Z".
var version = "0.1.0-dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("bedrock-compass " + version)
		return
	}

	fmt.Println("bedrock-compass " + version + " — scaffolding only; the TUI arrives with ticket #3.")
}
