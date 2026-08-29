# ADR-0010: Headless verbs confirm with --yes; every invocation is audited

**Status:** accepted (2026-08-26)
**Decisions ref:** D11, ticket #15

## Context
The TUI gates destructive actions behind confirmation tiers (D11): TierEnter for
low-risk, "type YES" for destructive. The headless verbs (ticket #15) have no
operator at a keyboard — a script or the systemd backup timer invokes them
unattended. Two problems sit in tension:

1. A mutating verb invoked by accident (a typo'd cron line, a test run pointed
   at the wrong config) would execute with zero friction.
2. Conversely, the scheduled-backup timer must invoke `backup-now` with no
   human present — an interactive prompt would wedge it forever.

## Decision
- Every headless verb that mutates server or host state — `backup-now`,
  `allowlist add`, `allowlist remove`, `restore` — requires an explicit
  `--yes`. Without it, the verb prints what it *would* do and exits with code
  **3** without touching anything. The flag is a deliberate, scriptable
  confirmation: its presence in a command line is itself the review step.
- Read-only verbs (`status`, `roster`, `logs`, `audit`) run unconditionally.
- `start` / `stop` / `restart` are an intentional exception to the gate: they
  shipped under ticket #13's contract as unattended lifecycle actions, their
  data impact is low (no world or access-list change), and re-gating them now
  would break existing scripts for little safety gain. They remain audited by
  the Lifecycle service as before.
- **Every** headless invocation appends one audit event: verb `"cli"`, params
  `cmd` plus the key arguments, result ok/error, duration. A confirmed mutating
  verb additionally records `bypass="--yes"`; a refused invocation (exit 3) is
  recorded as an error event, so a stray destructive attempt still leaves a
  trace. Service-level events (`access`, `restore`, `backup`, `lifecycle`)
  continue to record the operation itself — the trail shows both the
  *invocation* and its *effect*.
- The rendered timer service passes `--yes` in ExecStart: creating the schedule
  is the operator's prior explicit decision (cadence + mode), so the unattended
  run is pre-confirmed by construction.

## Exit codes
| code | meaning |
|------|---------|
| 0    | ok (including `--help`) |
| 1    | runtime error (config, wiring, server, journal) |
| 2    | usage / flag error |
| 3    | confirmation required but not given |

## Consequences
- + A typo'd or stray invocation of a mutating verb fails loudly (exit 3),
  touches nothing, and leaves an audit trace.
- + Scripts branch cleanly: exit 3 means "ask the operator", distinct from
  failure (1) and usage error (2).
- + The trail distinguishes headless invocations (`cli` events) from TUI
  actions, which have no `cli` event.
- - Mutating verbs produce two events per run (`cli` + service-level). Accepted:
  invocation and operation are different facts, and the bypass note lives on
  the invocation.
- - A timer created before this change would break on upgrade (its ExecStart
  lacks `--yes`, so scheduled backups exit 3). Remedy: re-run the timer's
  Create to re-render the unit; surfaced in the #15 handoff.
