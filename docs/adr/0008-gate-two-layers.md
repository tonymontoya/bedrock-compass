# ADR-0008: The allowlist gate is two layers; a toggle flips both

**Status:** accepted (2026-08-21)
**Decisions ref:** D3a, ticket #7

## Context
The allowlist gate — Bedrock's deny mechanism — lives in **two places**, and
they can disagree. Verified empirically on the scratch rig (bds):

- **Durable layer**: `allow-list=true|false` in `server.properties`. This is
  what a *restart* enforces. Editing it does not affect a running server, and
  `reloadconfig` does **not** apply it at runtime (probed: the property changed
  on disk but the live gate did not move).
- **Runtime layer**: the console command `allowlist on|off`. This flips the
  *running* server's gate immediately ("Turned on the allowlist") but is **not
  persisted** — `server.properties` keeps its old value, so a restart reverts
  to the durable setting.

So an operator who only toggles one layer gets a surprising state: flip the
console and it works until the next restart; edit the property and nothing
happens until the next restart. The Access screen (#7) must not hide this —
and, per the operator-onboarding constraint (D3a), the tool must **never**
auto-flip the gate on its own.

## Decision
The Access service's `SetGate` writes **both** layers in one action:

1. It sets `allow-list=` in `server.properties` atomically (temp+rename,
   preserving every other line) — the durable truth a restart enforces.
2. It sends `allowlist on|off` to the console so a running server applies it
   immediately without a restart.

The screen displays the **durable** value (`server.properties`) as the gate's
state, because that is what survives a restart and is the operator's intent. A
live-apply failure (no console, e.g. a stopped server) degrades to a note —
"persisted; applies on next start" — rather than an error, since the durable
write already succeeded. The toggle itself is TierYes with a prominent
consequence ("This REJECTS EVERY player not on the allowlist"), and the tool
never changes it without that explicit action.

## Consequences
- + One operator action keeps both layers in sync; no "it worked but reverted
  after restart" surprise.
- + Displaying the durable value is honest: it is what a restart enforces, so
  the screen never shows a gate state that won't stick.
- + Degrades cleanly when there is no console (stopped server): the file write
  still works and applies on next start.
- − A running server whose runtime layer was flipped out-of-band by another
  tool will show a durable value that differs from its live behavior until the
  operator toggles again or restarts. The screen shows the durable truth; this
  is an acceptable, documented edge (the console command is the only other way
  to move the runtime layer).
