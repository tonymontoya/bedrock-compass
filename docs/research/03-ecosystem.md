# Bedrock Server Admin Ecosystem (Research)

**Purpose:** Position our TUI admin tool against what exists — especially the
CurseForge "Admin Suite – Server Tools" project that inspired the idea.

**Date / access:** 2026-08-15. Companions: `01-control-surfaces.md` (control
mechanisms + manager-tool survey), `02-data-and-backups.md` (files + backup
tools). Findings below reuse those sources where noted.

---

## TL;DR

1. **The CurseForge "Admin Suite – Server Tools" is NOT a host-level admin
   tool.** It is an *in-game* Bedrock script pack (author **heraclaus**,
   published 2025-10-06, updated 2026-08-05, **116.2k downloads**, 3.8★)
   providing player-facing server features: rank shop NPC, item shop with
   in-game cash, land-claim teleport, random-teleport (RTP) menu + cooldown,
   player freeze, member book, login system, paid "pro" tier sold via
   Discord. It runs *inside* the game world; it does not touch the server
   process, host metrics, logs, or backups.
   → Our tool is in a **different category** (host-level operations), and
   shares almost no implementation surface with it. The only overlap with the
   user's feature list is the *concept* of "server tools" as one admin surface.
2. **There is no terminal-native (TUI/CLI) admin tool for Bedrock.** Every
   existing tool is a web console, REST API, or thin CLI wrapper around the
   server console (see table). The TUI niche is open.
3. The de-facto deployment standard is **itzg/docker-minecraft-bedrock-server**
   (1.8k★) or systemd + the raw binary; both leave the console on a pty/FIFO
   that a TUI can attach to (see 01).
4. "Remote access restrictions" in Bedrock = `online-mode`, allowlist
   on/off + members, `max-players` (hot via `setmaxplayers`), and
   `packetlimitconfig.json` anti-DDoS limits — all in
   `server.properties`/JSON, all manageable from our TUI (see 02).
