# Requirements — bedrock-compass

Status: in-grilling (grill-with-docs session, 2026-08-15). Decisions marked
resolved are confirmed with the operator; open items are pending.
Operator-specific values live in `local/notes.md` (gitignored) — this file
must stay open-source-safe.

## Confirmed

### D1 — Scope & roadmap (resolved)
- **v1:** single-server, host-level TUI running on the BDS host, reached over
  SSH. Features: backup + restore, log viewing, access restriction
  (allowlist / online-mode / player cap), player deny/kick, host telemetry
  (CPU/RAM/disk/net), live player roster, raw console passthrough, server
  lifecycle (start/stop/restart).
- **v2:** in-game admin client and/or web UI (deferred; architecture must not
  foreclose it).
- **v3+:** multi-server.
- The "Admin Suite – Server Tools" pack on CurseForge is *inspiration only*,
  not a feature spec (see research/03 §5).

### D2 — Console ownership (resolved)
Keep systemd as the process owner. The unit is upgraded once to expose a
permanent console (MCscripts pattern):
```
[Service]
StandardInput=socket
[Socket]
ListenFIFO=/run/bedrock-console.fifo
```
TUI writes commands: `echo "cmd" > /run/bedrock-console.fifo`; reads output
from the journal (or a dedicated log file). A one-time restart was approved
by the operator (nobody online at the time).

### D3 — Deny/ban = allowlist-based (resolved)
Bedrock has no native ban (verified on the live 1.26.43 binary — see
research/04). The "deny" feature is **allowlist management**:
- **D3a:** allowlist gating is the primary deny mechanism (toggle,
  add/remove, live reload).
- **D3b (operator requirement):** the TUI must make it *easy to identify*
  players and add them. The operator's original blocker was not knowing how
  to obtain players' XUIDs. Design must eliminate that:
  * live roster via console `list` (name + xuid of online players) with
    one-action "allow" per player;
  * "recently connected" list mined from journal lines
    `Player connected: <name>, xuid: <n>` (allow after they leave);
  * plain gamertag entry — `allowlist add <gamertag>` resolves the XUID on
    first join, so no XUID knowledge is ever required.
- **D3c:** host-firewall IP block (ufw/iptables) as a secondary per-IP deny;
  `kick` for immediate ejection.
- **Constraint (operator):** while onboarding new players the live server
  stays `allow-list=false` (wide open) until the operator flips it himself.
  General principle: the TUI is read-only by default; every mutation is an
  explicit user action.

### D4 — Implementation stack (resolved: Go)
**Go** with the charm stack (bubbletea/lipgloss) for the TUI. Rationale:
single static binary that deploys by `scp` + run (the host may have no
toolchains); reuses the operator's existing Go raknet code for the v2
in-game client; operator is fluent in Go. Consequences:
- Build on a dev box; ship one static binary.
- Host-side runtime deps: none (libc only).
- systemd integration, journal reading (journald socket), and /proc
  telemetry are all native to Go.

### D5 — Open-source hygiene (resolved)
The project will be open-sourced. **Tracked content must be free of operator
specifics** (IPs, hostnames, usernames, local paths, world names). Rules:
- `.gitignore` aggressively excludes private material (`local/`, `*.local.*`,
  `.env`, build artifacts).
- Tracked docs refer to private values as "see local/notes.md".
- The tool itself reads its environment (paths, service name, ports) from a
  config file / CLI flags / autodiscovery — never hardcoded operator values.

### D6 — Server provisioning (resolved: in scope for v1)
If the user installs the manager first and no server exists yet, the TUI can
**install a Bedrock server**: download the official server zip (Mojang
endpoint), extract, set EULA, create the world, and wire up a supervision
method — with a choice of:
- **systemd** (recommended; auto-restart, journal logs, FIFO console per D2),
- **tmux/screen** (human console session; simplest, no init integration),
- (nohup/daemon as a fallback — lowest priority).
The tool must also **adopt** an existing server (detect the running binary /
service) — the operator's case.

### D7 — Backup & restore (resolved 2026-08-15)
- **Live by default:** `save hold` → copy exactly the files `save query`
  lists → `save resume`. Players feel a short pause; server stays up.
- **"Stop-and-copy" mode** as an explicit alternative (disconnected, zero
  risk, faster).
- **Scope:** world dir + core config (`server.properties`,
  `allowlist.json`, `permissions.json`, `packetlimitconfig.json`).
  behavior/resource **packs excluded by default**, opt-in flag.
- **Destination:** local `backups/` dir **outside** the server dir
  (configurable); **retention: keep last 7** (configurable), oldest pruned.
- **Remote copy: removed (revised 2026-08-16, operator).** No push channel
  in the engine. Off-host copies are an operator concern: point
  `backup-dir` at a NAS share (SMB/NFS mount) for automatic off-host
  backups, or copy the flat `<id>` backup dirs manually (e.g. scp).
- **Triggers:** on-demand (TUI) **and** a systemd timer for scheduled
  backups (in v1).
- **Restore:** stop server → safety-net backup of current state → swap in
  chosen backup → start. Type-to-confirm (top confirmation tier).
- Backups are self-describing (manifest: timestamp, version, scope, sizes)
  so the TUI can list/inspect them.

### D8 — Telemetry (resolved 2026-08-15)
Dashboard tiles (2 s refresh; /proc + pong, no invented metrics):
server state (pong + supervisor: running/stopped, version, MOTD, uptime),
players online/max (pong + `list`), host CPU (`/proc/stat` delta),
host RAM (`/proc/meminfo`), server-process RSS (`/proc/<pid>/status`),
disk (`statfs` on server dir), net up/down rate (`/proc/net/dev` delta).
**No TPS/FPS** — Bedrock has none; world activity may be surfaced via the
journal's AutoCompaction cadence, nothing fabricated.

