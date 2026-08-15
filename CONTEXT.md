# CONTEXT.md — bedrock-compass glossary

Shared language for this repo. Keep current; it's how a fresh session
orients fast. Terms are the ubiquitous language — code, docs, and
conversation all use them.

## Core

- **BDS** — Bedrock Dedicated Server. The single Minecraft Bedrock server
  process this tool manages. (Java Edition is out of scope.)
- **operator** — the human who runs the server and uses this tool. (Never
  "the user of Minecraft"; the players are not our users.)
- **host** — the machine the server runs on; the tool runs here too.
- **console channel** — the stdin/stdout control path of the server process.
  The *only* native command interface BDS has (there is no RCON). In this
  tool's architecture it is a FIFO wired by the service supervisor (see
  ADR-0001); "send a command" always means "write a line to the console
  channel".
- **host-level** — the tool runs on the server host and uses local facilities
  (files, journal, /proc, firewall) in addition to the console channel.
  Distinguishes us from remote/web managers.

## Access control (D3)

- **allowlist gate** — the deny mechanism: `allow-list=true` plus
  `allowlist.json`; every player *not* listed is rejected at join. This is
  "banning" on Bedrock (which has no native ban).
- **wide open** — `allow-list=false`; anyone authenticated (online-mode) can
  join. The default onboarding posture.
- **gamertag** — the player's display name (Xbox gamertag); how humans
  identify players.
- **XUID** — the player's 64-bit Xbox unique ID; how the server identifies
  players internally. Resolved from a gamertag on first join.
- **roster** — the set of players currently online (name + XUID), obtained
  from the `list` console command / join-leave log lines.

## World & data

- **world dir** — `worlds/<level-name>/`; the LevelDB `db/` plus
  `level.dat`/`levelname.txt`. The unit of backup/restore.
- **live backup** — backup taken *while the server is running*, using the
  official save protocol: `save hold` → copy files listed by `save query` →
  `save resume`. Distinguished from a stopped backup.
- **cold config** — `server.properties` changes that only take effect on
  restart (vs. live-reloadable JSON like allowlist/permissions).

## Lifecycle & supervision

- **supervisor** — whatever keeps the server process alive: systemd
  (recommended) or a tmux/screen session. The tool is supervisor-aware but
  never assumes which one.
- **adopt** — pointing the tool at an existing server deployment (the
  operator's case).
- **provision** — the tool installing a new server: download the official
  zip, extract, accept EULA, create the world, wire the supervisor (ADR-0005).
- **pong / advertisement** — the RakNet `UnconnectedPong` server list reply;
  our read-only liveness + player-count + MOTD probe (no console needed).
