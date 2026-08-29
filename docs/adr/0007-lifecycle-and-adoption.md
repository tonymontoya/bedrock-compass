# ADR-0007: Lifecycle & adoption — one service over the supervisor, verified before claimed

**Status:** accepted (2026-08-19)
**Decisions ref:** D6 (adopt), D10 (Server tab), D11 (tiers) — ticket #13

`compass.Lifecycle` is the single execution path for start/stop/restart/
status and for adopting an existing deployment. The TUI's Server screen and
the headless verbs (`bedrock-compass start|stop|restart|status|adopt`) are
thin renderers over it (D10/D15 pattern); none of them know about the other.
Mirroring the backup engine (ADR-0006), the service is a thin layer over
already-resolved seams (Supervisor, Console, Discover, Pinger) with
injectable function fields, so the adopt path is testable offline while
production wiring is the real thing.

## Status: supervisor truth first, pong is enrichment

The Server screen's state is the **supervisor's truth** — active state,
main PID, uptime — not the pong. `Status()` degrades by source: a wired
supervisor that fails falls back to the process table (`/proc`), and a
tmux deployment (no supervisor for status) uses the process table
directly. The pong is a best-effort enrichment on top: `Status()` returns
it when the probe answers, and simply omits it (`Pong == nil`) when the
server is unreachable or the probe is not wired. A deployment is never
shown as "stopped" because its pong is down, and a down pong never fails
a status read — the reverse (a running server whose pong is unreachable)
is a normal, expected state.

## Adopt: detect → verify → persist, and verify is mandatory

Adoption is the operator's own case (D6): point the tool at a running
deployment it does not yet know. It runs three steps, in order:

1. **Detect** — `Discover`: find the one running `bedrock_server`
   process (v1 is single-server), resolve its server dir from the
   process, and identify the systemd unit that owns it.
2. **Verify** — against the *discovered* deployment's own channel: a
   pong round-trip (server is answering its own port) **and** a console
   round-trip (`list` returns a roster line, proving the tool can drive
   this server, not just that something is running). Both must succeed
   before the association is returned. This is the deliberate
   difference from "status": adoption claims a deployment, so a
   half-verified one is worse than none.
3. **Persist** — `Adopt` itself never writes the config. It returns an
   `Adopted` result; the caller (`PersistAdopt`) writes it. That
   separation lets the TUI and the headless verb decide where to persist
   without the service knowing. Both use the config file the tool loaded
   (the `--config` path, or the standard path when none was given), so a
   non-standard config keeps working after an adoption; tests use a temp
   path.

`PersistAdopt` writes via `Config.Save`, which is atomic: marshal →
write to `<path>.tmp` → `rename` into place (creating the parent dir).
A partial write can never leave a corrupt config in place, matching the
manifest-as-completion invariant of ADR-0006 (a half-written config is
trash; an atomic rename makes "written" a single moment).

## One seam set, both supervisors

`NewStack` resolves the supervisor, console, and unit once
(`resolveChannels`) and hands all three to `NewLifecycle`. A systemd
deployment resolves to a `systemctl` supervisor (start/stop/status via
`systemctl`, D11: a permission failure surfaces the exact command) and
the unit's console FIFO. A tmux deployment resolves to the tmux
supervisor (stop via a console `stop` + process-exit wait) and the tmux
pane console; **start is unsupported** for tmux (the operator launches
the server in the pane; the tool manages it via the console — ADR-0005,
option B "tool-as-parent" rejected). `Lifecycle` does not branch on
supervisor kind: it calls `Sup.Start/Stop/Status` and the resolved
seam answers, so the service stays supervisor-agnostic.

## Auditing

Every mutating lifecycle action (start, stop, restart, and the adopt
verify/persist) is appended to the audit `Trail` (verb `lifecycle`,
action + result + duration), mirroring `Engine.audit` (ADR-0006): the
append is best-effort and a missing/unwritable trail never blocks the
action itself, but every action is recorded when the trail is present.
`status` is read-only and not audited.

## Consequences

- + TUI Server screen and headless verbs are the same code path; a bug
  fix in one fixes the other.
- + Adopt is safe-by-construction: a deployment is only claimed after
  pong *and* console verification, and only the caller commits it.
- + `Config.Save` atomic rename: a config write is all-or-nothing.
- + `Status` degrades: pong down ≠ server stopped; tmux deployments get
  process-truth without a supervisor.
- - tmux deployments cannot be *started* by the tool (by design,
  ADR-0005); `Lifecycle.Start` surfaces `ErrTmuxStartDeferred` with the
  manual instruction.
- - Adoption requires the pong to answer on the deployment's port; a
  host that blocks it (firewall, or a server not advertising) cannot be
  adopted — the tool refuses rather than claim a deployment it could
  not verify. That is a deliberate, loud failure, not a silent degrade.
