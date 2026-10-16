# #17 · Assisted upgrade — verify, stage, plan, verify-after

**Label:** `ready-for-agent` (one operator decision required first — see Blocking edges)

## Goal

Compass should assist the operator through a BDS version change instead of leaving it as
a hand-written runbook procedure. It cannot *perform* the privileged part (ADR-0006/D11:
the tool never invokes sudo), so "assist" means: do everything it is allowed to do, print
the exact privileged commands, and then run the verification half itself.

The shape is one command / one screen section with five stages:

1. **Currency** — compare the running version against the latest *stable* release
   (preview builds excluded), so "should I upgrade at all?" is answered from the tool.
2. **Integrity** — the fetched zip must match the known-good hash recorded in config for
   that version, or the recorded hash is captured at fetch time over TLS and persisted;
   there is no official Mojang hash, so the tool's own record is the baseline. Stop on a
   mismatch.
3. **Safety net** — take a live backup and confirm its manifest (not just its exit code),
   keeping the id as the rollback snapshot.
4. **Stage + plan** — extract to a staging dir *outside* the live server dir, hash the
   extracted binary, then print the exact operator commands in the correct order
   (service-only stop, confirm no process, `install -m 775 -o <user> -g <group>`, start)
   including the FIFO warning (never stop the socket) and the transport note.
5. **Verify-after** — after the operator's restart: journal `Version: <new>`, clean boot,
   new-format pong, the transport property, and a real join recorded in the journal. A
   version change is not "done" until something joined.

## Definition of Done

- Currency check reads a running-version value already available to the tool (pong
  advertisement or the journal boot line) and compares against a stated latest-stable
  source; unknown/ambiguous sources are reported as such, never guessed.
- Integrity stage refuses to proceed on hash mismatch, and records the baseline hash in
  config/state on first capture.
- Staging happens outside the live server dir; nothing under the world dir is written or
  deleted (existing guardrail).
- The privileged stage prints commands as data (the D11 pattern already used for ufw),
  with confirmation tier for the destructive parts, and never executes them.
- Verify-after reports each check separately — up / version / pong / transport / join —
  so a stale-client-version failure is not misread as a server fault.
- Every stage is testable offline: currency against a fixture, hash comparison against a
  temp dir with a fake fetch, staging plan against the same fake-exec seam used by
  provisioning (D14), verify-after through the journal + pong seams.
- Runbook `ops/runbooks/upgrade-bds.md` stays the authority for the human steps; the tool
  mirrors it rather than inventing its own order.

## Verification

- `go test ./...`, `go vet ./...`, `gofmt -l .` clean; new tests at the agreed seams
  (currency, integrity, staging plan, verify-after).
- Red-first tests for: hash mismatch refuses; a manifest-less backup is not counted as a
  safety net; a silent fetch failure is loud; join check reports "no join observed" rather
  than success.
- One real assisted run against the scratch rig (never prod mutation beyond a live
  backup), reported by the operator.

## Key references

- `ops/runbooks/upgrade-bds.md` (the procedure this ticket automates half of)
- ADR-0006 (tool never invokes sudo; always-resume invariant), ADR-0010 (headless
  confirmation policy), D11 (surface the exact command), D5 (no operator specifics in
  tracked content), D14 (offline-testable engine seams)
- `docs/research/04-live-verification.md` — verified version/pong behaviour per BDS release

## Blocking edges

- **Operator decision:** can Compass execute the swap when permission exists (polkit rule
  or running as the service user), or does it always stop at printing commands? Current
  design says the latter; this ticket assumes the latter.
- A hash baseline has to live somewhere durable (config or state file) before the
  integrity stage can mean anything.
- Depends on #13 (lifecycle/adopt) for the service facts and on the pong request-format
  fix (this release) for the verify-after probe to work against a current server.