5. Player-connection visibility = RakNet pong (count, MOTD, version) +
   console log lines ("Player connected: <name>, xuid: <n>") — no in-game
   API needed (verified live against the operator's 1.26.43 server).

---

## 1. The CurseForge project (the design inspiration)

**What it is** (verified via the MCPEDL mirror of the CurseForge page, which
carries the full description/changelog/comments):

| Field | Value |
|---|---|
| Name | Admin Suite – Server Tools |
| Author | heraclaus |
| Type | Bedrock **script** (`.mcaddon`, 5 MB) — in-game, installed in the world |
| Category | Utility Scripts |
| Downloads | 116.2k (as of 2026-08-15) |
| Rating | 3.8 |
| Published / updated | 2025-10-06 / 2026-08-05 |

**Feature set** (from changelog + user comments, v1.1.0 current):
- Teleport-to-land-member (land claims), RTP (random teleport) menu + cooldown
- Player freeze
- Rank shop NPC (beta) + item shop with in-game currency ("cash"; players
  deposit ores/blocks and buy items)
- Member book (in-game member list item, resettable)
- Login system (in-game account gate; support routed to author's Discord)
- Multilingual (incl. Vietnamese; Chinese gated behind paid "pro" version)
- Paid tier ("pro version") — monetized outside the store

**Mechanism:** Bedrock Script API (TypeScript/JS in the world's behavior
pack). It has **no** host access: no shell, no logs, no backups, no process
control. It is a *gameplay feature pack for community/RP servers*, not an
operations tool. (CurseForge file pages: 7124135, 7086763, 8299689.)

**Could not verify:** the pack's actual source (`.mcaddon` download blocked
via MCPEDL CDN — 403 S3 policy); the feature list above is from the public
changelog/comments, which is sufficient for positioning.

## 2. Existing Bedrock admin tooling (positioning table)

Compiled from 01/02 surveys (sources there) + spot checks:

| Tool | Stars | Lang | Mechanism | Form factor | Status |
|---|---|---|---|---|---|
| itzg/docker-minecraft-bedrock-server | 1839 | Go (image) | container + `/data` volume; backup env | Docker | active (2026-08) |
| TapeWerm/MCscripts | 163 | bash+py | systemd FIFO console; journal log; `save`-protocol backup | systemd + CLI | active |
| DMedina559/bedrock-server-manager | 93 | Python | parent-pipe console; REST API | web/REST | active (most active manager) |
| clvrk/vellum | ~50 | C# | console wrapper; backups | web UI | semi-active |
| Minecraft-Server-Manager | ~49 | C# | console wrapper | web UI | semi-active |
| BedrockManagementService | ~41 | C# | console wrapper | web UI | semi-active |
| mrrooster/minecraftbedrockconsole | — | C++ | PrismLauncher-style console (GUI) | GUI | maintained by PrismLauncher author |
| Kaiede/Bedrockifier | ~58 | — | CLI + service (Bedrock & Java) | CLI | semi-active |
| binhex/arch-minecraftbedrockserver | — | bash | docker; `save`-protocol backup | Docker | active |
| EndstoneMC/endstone | ~716 | — | alternative server software (Paper-like API) | server | active |
| bdsx/bdsx, AllayMC/Allay | ~490/~348 | — | alternative server software | server | active |

**Gap analysis:** every operations tool is a **browser** or **headless API**.
None is a terminal-first tool. For an operator who is *already SSH'd into the
host* (our exact case), a TUI removes the browser tab, the REST client, and
the context-switch. That is the niche we own.

## 3. Bedrock Script API (why it is not our channel)

Official docs (learn.microsoft.com, "Scripting Bedrock Dedicated Server")
confirm script packs run **inside** the server process and can execute
in-game commands (`system.runCommand`) and hook game events — but there is no
documented shell/exec capability from server-side scripts. It is the right
surface for *in-world* features (like the Admin Suite pack) and the wrong
surface for *host operations* (backups, logs, metrics, bans). We will not
use it. (Caveat: flagged as not-exhaustively-verified — no official doc
promises shell access, which is the operative fact.)

## 4. "Remote access restrictions" (the user's phrase)

What it maps to on a vanilla Bedrock server (keys from the official
server.properties reference, see 02):

| Control | Key / mechanism | Hot? |
|---|---|---|
| Real-account gate (Xbox login required) | `online-mode` (true/false) | cold (restart) |
| Join allowlist | `allowlist` on/off + `allowlist.json` | hot (`allowlist reload`) |
| Player cap | `max-players` | hot (`setmaxplayers`) |
| Anti-DDoS / packet rate limits | `packetlimitconfig.json` | hot (`reloadpacketlimitconfig`) |
| Roles | `permissions.json` (operator/member/visitor) | hot (`permission reload`) |
| IPv6 exposure | `server-portv6` / V6 bind | cold |

No region/geo restrictions exist in vanilla Bedrock; that would be
firewall-level (host), outside the server's config.

## 5. Player-connection visibility

- **Pong advertisement** (verified live on the operator's server, 2026-08-15;
  full capture in `local/notes.md`):
  `MCPE;<name>;2168;1.26.43;0;<max-players>;<64-bit server GUID>;<MOTD1>;<MOTD2>;...`
  — note the old "max-players" slot is now a **64-bit server GUID** in 1.26
  (the captured value is in `local/notes.md`); the live player count is one
  of the trailing integer.
- **Console log lines**: `Player connected: <name>, xuid: <digits>` /
  `Player disconnected: <name>, xuid: <digits>` (parsed by
  bedrock-server-manager & mrrooster — see 02).
- **`list` command** for the authoritative online roster.
- No per-player ping/TPS is exposed by the server.

## Sources

- MCPEDL mirror of the CurseForge project (description, changelog, comments,
  download list): https://mcpedl.com/admin-suite-server-tools/ (accessed
  2026-08-15) — establishes the project's identity, features, stats.
- CurseForge project + file pages (URLs from search): https://www.curseforge.com/minecraft-bedrock/scripts/admin-suite-server-tools (files 7124135, 7086763, 8299689) — existence + file history.
- Microsoft Learn: Scripting Bedrock Dedicated Server — https://learn.microsoft.com/en-us/minecraft/creator/documents/bedrockserver/scripting (accessed 2026-08-15) — server-side script model.
- Live RakNet pong from the operator's server captured 2026-08-15 (see research/04 + this doc §5) — advertisement field layout in 1.26.
- Tool table: sources cited in `01-control-surfaces.md` and
  `02-data-and-backups.md` (GitHub repos, READMEs, source code).

## Could not verify

- The `.mcaddon` source of Admin Suite (CDN blocked) — feature list is from
  public changelog/comments only.
- Whether any *other* TUI Bedrock admin tool exists that search didn't surface
  (searches for "bedrock tui admin" return no such project; GitHub manager
  survey in 01 found none).
- Bedrock Script API server-side exec surface (asserted absent based on docs;
  no exhaustive audit of every script module performed).