### D9 — Log viewing (resolved 2026-08-15)
- **Primary source: systemd journal** via the journald socket (rotation-proof).
- **Fallback: log file** for tmux-supervised deployments (tmux `pipe-pane`
  to a file, set up by the provisioning path).
- Screen: live tail + ~5,000-line scrollback, **level filter** (INFO/WARN/
  ERROR), **text search**, pause, **historical queries by time range**
  (last hour / today / custom).
- Join/leave lines are parsed into the roster's "recently connected" list
  (shared parser with D3b).
- The tool is a **reader** of logs — never a writer or rotator.

### D10 — TUI layout & interaction (resolved 2026-08-15)
Screens (tab row): **Dashboard** (telemetry + quick actions), **Players**
(roster + recently connected; kick/allow/op), **Access** (allowlist gate,
allowlist mgmt, IP blocks, op/deop), **Backups** (list,
create live/stopped, restore, timer), **Logs** (D9), **Config**
(server.properties editor with hot/cold markers per key), **Console**
(raw passthrough, Esc back), **Server** (lifecycle, supervisor,
provision/adopt).
Persistent status bar: server state, name, version, online/max, host cpu/ram.
Keyboard-first; `/` search; **confirmation tiers**: Enter (trivial) →
type `YES` (risky: stop/restart) → type target name (destructive: restore).
Toast notifications for async results.
**Headless verbs** (same binary, scriptable): `status`, `roster`,
`backup now`, `restore <id>`, `logs --level ERROR`, …

### D11 — Safety & ops (resolved 2026-08-15)
- **Confirmation tiers:** Enter (allowlist/op/kick-one) → type `YES`
  (stop/restart) → type the exact target (restore). Every tier shows exactly
  what will happen first.
- **Audit trail (JSONL), kept:** every mutating action appended to
  `~/.local/state/bedrock-compass/audit.jsonl` (timestamp, verb, params,
  result, duration); "recent operations" panel on Dashboard;
  `bedrock-compass audit` headless verb. Doubles as the allowlist history.
- **Save-protocol crash safety:** hold→copy→resume executed as one fast
  critical section; backup **lock file** shared by TUI + timer; **startup
  self-check** offers one-key "finish the interrupted save" if a stale lock
  is found (prevents a stuck `save hold`).
- **Atomic config writes:** temp file + validate + rename (never a
  half-written allowlist.json).
- **Graceful degradation:** no root required for core ops; journal needs
  `systemd-journal` group (installer adds it, documented); root-only actions
  (ufw) show the exact command when permission is missing.
- **SSH disconnect:** plain foreground process; SIGTERM handler cancels
  non-critical work, never abandons an in-flight save resume; no daemons.

### D12 — Project name & home (resolved 2026-08-15)
Project name: **bedrock-compass**. Binary: `bedrock-compass`. Repo:
`github.com/tonymontoya/bedrock-compass` (Go module
`github.com/tonymontoya/bedrock-compass`).

### D13 — Distribution & install (resolved 2026-08-15)
- **GitHub Releases**: one static binary + `config.example.yaml`.
- `install.sh` (raw.githubusercontent): binary → `/usr/local/bin`, starter
  config, one-time `sudo` to add the user to `systemd-journal`.
  Uninstall = remove binary; `~/.local/state` survives.
- **Config** `~/.config/bedrock-compass/config.yaml` (server dir, service /
  tmux session, backup dir + retention, ports) — but
  **autodiscovery first**: `bedrock_server` process → working dir →
  matching systemd unit; config only overrides. One-server boxes need
  zero config.
- No root to install; the tool is only ever a foreground process.

### D14 — Testing strategy (resolved 2026-08-15)
- **Unit (TDD)**: pong parser, log-line parser, server.properties
  reader/writer, allowlist JSON ops, backup manifest, audit log, config
  validation — `go test ./...`, offline.
- **Integration against a real scratch server on the same host** (approved
  by operator): second BDS instance under its own systemd unit
  (`bedrock-compass-test`), scratch dir + port (e.g. 19142), never the live
  server; covers lifecycle, console round-trips, allowlist ops,
  save-hold/query/resume, backup→restore round-trip, audit assertions.
  Runs only when explicitly opted in.
- **TUI component tests** via `tea.TestProgram` (renders, confirm flows).

### D15 — v2 in-game admin client — feasibility note (assessed 2026-08-15)
Feasible, medium risk, deferred per D1. Assessment:
- **In-game surface:** Bedrock behavior packs run JavaScript (script API,
  `@minecraft/server`) *inside the server process*. Packs can register chat
  commands, show modal/action forms, read player/world state, and run
  in-game commands (`allowlist`, `op`, `kick` are in-game commands).
- **The host gap:** a pack has no filesystem/process access — backups,
  lifecycle, host telemetry, logs need a **bridge**: pack sends structured
  events (chat/UI) → a host-side service exposes the same domain core over a
  local socket → results return via chat/form.
- **What works with no bridge:** player/allowlist management, chat, forms,
  in-server state — a useful v2-lite.
- **Risks:** script-API churn (API version tracks the game version; 1.26
  already changed the pong layout once), forms UX, and the bridge adds an
  attack surface (bind to localhost, auth token).
- **V1 obligation (binding):** the domain core (`internal/bedrock` and the
  ops engine) must stay **free of TUI dependencies**, so v2 (in-game pack +
  bridge, or a web UI) can sit on the same core. The TUI is just client #1.

## Grill status

**All decisions resolved as of 2026-08-15** (D1–D15). The grill is complete;
the next phase is ticket decomposition (`/to-tickets`) and implementation.
