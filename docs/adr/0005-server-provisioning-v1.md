# ADR-0005: Server provisioning in v1

**Status:** accepted (2026-08-15)
**Decisions ref:** D6

## Context
Users may install the manager before any server exists. The tool should be
able to create the deployment, not just adopt one.

## Decision
v1 includes **provision**: download the official server zip from Mojang's
endpoint, extract, accept the EULA, create the world, and wire a supervisor
chosen by the user:

- **systemd** (recommended) — unit per the ADR-0001 console pattern,
  `Restart=on-failure`, journal logging.
- **tmux** — session with a human console; commands via `tmux send-keys`.
- **nohup** — fallback only.

And **adopt**: detect an existing deployment (process, service, or
directory) and point the tool at it — the operator's case.

## Consequences
- + One tool covers both "fresh host" and "existing server" onboarding.
- - Mojang's zip URL and EULA flow change per version — must be
  version-tolerant and verified against the live endpoint.
- - Supervisor wiring means writing system config (units, tmux) — behind
  explicit user confirmation with the exact config shown first.
