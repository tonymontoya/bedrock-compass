# Bedrock Dedicated Server — Control Surfaces (Research)

**Purpose:** Map every way to programmatically drive a vanilla Minecraft
Bedrock Dedicated Server (`bedrock_server`, Linux x86-64 binary) so we can
design a same-host TUI admin tool run over SSH.

**Date / access:** 2026-08-15. All sources accessed on this date; URLs in
[Sources](#sources). Version context: findings verified against the
**1.26.40** server build (authoritative command metadata from Mojang's
`bedrock-samples` v1.26.40.05) and cross-checked against the operator's
running **1.26.43** BDS (see `~/source/bedrock-connect-HANDOVER.md`).

---

## TL;DR / Recommendation

- **There is no RCON and no official remote console for Bedrock.** The only
  native control surface is the **server's own stdin/stdout pair**: type a
  command line + Enter into the console (stdin) and read log/status lines
  from stdout. Everything that "manages" a Bedrock server in the wild is a
  **console wrapper** around that single channel.
- **Recommended control mechanism for the TUI: own the stdin/stdout pair from
  the moment the process starts.** Two proven, low-risk patterns:
  1. **Parent-process pipe** (what `DMedina559/bedrock-server-manager`
     does): launch `bedrock_server` with `stdin=PIPE, stdout=PIPE` and
     write `"command\n"` to the pipe, read lines from stdout.
  2. **systemd FIFO socket** (what `TapeWerm/MCscripts` does): the unit uses
     `StandardInput=socket` + `ListenFIFO`; to send a command you
     `echo "stop" > /run/<unit>`. This lets the TUI drive an *already-running*
     server without being its parent.
  - Avoid **tmux/screen** for the core control path: it works (TheRemote and
    Docker paths use it) but you parse by scraping `capture-pane`/`screen -X
    hardcopy`, which is racy and buffer-bounded. Reserve it for a
    human-attach escape hatch, not the automation path.
- **For status (player count, MOTD, version, gamemode), do NOT use the
  console.** Use the **RakNet `UnconnectedPing` (0x01) → `UnconnectedPong`
  (0x1C)** on **UDP 19132** (the game port). This is what the in-game server
  list and every status library (`mcstatus`, `mcutils`, `MinecraftStatus`)
  use. It is **not** the Java 0xFE Source query protocol.
- **Command set:** Bedrock's admin commands are a *subset with renames* of
  Java. Notably **absent** in Bedrock: `ban`, `pardon`, `ban-ip`,
  `pardon-ip`, `broadcast`, `save-all`. Bedrock **adds**: `setmaxplayers`
  (runtime max players), `changesetting`, `reloadconfig`, `save
  hold|query|resume` (online backup), `transfer`. The console itself always
  runs at permission level 4 (owner), so it can execute everything.
- **Lifecycle:** `bedrock_server` is a **foreground** process (`Type=simple`);
  it needs `LD_LIBRARY_PATH=.` and `WorkingDirectory` = the server dir.
  `stop` = graceful save + exit (all tooling sends `stop` then waits for the
  PID). `server.properties` is **cold** (read at startup / world creation) —
  most keys need a **restart**; only a few things are hot via console
  (`setmaxplayers`, `allowlist reload`, `permission reload`, `changesetting`,
  `reloadconfig`).

---

## Q1. Official console interface

**What it is.** A plain line-oriented reader on the server process's stdin.
You type a command and press **Enter (newline)**; the server prints its own
log/status lines to stdout. There is no prompt, no tab-completion, no
history, and no interactive shell layer (no `readline`).

- Official getting-started (Microsoft Learn): *"Once the server is running,
  you can enter commands on the console. Type `help` to get a list of
  commands."* — i.e. raw command entry, no prompt mentioned.
- Official how-to (`bedrock_server_how_to.html`, community-translated copy of
  Mojang's page): *"You can issue commands to the server by typing into the
  console."* — again, raw typing.

**Echo / output semantics (strongly supported, not fully binary-verified).**
Every implementation examined treats the channel as: *write `command\n` to
stdin → read server log lines from stdout.* None waits for a prompt, none
parses echoed input, none implements completion:

- `DMedina559/bedrock-server-manager`: writes `command + "\n"` to the stdin
  pipe; reads the stdout log file.
- `TapeWerm/MCscripts`: writes `command\n` to a FIFO (the service's stdin);
  reads output from the **systemd journal** via `journalctl --after-cursor`.
- `TheRemote/MinecraftBedrockServer`: `screen -Rd <name> -X stuff
  "stop$(printf '\r')"` — inject command + carriage return, then poll.

**What echoes:** the server emits its own lines (e.g. `save query` returns a
file list; it can print `"A previous save has not been completed."`). On an
interactive TTY the *terminal* echoes typed characters (that's tty line
discipline, not the server); on a pipe/socket there is no input echo — only
server output appears. **Could not fully verify** the exact per-command
stdout confirmation strings without running the binary (download timed out —
see [Could not verify](#could-not-verify)).

**Where stdout goes:**
- **systemd:** `StandardOutput=journal` → read with
  `journalctl -u <unit> -f` / `--after-cursor` (MCscripts pattern).
- **nohup / manual:** redirected to a file or `nohup.out`.

**Attaching to an *already-running* server.** There is **no official attach
mechanism**, and on Linux you cannot attach to a running process's existing
stdin. So the design must control stdin **from launch time**:
- as the parent process (direct pipe), or
- via a persistent FIFO/socket created at start (MCscripts
  `StandardInput=socket` + `ListenFIFO`), or
- via a pty/screen session the server was started in (TheRemote).
If the server was started with stdin = `/dev/null`, **there is no way to send
it console commands later** — only signals. This is the single most
constraining fact for the TUI.

---

## Q2. RCON support / community RCON projects

**Official Bedrock: none.**
- minecraft.wiki "Bedrock dedicated server" lists as a limitation:
  *"Doesn't support remote console (RCON), which allows sending commands
  remotely from external sources."*
- The full official `server.properties` schema (Microsoft Learn
  "Bedrock server properties") contains **no `rcon` key** (verified by
  searching the entire properties page). (Java has `enable-rcon`/`rcon.port`;
  Bedrock has neither.)

**Community projects that add an RCON-like network surface** (all are
**console wrappers** — they become the server's parent and bridge a network
protocol to the child's stdin/stdout):

| Project | Lang | ★ / last push | Mechanism | Notes |
|---|---|---|---|---|
| `seeruk/bedrockd` | Go | 2★ / 2019 | parent proc + RCON port | lifecycle (start/stop) + RCON bridge |
| `saulmaldonado/mc-bedrock-runner` | Go | 0★ / 2021 | parent proc + RCON server | uses `Tnze/go-mc`; SIGTERM handling |
| `Keito-Klein/Bedrock-Socket` | JS | 0★ / 2026-06 | websocket "like a Rcon" | new, tiny |
| `xtamtamx/pickaxe-rcon` | HTML/JS | 1★ / 2026-05 | web admin + realtime console | new, tiny |

**Mechanism, not injection.** None of these use `LD_PRELOAD` or binary
patching — they spawn `bedrock_server` as a subprocess and forward
stdin/stdout to a TCP/websocket endpoint. (I did not find any LD_PRELOAD or
binary-patch RCON for Bedrock in the top search results.)

**Risk to a production server.**
- Becomes the **parent process**: if the wrapper dies, the server can lose
  its stdin/stdout (or be orphaned). You now have an extra process in the
  trust chain that you must keep alive and secure.
- Adds a **network listener** (RCON port) — new attack surface; Java-style
  RCON is unencrypted and only password-gated.
- **Maturity is low** (0–2★, several years stale or brand-new). No
  production-grade, well-maintained Bedrock RCON exists that I found.

**Bottom line:** a Bedrock "RCON" is just a thin, network-exposed console
wrapper. For a *same-host* TUI you don't need the network layer at all —
drive the stdin/stdout pair directly (Q3/Q4 patterns).

---

## Q3. Driving via tmux/screen/pty (de-facto pattern)

This is a real, in-use pattern, but it is the **least clean** of the options
and the top-maturity projects have moved away from it toward a FIFO/socket
or a direct pipe.

**Real projects:**

- **`TheRemote/MinecraftBedrockServer`** — runs the server inside
  **screen**; systemd unit `Type=forking` with `start.sh`/`stop.sh`.
  Sending a command to the running server:
  ```bash
  screen -Rd servername -X stuff "stop$(printf '\r')"   # inject + Enter
  screen -Rd servername -X stuff "say restarted$(printf '\r')"
  ```
- **`TapeWerm/MCscripts`** (163★, active) — *systemd-first*; uses a
  **FIFO socket** as the server's stdin instead of tmux (see below). Its
  **Docker** path is the closest thing to a pty bridge:
  ```bash
  echo "$*" | socat - EXEC:"docker container attach -- $service",pty
  ```
- **`DMedina559/bedrock-server-manager`** (93★, active) — **no pty/tmux**;
  plain `subprocess.Popen(stdin=PIPE, stdout=<log file>)` and
  `f.write("command\n")`.

**The cleaner "pty-like" mechanism (recommended over tmux):** MCscripts
`mcbe@.service` + `mcbe@.socket`:

```ini
# mcbe@.socket
[Socket]
ListenFIFO=%t/%N            # /run/mcbe@<instance>  (a named pipe = the server's stdin)
SocketMode=600
SocketUser=mc
[Install]
WantedBy=sockets.target
```
```ini
# mcbe@.service  (relevant lines)
[Service]
Environment=LD_LIBRARY_PATH=.
ExecStart=/opt/MC/bedrock/%i/bedrock_server
StandardInput=socket        # the FIFO above IS stdin
StandardOutput=journal
StandardError=journal
Type=simple
User=mc
WorkingDirectory=/opt/MC/bedrock/%i
ExecReload=/bin/bash -c 'echo reload > %t/%N'
ExecStop=/opt/MCscripts/bin/mc_stop.py %N
```

Sending a command is then just a write to the FIFO:
```python
pathlib.Path("/run", "mcbe@main").write_text("stop\n")   # MCscripts server_do()
```
Reading the result: `journalctl "_SYSTEMD_UNIT=mcbe@main.service" --after-cursor <c> -o cat`.

**Failure modes of the tmux/screen/pty approach (design these around):**
1. **No prompt** → you cannot "wait for the prompt" to detect command
   completion; you must match on a *server status line* or a fixed delay.
   MCscripts uses a `sleep 1` + **journal cursor** (send a marker, capture the
   cursor, then read everything after it) — the most robust variant.
2. **Multi-line output** — `help`, `list`, `allowlist list`, `save query`
   (file list) all emit multiple lines; a single `readline()` loses data.
3. **Race between send and read** — you must snapshot a *before* cursor/tail
   position and only read *after* it, or you'll swallow unrelated log lines.
4. **Finite capture buffer** — `tmux capture-pane` is bounded by
   `history-limit`; long logs scroll off. `screen -X hardcopy` similar.
5. **ANSI / color** — Bedrock stdout is mostly plain text, but some versions
   emit color/escape sequences; strip before parsing.
6. **Stuffing timing** — `screen -X stuff`/`tmux send-keys` inject into a
   line buffer; if the server is mid-write you can interleave. (TheRemote
   mitigates with explicit `sleep` between stuffs.)
7. **Ownership** — the tmux/screen server is the *real* stdin owner; the TUI
   is just a client of that session. If the session dies, the server's stdin
   is gone.

**Verdict:** tmux/screen is fine for a *human* attach escape hatch, but for
automation prefer (a) parent-process pipe or (b) a FIFO/socket stdin, both of
which make the "send command / read after-cursor" contract explicit.

---

## Q4. REST / HTTP admin projects

All of these are **console wrappers** (process + stdin/stdout) with a web or
service front-end. None use RCON (it doesn't exist). Maturity/activity as of
access date:

| Project | Lang / stack | ★ / last push | Mechanism | Status |
|---|---|---|---|---|
| **`DMedina559/bedrock-server-manager`** | Python (FastAPI + CLI) | **93★ / 2026-08** | `subprocess.Popen(stdin=PIPE)`; `mcstatus` ping for status; graceful `stop`→wait→`SIGKILL` | **most active & mature** |
| `clvrk/vellum` | C# (.NET) | 50★ / 2023 | "uses the server console and its commands"; backups | active-ish, older |
| `Benjerman/Minecraft-Server-Manager` | C# | 49★ / 2025 | Bedrock server manager | maintained |
| `crowbarmaster/BedrockManagementService` | C# (Win service + GUI) | 41★ / active | multi-host, Windows service | active |
| `imueller64/bedrock-server-manager-dash` | HTML/Docker | 18★ / active | "Docker-backed web console… real-time [console]" | active |
| `Ratcoder/WebMC` | Go | 13★ | web-based Bedrock manager | small |
| `SimplyFriday/bedrock-portal` | C# | 6★ | "web portal that wraps the Mojang Bedrock Dedicated Server" | small |

**Representative control code — `DMedina559/bedrock-server-manager`**
(the cleanest reference implementation of the recommended pattern):

```python
# start
self.process = subprocess.Popen(
    [self.binary, ...],
    stdin=subprocess.PIPE, stdout=stdout, stderr=stderr,
    cwd=self.directory,
)
# send a command
def send_command(self, command: str) -> None:
    if not self.is_running(): raise ...
    self.process.stdin.write((command + "\n").encode())
    self.process.stdin.flush()
# graceful stop: write "stop", wait, then force
self.process.stdin.write(b"stop\n"); self.process.stdin.flush()
# (wait up to ~60s; SIGKILL fallback)
```

**Game panels.** Pterodactyl is the common "game panel" with a **pty-based**
web console (the model: attach to a pty, relay bytes). I **could not
verify** a first-party Bedrock process driver in this session (docs page
404'd; no driver found in search). Treat panel support for Bedrock as
**unconfirmed** here.

**Takeaway for the TUI:** the proven web/service design is exactly
*process manager + stdin pipe + stdout tail + `mcstatus` for status*. A TUI
can reuse that contract minus the HTTP layer.

---

## Q5. Complete admin command set (Bedrock 1.26.x)

**Authoritative source:** `mojang-commands.json` shipped in Mojang's
`bedrock-samples` **v1.26.40.05** (83 commands). Cross-checked against the
official how-to and Microsoft Learn. Permission levels in the metadata are
numeric: `0=visitor, 1=member, 2=operator, 3=host, 4=owner`. **The console
always runs at level 4 (owner)**, so every command below is available to the
TUI.

### The admin commands the TUI cares about

| Command | Signature (Bedrock) | Perm | Notes / Java difference |
|---|---|---|---|
| **kick** | `/kick <player> [reason]` | 1 | ✓ exists. (Bedrock quirk: member-level may kick; Java needs op.) |
| **ban** | — | — | ✗ **absent in Bedrock.** No `ban`, no `ban-ip`. Use `/allowlist` (IP) or client-side. |
| **pardon / unban** | — | — | ✗ **absent in Bedrock.** No `pardon`, no `pardon-ip`. |
| **op** | `/op <player>` | 2 | ✓ |
| **deop** | `/deop <player>` | 2 | ✓ (Java `/deop <player>` too) |
| **whitelist** | `/whitelist add\|remove\|on\|off\|list\|reload [name]` | 4 | Alias of `/allowlist`. **No `clear`** (Java has `whitelist clear`). Actions: `add remove off list reload on`. |
| **allowlist** | `/allowlist <action> [name]` | 4 | Canonical Bedrock name. Persists to `allowlist.json`. |
| **list** | `/list` | 0 | ✓ |
| **say** | `/say <message>` | 1 | ✓. Bedrock has **no `broadcast`** (use `say`). |
| **save-all** | — | — | ✗ **absent in Bedrock.** Bedrock instead has `/save hold\|query\|resume` (online backup) + continuous persistence. |
| **stop** | `/stop` | 4 | ✓ graceful shutdown (see Q6). |
| **tp / teleport** | `/tp <target> [dest]` | 1 (cheats) | ✓ `/tp` is an alias of `/teleport`. |
| **gamemode** | `/gamemode <mode> [player]` | 1 (cheats) | modes: `default creative spectator survival adventure` (+ `d c s a`) |
| **difficulty** | `/difficulty <level>` | 1 (cheats) | ✓ |
| **max players (runtime)** | `/setmaxplayers <int>` | 3 | ✓ **Bedrock-only** (no Java runtime equivalent; Java needs restart/RCON). |
| **status/query** | — | — | No console command; use the RakNet ping protocol (Q7). |

### Bedrock-only additions (no Java equivalent)
- `/changesetting allow-cheats|difficulty <value>` (perm 4) — change a
  `server.properties`-style setting **at runtime**.
- `/reloadconfig` (perm 4) — reload config files.
- `/save hold|query|resume` (perm 4) — **online backup protocol** (see Q6).
- `/permission list|reload` (perm 4) — manage `permissions.json`.
- `/transfer <pfid> <server> [port]` (perm 4) — transfer a player.
- `/editor-allowlist`, `/sendshowstoreoffer` (perm 4) — store/editor ops.

### Java-only (missing from Bedrock) — do NOT send these
`ban`, `ban-ip`, `pardon`, `pardon-ip`, `broadcast`, `save-all`,
`whitelist clear`, `defaultgamemode`-style runtime changes (Bedrock uses
`changesetting`/`gamemode`), `rcon`-related (n/a).

### Bedrock vs Java naming differences (summary)
- `allowlist` (canonical) ↔ Java `whitelist`; Bedrock keeps `/whitelist` as
  an alias.
- `setmaxplayers` (runtime) ↔ Java: none at runtime.
- `changesetting`, `reloadconfig`, `transfer`, `save hold/query/resume` ↔
  Java: none (Java uses `save-all`, `reload`, RCON, `broadcast`).
- `ban`/`pardon`/`broadcast`/`save-all` ↔ Java: present.

### `server.properties` runtime-changeable keys (hot) vs restart (cold)
- **Hot, via console:** difficulty + allow-cheats (`changesetting`), allowlist
  (`allowlist reload`), permissions (`permission reload`), config
  (`reloadconfig`), max players (`setmaxplayers`).
- **Cold (restart required):** `server-port`, `level-name`, `online-mode`,
  `max-players` (base), `view-distance`, `tick-distance`, etc.
- **World-creation-only:** `level-seed`, `level-type`, initial `gamemode`.

> The official how-to states some properties *"are read only when creating a
> new world, while others are read on every startup"* — i.e. **a restart is
> required** for property changes (they are not live-reloaded except via the
> explicit console commands above).

---

## Q6. Server process lifecycle

### How it runs
- `bedrock_server` is a **foreground** process (it stays attached to the
  terminal; it does **not** daemonize). Run as:
  ```bash
  cd /path/to/server
  LD_LIBRARY_PATH=. ./bedrock_server        # needs LD_LIBRARY_PATH=. + server dir as CWD
  ```
- **systemd** (recommended; MCscripts `mcbe@.service` is a clean, hardened
  template — see Q3). Key lines:
  ```ini
  [Unit]
  After=network.target
  BindsTo=%N.socket
  [Service]
  Environment=LD_LIBRARY_PATH=.
  ExecStart=/opt/MC/bedrock/%i/bedrock_server
  StandardInput=socket
  StandardOutput=journal
  StandardError=journal
  Type=simple            # foreground, does NOT fork
  User=mc
  WorkingDirectory=/opt/MC/bedrock/%i
  Restart=on-failure
  ```
  (MCscripts also sets `NoNewPrivileges=`, `ProtectSystem=full`,
  `ProtectHome=true`, `PrivateTmp=true`, etc.)

### start / stop / restart
- **start:** `systemctl start mcbe@main` (or run the binary). `ExecStartPre`
  can `chmod +x bedrock_server`.
- **stop (graceful):** send `stop` to the server's stdin, then **wait for the
  PID to exit**. Every mature tool does exactly this:
  - MCscripts `mc_stop.py`: `say "Server stopping in N seconds"` (countdown),
    then `write "stop\n"` to the FIFO, then `tail -f --pid $MAINPID /dev/null`
    to block until the process dies.
  - `bedrock-server-manager`: `stdin.write(b"stop\n")`, wait ~60 s, `SIGKILL`
    fallback.
  - TheRemote: `screen -X stuff "stop\r"`, poll up to 30 s.
  - If you only have the PID and no stdin (recovered/orphaned process):
    `SIGTERM`, then `SIGKILL` fallback (DMedina's `kill()` path).
- **restart:** stop then start. (TheRemote `restart.sh` = stuff `stop`, wait,
  re-run `start.sh`.)

### What `stop` does to world integrity
- `stop` = **graceful shutdown** (official how-to: "gracefully shuts down the
  server"). The server finalizes its save and exits. All tooling relies on
  this (they send `stop`, wait for clean exit, and only `SIGKILL` as a last
  resort) — strong functional evidence that `stop` is the integrity-safe
  shutdown.
- Bedrock persists world state under `worlds/<level-name>/` and supports
  **online backups** via `/save hold` → `/save query` (poll until done;
  returns a file list) → copy files → `/save resume` (MCscripts
  `mcbe_backup.sh` implements exactly this). **Could not verify** the exact
  autosave interval / whether `stop` always does a final full save from a
  primary source — mark as **strongly implied, not explicitly documented**.

### `server.properties` hot vs cold
- See Q5. **Cold by default** (restart required for most keys). Hot only via
  the explicit console commands (`setmaxplayers`, `changesetting`,
  `allowlist reload`, `permission reload`, `reloadconfig`).
- **`online-mode` is cold and critical:** with `online-mode=true`, if the
  server cannot reach Microsoft services it **shuts down ~60 s after start**
  ("Could not connect to Minecraft services"), and a `Restart=on-failure`
  unit then crash-loops. (Operator-verified, `bedrock-connect/NOTES.md`.)
  `online-mode=false` avoids this but also skips initializing the RakNet ping
  responder in some versions — so a *downed status ping* may be a DNS /
  `online-mode` issue, not a client issue.

---

## Q7. Status discovery (query / ping)

**Bedrock does NOT use the Java 0xFE Source query protocol.** It uses the
**RakNet `UnconnectedPing` → `UnconnectedPong`** exchange on **UDP 19132**
(the game port — there is no separate query port). This is exactly what the
in-game server list uses, and the vanilla server **does** respond to it
(unless `online-mode=false` + no MS connectivity, see Q6).

**Wire protocol (UDP 19132):**
1. Client sends **`UnconnectedPing`** (RakNet packet id **`0x01`**) to
   `host:19132`.
2. Server replies with **`UnconnectedPong`** (packet id **`0x1C`**) whose
   payload is a **semicolon-delimited "Server ID" string**.

**Server ID string format** (12+ fields, trailing empty field):
```
MCPE;<MOTD>;<Protocol>;<Version>;<Players>;<MaxPlayers>;<ServerGUID>;<MOTD2/level-name>;<Gamemode>;<GamemodeNum>;<PortV4>;<PortV6>;
```
- `[0]` = `MCPE` (protocol family tag)
- `[1]` = MOTD line 1
- `[2]` = protocol number
- `[3]` = version string
- `[4]` = **current player count**
- `[5]` = **max players**
- `[6]` = server GUID
- `[7]` = level name (rendered as the top line on consoles)
- `[8]/[9]` = gamemode / gamemode number
- `[10]/[11]` = cosmetic IPv4/IPv6 port

**Critical gotcha — the Bedrock RakNet magic (operator-verified via
tcpdump, `bedrock-connect/NOTES.md`):**
```
correct (Bedrock): 00 FF FF 00 FE FE FE FE FD FD FD FD 12 34 56 78
wrong (classic):   00 FF FF 00 FF FF 00 FF FF 00 FF FF 00 FF FF 00
```
BDS **silently drops** the wrong magic — no log, looks like a network black
hole. tcpdump on the BDS host is the only way to confirm the packet arrived.
Reference: <https://vp817.github.io/RakNetProtocolDoc/>.

**Ready-made status libraries (all use RakNet ping for Bedrock; 0xFE only for
Java):**

| Library | Lang | ★ / status | API |
|---|---|---|---|
| `py-mine/mcstatus` | Python | **600★, active** | `mcstatus BedrockServer.status()` |
| `xrjr/mcutils` | Go | 26★, active | `pkg/bedrock` (`Query`) |
| `jgniecki/MinecraftStatus` | PHP | 5★ | RakNet ping (Bedrock) |
| `Dinnerbone/mcstatus` | Python | 1095★, **archived** | historical reference |

Example (Python, `mcstatus`):
```python
from mcstatus import BedrockServer
s = BedrockServer.address(("<your-bds-host>", 19132))
st = s.status()          # sends UnconnectedPing, reads UnconnectedPong
print(st.players.online, st.players.max, st.version, st.gamemode, st.latency)
```

**Best way to get status without the console:** send one RakNet
`UnconnectedPing` (with the **correct** Bedrock magic) and parse the pong —
gives player count, max players, MOTD, version, gamemode, and level name in a
single round-trip. No process access needed. Use the console **only** for
*actions* (kick, op, allowlist, stop, …).

---

## Sources

Primary / authoritative:
- Microsoft Learn — *Get started with the Minecraft Bedrock server*
  (console entry, `help`): <https://learn.microsoft.com/en-us/gaming/minecraft/server/getting-started> (accessed 2026-08-15)
- Microsoft Learn — *Bedrock server properties* (full `server.properties`
  schema; **no `rcon` key**): <https://learn.microsoft.com/en-us/gaming/minecraft/server/properties> (accessed 2026-08-15)
- Mojang `bedrock-samples` **v1.26.40.05** — `mojang-commands.json` (authoritative
  83-command set, permission levels, actions) and
  `bedrock_server_how_to.html` (lifecycle, online backup, property read
  timing). Fetched from the release assets during this session.
- minecraft.wiki — *Bedrock dedicated server* (lists "Doesn't support remote
  console (RCON)"): <https://minecraft.wiki/w/Bedrock_dedicated_server> (accessed 2026-08-15)
- RakNet protocol documentation (magic + UnconnectedPing/Pong):
  <https://vp817.github.io/RakNetProtocolDoc/> (accessed 2026-08-15)

Projects (mechanism + code):
- `TapeWerm/MCscripts` (163★) — `systemd/mcbe@.service`, `systemd/mcbe@.socket`,
  `src/mc_stop.py`, `src/mcbe_backup.sh`:
  <https://github.com/TapeWerm/MCscripts>
- `DMedina559/bedrock-server-manager` (93★) — `subprocess.Popen(stdin=PIPE)`,
  graceful `stop` + kill fallback, `mcstatus` status:
  <https://github.com/DMedina559/bedrock-server-manager>
- `TheRemote/MinecraftBedrockServer` — screen + systemd, `screen -X stuff`:
  <https://github.com/TheRemote/MinecraftBedrockServer>
- `seeruk/bedrockd` (2★), `saulmaldonado/mc-bedrock-runner` (0★),
  `Keito-Klein/Bedrock-Socket`, `xtamtamx/pickaxe-rcon` — RCON wrappers.
- `clvrk/vellum` (50★), `Benjerman/Minecraft-Server-Manager` (49★),
  `crowbarmaster/BedrockManagementService` (41★),
  `imueller64/bedrock-server-manager-dash` (18★), `Ratcoder/WebMC` (13★),
  `SimplyFriday/bedrock-portal` (6★) — web/service managers.
- `py-mine/mcstatus` (600★), `xrjr/mcutils` (26★),
  `jgniecki/MinecraftStatus` (5★) — status libraries.

Local (operator-verified):
- `~/source/bedrock-connect/NOTES.md` — Bedrock RakNet magic, silent drops,
  `online-mode` crash-loop, LAN-game rendering.
- `~/source/bedrock-connect/packaging/bedrock-connect.service` — hardened
  systemd unit template.
- The operator's prior project handover (bedrock-connect) — a running BDS
  1.26.43 that the live pong capture was verified against (see 04).

## Could not verify
- **Exact console echo / per-command stdout confirmation strings** (e.g. does
  the server echo the typed command to stdout; exact text for `say`, `kick`
  ack). All implementations treat it as "write `cmd\n`, read log lines," but
  I did not run the binary to capture a transcript (the 90 MB
  `bedrock_server` download from minecraft.net **timed out** on the network
  too slow).
- **Whether the 1.26.43 running binary matches the 1.26.40.05
  `mojang-commands.json`** exactly (I verified against 1.26.40 metadata;
  1.26.43 is the operator's live build).
- **Existence of hidden/undocumented console commands** beyond
  `mojang-commands.json` (treated as complete for 1.26.40, not proven for
  1.26.43).
- **Exact autosave cadence and whether `stop` always performs a final full
  save** — strongly implied (graceful-shutdown + online-backup design), not
  stated in a primary source found.
- **Pterodactyl first-party Bedrock driver** — not confirmed in this session
  (docs page 404; no driver in search).
- **Java 0xFE query against a Bedrock server** — expected to fail (Bedrock
  uses RakNet ping); not empirically tested here.

