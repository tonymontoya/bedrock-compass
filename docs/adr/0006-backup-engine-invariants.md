# ADR-0006: Backup engine invariants — always-resume, lock + recovery, manifest as completion

**Status:** accepted (2026-08-16), amended 2026-10-16 (B-04)
**Decisions ref:** D7 (revised), D11 — ticket #9 grill

The backup engine treats `save hold → copy → save resume` as one critical
section with a non-negotiable invariant: **once `save hold` succeeds,
`save resume` runs on every exit path** — copy failure, timeout, context
cancellation, process death. A world left in hold state is the single
worst failure mode (server stuck, players frozen), so the engine trades
"backup completes" for "server is never stuck": a failed copy resumes the
world *first*, then reports the error.

**Amended 2026-10-16 (B-04, verified live on prod):** the invariant also covers a hold
that was dispatched but whose response could not be observed. Harvesting read silence as
"settled" 500 ms after the send while `Saving...` landed at 641 ms, so the engine
reported `save hold: unexpected response ""` and left the world in an active hold — no
lock, no manifest, nothing to detect, and no resume because the hold had never been
*confirmed*. Two rules follow: a required-response command is complete only after its
first line (silence cannot settle it), and when no response was observed after dispatch
the engine closes the window it may itself have created. If the server did answer with
something other than the hold confirmation, no hold exists and the engine must still not
invent a resume.

To make process death recoverable, every mutating engine operation (live
backup, stop-and-copy, restore) holds a lock file in the backup dir, with
an atomic claim, recording pid, operation, and phase (`hold` / `copying` /
`resuming` / …). A lock whose holder is dead — or alive but not this
binary, guarding against pid reuse — is stale. The startup self-check
*reports* what finishing would do; a one-key action then sends
`save resume` when the interrupted op was inside the hold window, cleans
the partial backup, and clears the lock. The engine never auto-resumes
without a human trigger, and never auto-starts a server during recovery.
Concurrent starts **fail fast** ("backup in progress") rather than queue —
a silently delayed backup is a surprise.

A backup dir is a valid backup only when its manifest parses; the
manifest is written **last, atomically (temp + rename)**. Manifest-less
dirs are incomplete trash: `List()` ignores them and retention prunes
them. Restore follows a fixed order — verify manifest → stop (verify) →
safety-net backup → wipe-then-copy the backup's world + config → start
(verify) — and every abort path *after our own stop* attempts to restart
the server, so no mutation ever leaves the deployment stopped by accident.

## Considered options

- **Remote push (rsync-over-SSH) in the engine** (original D7): removed
  2026-08-16 — the operator's off-host path is a NAS mount or a manual
  copy; an engine push channel would be an unused, silently-failing
  surface. D7 revised.
- **Wait/queue for a busy lock**: rejected — fail fast is safer than a
  surprise delay.
- **Auto-retry `save hold` on "a previous save has not been completed"**:
  rejected — loud failure; the timer retries on its next schedule.
- **Restore world-only (keep current config)**: rejected — the backup's
  full set (world + config) is one consistent point in time; the
  safety-net already preserved the current state, so nothing is lost.
- **Supervisor seam in #9:** restore cannot exist without stop/start, and
  a dead server has no console, so #9 introduces the minimal
  `Stop` / `Start` / `Running` seam (systemd complete; tmux stop via
  console, tmux start deferred to #13 with a loud error). Ticket #13
  owns and extends it (adopt, restart, status truth).
- **Tool invokes sudo**: rejected — no password prompts in a TUI; the
  tool runs `systemctl` as the current user and, on denial, surfaces the
  exact command (D11 pattern, same as ufw).

## Consequences

+ A stuck-in-hold server is always detectable (stale lock + phase) and
  finishable in one action; a hold the engine dispatched but could not confirm is
  closed by the engine itself before it reports the failure.
+ "Backup" is unambiguous: manifest present ⇔ restorable.
+ The engine stays TUI-free (D15) and fully testable offline with fake
  console/supervisor (D14).
- The operator needs read/write on the server dir and permission to manage
  the unit (sudo or a polkit rule scoped to it); neither is required to
  *install* or run read-only features.
- A required-response command that gets no reply now costs the caller up to the harvest
  Timeout instead of the Quiesce window; that is the price of not mistaking silence for
  an answer, which is why silent-success commands (kick/op/reload) keep quiet-settle.
- Moving the unit to a dedicated user (e.g. `User=minecraft`) is a later
  deployment hardening (#13/#14): shared-group ownership of the server
  tree must be set up then, or restore writes break.
