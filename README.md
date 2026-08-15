# bedrock-compass

A single-binary **TUI for running one Minecraft Bedrock Dedicated Server
(BDS) like a sysadmin**: live dashboard (host CPU/RAM/disk/net, players
online), log tail, access control (allowlist gate, kick, IP block), live
backups & restore, config editing, raw console passthrough, and server
lifecycle — all from a terminal you reach over SSH.

> **Status: pre-release.** In the requirements/design phase — see
> [`docs/requirements.md`](docs/requirements.md) for the decision record.

## Why
BDS has no RCON and no admin UI. Every other Bedrock admin tool is a web
panel or a Docker wrapper; none are a local terminal tool that treats the
server the way you'd treat any other service on a box. This one is.

## Design notes (the short version)
- Runs **on the server host**, over SSH, as a normal terminal app.
- Talks to the server through its real surfaces: the **console channel**
  (FIFO under systemd, or a tmux pane), the **save hold/query/resume**
  protocol for live backups, the **journal** for logs, `/proc` for
  telemetry.
- "Banning" on Bedrock = the **allowlist gate** (BDS has no native ban —
  see `docs/research/04-live-verification.md`); the TUI makes it easy to
  find players and add them.
- Can also **install** a server from scratch (systemd or tmux) or **adopt**
  an existing one.

## Repository map
- [`AGENTS.md`](AGENTS.md) — operating manual for humans and agents (read
  first).
- [`CONTEXT.md`](CONTEXT.md) — glossary / ubiquitous language.
- [`docs/requirements.md`](docs/requirements.md) — decision record (D1–D14).
- [`docs/adr/`](docs/adr/) — architecture decisions.
- [`docs/research/`](docs/research/) — sourced research on BDS control
  surfaces, data & backups, and the ecosystem.
- `local/` — operator-specific notes; **gitignored, never committed**.

## Development
See [AGENTS.md §3](AGENTS.md) for the build/test/vet trio and §8 for the
agentic workflow this repo follows.
