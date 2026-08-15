# Live Verification — BDS 1.26.43 on the operator's host

**Date:** 2026-08-15. **Method:** direct inspection of a running operator
server (read-only) + binary string analysis of the 1.26.43 binary. Operator
specifics (IPs, paths, names) are in `local/notes.md` (gitignored).

## Verified facts

### Process & unit (the operator's setup; representative of the common
single-host systemd deployment)
- Unit: `Type=Simple`, `ExecStart=/bin/sh -c "LD_LIBRARY_PATH=.
  ./bedrock_server"`, `Restart=on-failure`, **no `StandardInput=`** (default
  `tty` → /dev/null outside a TTY) → **the console is NOT reachable**; there
  is no `systemctl attach` verb in current systemd to change that.
- **No `StandardOutput=`** → logs go to the **journal**.
- World layout: `worlds/<level-name>/` containing `db/`, `level.dat`,
  `level.dat_old`, `levelname.txt`. On this host some server-created dirs
  are root-owned (unit ran without `User=`).
- `Dedicated_Server.txt` exists but is **0 bytes** — on a journal-wired setup
  the journal is the log, not this file.
- Log line format (observed in journal):
  `[2026-08-15 18:34:58:331 INFO] Running AutoCompaction...`
  i.e. `[YYYY-MM-DD HH:MM:SS:mmm LEVEL] message`; LevelDB AutoCompaction
  runs roughly every 6 minutes while idle.

### Pong advertisement (captured live, 2026-08-15)
`MCPE;<name>;2168;<version>;<gametype>;<MAX-PLAYERS>;<64-bit server GUID>;<MOTD1>;<MOTD2>;...`
- Field `[5]` is **max-players** (matched the live server.properties value),
  so in the 1.26 layout the classic "players" slot now carries max-players
  and a 64-bit server GUID occupies the old max-players slot. The live
  player count must be one of the trailing fields (candidate: the `0` after
  the two port fields, consistent with zero players online at capture time).
- **Parsers must be written against this layout, not the old 8-field one.**
  Third-party libs (mcstatus etc.) may still assume the old layout — verify
  before reusing.

### THE BAN QUESTION — RESOLVED (negative)
**BDS 1.26.43 has no native ban feature.** Evidence (all from the live
binary / live host):
1. `strings -n 4 bedrock_server | grep -xE` finds **every** expected console
   command (`allowlist changesetting deop difficulty gamemode kick list
   permission reloadconfig save setmaxplayers stop transfer`) but **no**
   `ban` / `pardon` / `ban-ip` / `pardon-ip`.
2. `strings | grep -iE "blacklist|deny|banned|forbid|reject"` → only C++
   symbols and bundled-library noise; nothing ban-related.
3. The bundled official `bedrock_server_how_to.html` has zero ban mentions.
4. No `banned-players.json` / `banned-ips.json` in the server directory.
5. Corroborates research/02: no documented ban file; no tool writes one.

**Consequence:** "ban users" on Bedrock means one of:
- **allowlist gating** (`allow-list=true` + allowlist.json — positive list;
  everyone else is rejected at join), or
- **host firewall IP block** (ufw/iptables on the server port), or
- **kick** (console `kick <xuid>`) — immediate, not persistent.
This became decision D3 (see docs/requirements.md).

### Console command strings confirmed present in the 1.26.43 binary
`allowlist changesetting deop difficulty gamemode kick list permission
reloadconfig save setmaxplayers stop transfer` (full reference in research/01).
