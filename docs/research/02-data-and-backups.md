# Bedrock Dedicated Server — Data & Backups (Research)

**Purpose:** What data a vanilla Bedrock Dedicated Server (BDS, Linux x86-64)
keeps on disk, how to back it up safely while it runs, how to restore it,
and which files may be touched live. Companion to
`01-control-surfaces.md` (same date/sources where overlapping).

**Date / access:** 2026-08-15. Version context: 1.26.x BDS
(official how_to doc + 1.26.40 command metadata + operator's live 1.26.43).

---

## TL;DR

- **One world at a time:** all game data lives in
  `worlds/<level-name>/` (name from `level-name` in `server.properties`).
  Everything else (allowlist, permissions, properties, packs) is small
  per-server config at the top level.
- **Online backup = `save hold` → `save query` → *you copy* → `save resume`.**
  It is a copy-coordination protocol, not a snapshot: the server pauses
  autosave, tells you the exact `path:size` file list (paths relative to
  `worlds/`), and the admin copies the files. Every real-world tool
  (binhex, MCscripts, mrrooster, vellum) implements exactly this.
- **`allowlist.json` and `permissions.json` are officially editable on disk**
  while the server runs, followed by `allowlist reload` / `permission
  reload`. Production tools (bedrock-server-manager) edit the JSON directly.
- **Bans are a black box.** The official how_to documents **no ban file and
  no ban console command** (Bedrock has no `ban`/`ban-ip`/`pardon`), and no
  tool examined writes a `banned-players.json`. Treat ban support as
  **undocumented — must verify on the live server** before building it.
- **No TPS/FPS counters exist** in Bedrock logs or the status pong. Player
  count comes from the RakNet pong; host metrics via `psutil`.

### Safe-to-touch file matrix

| File | Purpose | Safe while running? | How to modify |
|---|---|---|---|
| `server.properties` | Cold config (all keys) | **No** — read at startup / world creation | Edit while stopped; hot only via `setmaxplayers`, `changesetting`, `reloadconfig` |
| `allowlist.json` | Join gate (gamertag/xuid) | **Yes** — officially supported | Edit JSON + `allowlist reload`; or `allowlist add/remove <tag>` |
| `permissions.json` | Roles (operator/member/visitor) | **Yes** — officially supported | Edit JSON + `permission reload`; or `op`/`deop` (server writes file) |
| `packetlimitconfig.json` | Anti-DDoS rate limits | **Yes** — officially supported | Edit JSON + `reloadpacketlimitconfig` |
| `banned-players.json` / `banned-ips.json` | (alleged) bans | **Unknown — undocumented** | **Must verify.** No doc, no console command, no tool writes these |
| `worlds/<name>/` | World (db/, level.dat, …) | **No** — active write target | Never copy while running without `save hold`/`resume`; restore only stopped |
| `behavior_packs/`, `resource_packs/` | Shared packs | Mostly (new packs); risky for in-use packs | Edit while stopped; no BDS pack-reload command found |
| `server_output.txt` | Console log | Read-only (tail) | — |

---

## World layout

Server directory (top level):

```
server.properties        cold config (INI, # comments)
allowlist.json           join gate (created on first use)
permissions.json         roles (created by op/ on first use)
banned-players.json      NOT documented (see below)
server_output.txt        console log
worlds/
  <level-name>/          ONE active world (level-name in server.properties)
    level.dat            world meta (BDS keeps level.dat_old rollback copy)
    levelname.txt
    db/                  LevelDB game data (entities, blocks, player data)
    (poi / level data files)
behavior_packs/          shared across all worlds (top-level)
resource_packs/          shared across all worlds (top-level)
uuid/
```

- `worlds/` subfolders are named by `level-name`; multiple worlds can exist,
  **one active at a time**. World folders can be copied between BDS and
  single-player Bedrock (client worlds live under
  `users/<id>/games/com.mojang/minecraftWorlds`, identified via
  `levelname.txt`).
- Packs **inside** `worlds/<name>/` apply to that world only; top-level
  packs apply to all worlds.

## Backup protocol (save hold / query / resume)

Exact console strings (version variance noted; verified in tooling that
greps for these):

1. `save hold`
   - stdout: **`Saving...`** (flushes pending writes, pauses autosave).
   - if a previous save is still running: **`A previous save has not been
     completed.`** — retry later.
2. `save query`
   - stdout: **`Data saved. Files are now ready to be copied.`** followed by
     a **comma-separated `path:size` list**, paths relative to `worlds/`
     (e.g. `<world>/level.dat`, `<world>/db/...`). **The admin copies these
     files** — the server does not zip anything.
   - mrrooster parses the list with `split(", ")`; MCscripts copies the
     listed files; binhex just copies the whole `worlds/` dir; vellum zips
     the world dir.
3. `save resume`
   - stdout: **`Changes to the level are resumed.`** — some versions say
     `world` (vellum waits for
     `^(Changes to the (level|world) are resumed.)`).
   - **Always resume**, even on copy failure, or the world stays in hold
     state.

**Golden rule:** `save hold` → copy → `save resume` in one short window;
hold state is not a snapshot — data stops changing, it is not frozen into an
archive by the server.

## Backup tools survey

| Tool | Mechanism (one line) |
|---|---|
| **binhex/arch-minecraftbedrockserver** (Docker) | `backup.sh` copies the whole `worlds/` directory (online, via save protocol) |
| **TapeWerm/MCscripts** (163★, systemd) | `mcbe_backup.py` runs the console, copies the `save query` file list into a timestamped archive; `mcbe_restore.sh` for restore |
| **itzg/docker-minecraft-bedrock-server** (1839★) | de-facto standard container: `/data` volume, EULA, UDP 19132/19133, `ENABLE_BDS_V6BIND_FIX`; backups are a user-side concern (logs to stdout) |
| **mrrooster/minecraftbedrockconsole** (C++) | zips world files + `server.properties` + permissions; reads the `save query` list via `split(", ")` |
| **clvrk/vellum** (C#) | `save hold` → `save query` (500 ms wait) → `save resume`; `ZipFile` archive; `StopBeforeBackup` option for full-copy |
| **Bedrockifier** | Bedrock server fork/extension project; **no backup-protocol source verified this session** (see Could-not-verify) |

## Restore

1. **Stop the server.** MCscripts prompts `Enter Y if you stopped the server
   to restore`; vellum offers `StopBeforeBackup`. A running server will
   rewrite `db/` and clobber the restored data on next save.
2. **Replace `worlds/<level-name>/` contents**: `db/`, `level.dat`,
   `levelname.txt`, poi/level files. (MCscripts: `rm -rf worlds/<name>` then
   `unzip backup -d worlds/` — i.e. the archive contains the `worlds/`
   prefix; vellum extracts straight into the world dir.)
3. **Preserve** top-level config: `server.properties`, `allowlist.json`,
   `permissions.json`, packs. Restore is world-only.
4. **Gotchas:**
   - BDS keeps **`level.dat_old`** as its own rollback copy — delete or
     expect the server to offer it; a stale one can mask a bad restore.
   - **World/protocol version mismatch**: a world saved by a different BDS
     version may not load, or may be migrated on first boot (one-way).
   - If the backup was taken during a `save hold` that was never resumed,
     the world may be mid-write — prefer backups from a clean
     hold/query/copy/resume cycle.
   - Take a **precautionary backup of the current world first** (vellum does
     this automatically) — restore is destructive to `worlds/<name>/`.
   - Fix ownership (MCscripts `chown -R mc:mc worlds/<name>`).

## JSON schemas

### `allowlist.json` (official, example verbatim from MS docs)

JSON array of entries; direct edit is **officially supported** — "You can
also edit the `allowlist.json` file directly… you'll need to run the command
`/allowlist reload`".

| Key | Type | Notes |
|---|---|---|
| `name` | string | gamertag |
| `xuid` | string | optional; **filled in automatically** when the named player connects (so `allowlist add <tag>` works pre-join) |
| `ignoresPlayerLimit` | boolean | player does not count toward `max_players` (soft cap still 30 or max+1) |

```json
[
    { "ignoresPlayerLimit": false, "name": "MyPlayer" },
    { "ignoresPlayerLimit": false, "name": "AnotherPlayer", "xuid": "274817248" }
]
```

- File was **renamed from `whitelist.json`** in an older version (official
  how_to note; stale tooling still says `whitelist.json`).
- `allowlist list` → JSON `{"command":"whitelist","result":[{name,
  ignoresPlayerLimit}]}`.

### `permissions.json` (official)

JSON array of `{permission, xuid}`; `permission` ∈ `operator | member |
visitor`. Official how_to: after editing, run `permission reload`;
`op`/`deop` **write this file** for XBL-authenticated players (creates it if
missing; non-XBL players get a session-only change).

```json
[
    { "permission": "operator", "xuid": "274817248" },
    { "permission": "member",   "xuid": "123456789" },
    { "permission": "visitor",  "xuid": "987654321" }
]
```

- `permission list` → JSON `{"command":"permissions","result":[{xuid,
  permission}]}`.
- bedrock-server-manager additionally writes a `name` field into entries
  (harmless extra key); it edits the file directly with
  `json.dump(..., indent=4, sort_keys=True)` — production proof that live
  file editing is the accepted pattern.
- New-player default comes from `default-player-permission-level`
  (default `member`).

### `banned-players.json` / `banned-ips.json` — **UNDOCUMENTED**

- The official how_to ("How to use the dedicated server") and the MS Learn
  BDS pages document **no ban file and no ban command**; the 1.26.40
  `mojang-commands.json` has **no `ban`/`pardon`/`ban-ip`/`pardon-ip`**.
- Java has `banned-players.json` (`{name,uuid,created,expires,source,reason}`)
  and `banned-ips.json` — **do not assume the same shape works for Bedrock**;
  Bedrock bans are XUID-based, but no primary source was found describing the
  file(s).
- bedrock-server-manager's "ban" API is **bookkeeping only** (its own SQLite
  `ServerBan` table: server_id, player_name, xuid, reason, banned_at) — it
  never writes a ban JSON to the server dir.
- **Verdict:** ban support must be verified empirically on the live 1.26.43
  server (create a ban via client/console if any path exists, inspect the
  dir) before implementing it.

## Log format

- **File:** `server_output.txt` in the server dir (bedrock-server-manager's
  documented expected path `<server_dir>/server_output.txt`; in containers
  the same output goes to stdout and is captured by Docker). No rotation
  behavior found in any source — treat as a single growing file.
- **Line formats seen in production parsers** (mrrooster,
  bedrock-server-manager; operator logs):
  - `Player connected: <name>, xuid: <digits>`
  - `Player disconnected: <name>, xuid: <digits>`
  - `Opped: <name>` / `De-opped: <name>`
  - `Saving...` / `Data saved. Files are now ready to be copied.` /
    `Changes to the level are resumed.` (backup protocol)
  - kick lines (MCscripts posts connect/disconnect/**kick** to webhooks)
- **No performance counters** (no TPS/FPS lines) appear in any of these
  sources.

## Telemetry / status

- **Player count, MOTD, version, gamemode:** RakNet `UnconnectedPing` →
  `UnconnectedPong` on UDP 19132 (see `01-control-surfaces.md` Q7). The
  semicolon-separated server-ID string carries `MCPE;MOTD;protocol;version;
  players;<GUID slot>;level-name;gamemode;...;portv4;portv6`. In 1.26 the
  slot conventionally read as *max players* carries a **64-bit server GUID**
  (operator-verified live); the current player count is still in the
  `players` field. Use `mcstatus` (`BedrockServer.status()`) or a raw 0x01
  ping with the correct Bedrock magic.
- **TPS/FPS:** **not exposed** — no log lines, no pong field, no primary
  documentation. If the TUI wants a tick-health signal, derive it from
  client-side latency or skip it.
- **Host metrics:** `psutil` on the BDS host against the `bedrock_server`
  PID (CPU%, RSS, disk I/O on `worlds/`).

## Sources (accessed 2026-08-15)

- Microsoft Learn — *Getting Started with Bedrock Dedicated Server*
  (worlds/ layout, allowlist.json schema+example, direct-edit + `/allowlist
  reload`, player soft cap):
  <https://learn.microsoft.com/en-us/minecraft/creator/documents/bedrockserver/getting-started>
- Microsoft Docs source (same content, incl. `server-properties.md`,
  `scripting.md`): <https://github.com/MicrosoftDocs/minecraft-creator/tree/main/creator/Documents/BedrockServer>
- Official how_to doc *"How to use the dedicated server"* (shipped in BDS
  releases; fetched from mirror) — properties table (incl.
  `packetlimitconfig.json` + `reloadpacketlimitconfig`), whitelist→allowlist
  rename, permissions.json + `permission reload`, op/deop file writes,
  `save hold|query|resume`, **absence of any ban command/file**:
  <https://github.com/IronWalnut/minecraft_server/blob/main/bedrock_server_how_to.html>
- `DMedina559/bedrock-server-manager` — `allowlist_mixin.py` +
  `permissions_mixin.py` (direct JSON editing, live), `api/ban.py` (own-DB
  bookkeeping, no ban file), `log_streamer.py` + `base_server_mixin.py`
  (`server_output.txt`): <https://github.com/DMedina559/bedrock-server-manager>
- `TapeWerm/MCscripts` — `mcbe_backup.py` (copy the `save query` list),
  `mcbe_restore.sh` (stop first, `rm -rf worlds/<name>`, unzip, chown),
  `mcbe_log.sh` (connect/disconnect/kick lines):
  <https://github.com/TapeWerm/MCscripts>
- `clvrk/vellum` — `BackupManager.cs` (hold/query/resume sequence,
  "level|world are resumed" wait regex, zip restore + precautionary
  backup): <https://github.com/clvrk/vellum>
- `mrrooster/minecraftbedrockconsole` — `backup/backupmanager.cpp` (zips
  world + `server.properties` + permissions; parses list with
  `split(", ")`): <https://github.com/mrrooster/minecraftbedrockconsole>
- `binhex/arch-minecraftbedrockserver` — `backup.sh` (copies whole `worlds/`
  dir): <https://github.com/binhex/arch-minecraftbedrockserver>
- `itzg/docker-minecraft-bedrock-server` (1839★) — `/data` volume, EULA,
  UDP 19132/19133: <https://github.com/itzg/docker-minecraft-bedrock-server>
- Local — `docs/research/01-control-surfaces.md` (pong wire format, RakNet
  magic, command set, `server.properties` cold/hot); operator live 1.26.43
  (`~/source/bedrock-connect-HANDOVER.md`, pong GUID slot verified live).

## Could not verify

- **Ban file schemas + enforcement** (`banned-players.json`,
  `banned-ips.json`): no official doc, no console command, no tool writes
  them. **Must verify on the live server** before building a ban feature.
- **Whether BDS caches `allowlist.json`/`permissions.json` in memory and
  rewrites them on shutdown** (which could clobber external edits made
  without a `reload`). Official pattern (edit + reload) is the safe one.
- **Log file name / rotation on 1.26** — `server_output.txt` is the
  documented expectation of one production tool; no primary source on
  rotation, and containers redirect it to stdout.
- **Exact `save query` file list contents on 1.26** (whether poi files are
  listed) — not run against the binary this session; tooling examples show
  `level.dat` + `db/...`.
- **Bedrockifier backup mechanism** — no primary source examined.
- **Whether `stop` always performs a final full save** — strongly implied
  by the graceful-shutdown design, not stated in a source found.
