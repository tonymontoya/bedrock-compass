# ADR-0009: The raw console is tier-free but always audited

**Status:** accepted (2026-08-25)
**Decisions ref:** D11, ticket #12

## Context
The Console screen (#12) is the tool's **raw passthrough**: whatever an operator
types goes to the server verbatim and its output streams back. This is a
fundamentally different trust surface from every other screen in the TUI.

Every *structured* action (start/stop, kick, gate toggle, allowlist edit, backup)
is a fixed verb with known consequences, so it can be gated behind the shell's
confirmation tiers (D11): TierEnter for low-risk, TierYes ("type YES") for
destructive. The raw console has **no such verbs** — it is an open channel to
the server's command interpreter. An operator could type `stop`, or a
`set-gamemode creative`, or anything else the server accepts. There is no finite
set of consequences to enumerate, so there is nothing for a tier to confirm:
a "type YES" prompt in front of an arbitrary free-text field would be pure
theater — it confirms *that* you are about to type something, not *what*.

What the raw surface does share with every other action is one invariant that
must hold regardless: **it lands in the audit trail** (D11). A raw command is at
least as consequential as any structured verb, so an operator-grade record of
"who typed what, and did it work" is non-negotiable.

## Decision
The `RawConsole` service sends a command verbatim through one `Console.Send`
round-trip and records **every** attempt in the audit trail with verb `"console"`
and the exact command as its param — on success *and* failure (including a nil
channel, so a misconfigured surface still leaves a trace).

The Console screen applies **no confirmation tiers**: typing is itself the
deliberate act. The operator sees exactly what they are about to send (the
buffer), presses Enter, and can see the result in the transcript. Esc returns to
the previous tab; Ctrl+C still quits globally at the shell level.

## Consequences
- + No fake safety theater: a tier on free text would confirm nothing real. The
  operator's own typing is the confirmation.
- + Every raw command is auditable with its exact text and outcome, so the trail
  remains an operator-grade record of everything that reached the server —
  structured *and* raw alike.
- + Degrades cleanly: a deployment with no resolved channel fails fast and clean
  (no hang), and even that failed attempt is recorded.
- − The tool offers no guardrail against a destructive command typed at the raw
  surface. This is accepted by design — it is a passthrough, not a policy
  layer — but it means the audit trail (not prevention) is the safety net for
  what was actually run.
