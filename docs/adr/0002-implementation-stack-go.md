# ADR-0002: Implementation stack — Go + charm

**Status:** accepted (2026-08-15)
**Decisions ref:** D4

## Context
The tool deploys to a game-server host that may have no toolchains (the
operator's host: no Go/Rust; Python 3.14 + Node present). Options: Go
(static binary), Python (textual; needs a runtime + deps on the host),
Rust (no toolchain experience).

## Decision
**Go** with the charm stack (bubbletea/lipgloss).

## Consequences
- + One static binary; deploy = copy + run; zero host runtime dependencies.
- + Reuses the operator's existing Go raknet code (v2 in-game client).
- + Native /proc, journald-socket, and FIFO access.
- - TUI code is more verbose than textual; charm must be the only
  non-stdlib TUI dependency.
- - Built on a dev box; the host never compiles.
