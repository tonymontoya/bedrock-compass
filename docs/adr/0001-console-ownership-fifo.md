# ADR-0001: Console ownership — supervisor-owned process, FIFO console channel

**Status:** accepted (2026-08-15)
**Decisions ref:** D2

## Context
BDS has no RCON; the console (stdin/stdout) is its only native command
interface. A supervisor-less console (the operator's current setup:
`Type=Simple`, no `StandardInput=`) means the console is dead — commands
cannot be injected, and "send a command" features (kick, allowlist, save
protocol, `stop`) would be impossible. Options considered:

- **A. Supervisor-owned + FIFO** (`StandardInput=socket`,
  `ListenFIFO=/run/bedrock-console.fifo`): the proven MCscripts pattern.
  Tool writes `echo cmd > fifo`; reads output from the journal.
- **B. Tool-as-parent** (tool spawns the server, owns the pipe): the
  bedrock-server-manager pattern. Loses boot-time auto-start, and the tool
  dies the server.
- **C. Do nothing** — degraded read-only tool (pong + files only).

## Decision
**A.** The supervisor (systemd recommended) owns the process; the console is
exposed as a FIFO via a one-time unit change. One-time restart was accepted
by the operator. For tmux-supervised deployments (see ADR-0005), the
"console channel" is the tmux pane — the TUI sends commands via
`tmux send-keys` and reads via `tmux capture-pane`. The tool abstracts
behind one `console` interface so both work.

## Consequences
- + Tool keeps full command capability; supervisor keeps restart policy.
- + Works identically for adopted and provisioned servers.
- - Requires the one-time unit change (done once, documented).
- - Journal reading requires access to the systemd journal (local root or
  `systemd-journal` group) — a documented install requirement.
