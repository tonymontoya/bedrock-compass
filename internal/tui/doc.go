// Package tui hosts the bubbletea TUI (tickets #3 onward).
//
// It is deliberately an internal package: the domain core (package compass)
// must never import it, so the core stays linkable by a future non-TUI
// consumer (the v2 in-game bridge, D15). The depguard test in compass
// enforces that direction.
package tui
