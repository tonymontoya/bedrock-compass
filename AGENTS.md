# AGENTS.md

Operating manual for anyone — human or LLM agent — working in this repository.
Read this **before** writing code or docs. It captures the non-negotiables
and conventions.

## 1. Project identity & non-negotiables

`bedrock-compass` is an open-source, single-binary **TUI manager for one
Minecraft Bedrock Dedicated Server (BDS)**. It runs *on the server host*
(reached over SSH) and drives the server through its only native control
surface — the console — plus host facilities (journal, /proc, filesystem,
firewall). See `README.md` for user-facing context and
`docs/requirements.md` for the decision record.

Hard constraints:
- **No operator specifics in tracked content** (IPs, hostnames, usernames,
  local paths, world names). Private values live in `local/` (gitignored);
  the tool learns its environment from config/flags/autodiscovery. See D5.
- **Read-only by default.** Every mutation of the server or host is an
  explicit user action in the TUI, with confirmation for destructive ones.
  The tool must never auto-modify server config.
- **One static binary, stdlib + charm only.** Small dependency footprint is
  a deliberate property (network-adjacent, runs on a game server box). No
  new external dependency without strong justification.
- **Single server per v1.** The architecture may grow multi-server, but the
  interface, storage, and state are designed around one host, one server.

## 2. Tech stack & architecture

- **Go** (module `github.com/tonymontoya/bedrock-compass`), TUI via the charm
  stack (`bubbletea`/`lipgloss`), config via YAML (`gopkg.in/yaml.v3` or
  stdlib `encoding` if simpler).
- Build on a dev box; deploy by copying one static binary to the host. The
  host may have no toolchains — never assume `go`, `python`, or `node` there.

Planned package layout (refine as it takes shape):
```
cmd/bedrock-compass/         entrypoint, flags, TUI wiring
internal/bedrock/          server domain: console channel, roster, config
                           model, allowlist, lifecycle, save protocol
internal/journal/          systemd journal reader (journald socket)
internal/telemetry/        host metrics (CPU/RAM/disk/net from /proc, /sys)
internal/backup/           live backup (save hold/query/resume) + restore
internal/provision/        install/adopt a server (systemd or tmux)
internal/tui/              bubbletea screens: dashboard, players, logs,
                           backup, access, config, console
```

The one architectural rule: **everything the tool does to the server goes
through the console channel or the well-known file protocol** (allowlist /
permissions JSON, save hold/query/resume, server.properties). No hacks, no
undocumented ABI, no assumption of root beyond what a chosen action needs.

## 3. Commands

```
# build (static)
GOFLAGS=-mod=mod go build -o dist/bedrock-compass ./cmd/bedrock-compass

# test (all must pass)
go test ./...

# vet (must be clean)
go vet ./...

# format check (must produce no output)
gofmt -l .
```

**`go test ./...`, `go vet ./...`, and `gofmt -l .` must all be clean
before any commit.** Run them after every change (the Makefile `check`
target runs all of them plus the static build). CI runs the same trio on
every push and PR — green locally, green upstream.

## 4. Doc status system

Docs carry a status where relevant:

- `active` — current authority; implement against this.
- `adapted` — imported from elsewhere; verify against current vision before
  implementing.
- `legacy` — historical reference only. Do **not** implement.

`docs/requirements.md` is the decision record (D-numbers). ADRs in
`docs/adr/` are created lazily by `/domain-modeling` when a hard-to-reverse
decision is made.

## 5. Conventions

- **Go 1.22+**, idiomatic: errors wrapped with `fmt.Errorf("context: %w",
  err)` at package boundaries; sentinel errors for known failure modes.
- **Doc comments on exported identifiers**, starting with the identifier
  name. No comments inside functions unless genuinely subtle.
- **No goroutines without a shutdown path** (`ctx.Done()` / stop channel).
- **No `panic` outside `main`.**
- **Tests next to code** (`_test.go`); every package has tests; new code
  ships tests in the same change.
- **No emoji in code.** No comments unless asked.
- Prefer editing existing files over creating new ones; never create docs
  unprompted.
- **Never commit unless explicitly asked.**
- Surface assumptions and hazards rather than silently "fixing" them.

## 6. Working with an LLM agent

- Always read this file (and `CONTEXT.md` + relevant `docs/adr/`) before
  producing code.
- Trust is earned through feedback loops: types, tests, and running the
  actual program against a real (or scratch) server.
- LLM-generated code/content is not trusted without a green test run.
- When behavior depends on a specific BDS version, say which version was
  verified and where (see `docs/research/04-live-verification.md`).

## 7. Agent skills

### Issue tracker

GitHub Issues once the repo is public (planned:
`github.com/tonymontoya/bedrock-compass`); until then, local tickets in
`docs/tickets/NNN-short-title.md`, each with: goal, Definition of Done,
verification, key references, blocking edges.

### Triage labels

Five canonical roles when a tracker exists: `needs-triage`, `needs-info`,
`ready-for-agent`, `ready-for-human`, `wontfix`. Locally, use the same
names in the ticket file header.

### Domain docs

Single-context: read `AGENTS.md` + `CONTEXT.md` first; `docs/adr/` is created
lazily by `/domain-modeling`.

## 8. Execution approach (agentic workflow)

This repo uses the **agentic engineering skill set**. Apply this workflow
consistently so any session can resume where the last left off.

**Ship-a-change chain (run per change):**
1. **`/grill-with-docs`** *before* writing anything — align on the change and
   capture decisions into `CONTEXT.md` + `docs/adr/`. Never skip it for
   decision-rich work. (No codebase yet? Use `/grill-me`.) Use **`/ask-ai`**
   any time you're unsure which skill fits.
2. **`/to-tickets`** (chartable work) or **`/to-spec`** (single spec) —
   publish to the tracker. Each ticket carries **Definition of Done** +
   verification + key references + blocking edges.
3. **`/implement`** — build it, driving **`/tdd`** (red-green-refactor) at
   code seams and closing with **`/code-review`** (Standards + Spec) before
   committing.

**Cadence & sizing:**
- **One ticket per session**, sized to stay within the model's sharp
  reasoning window. Clear context between tickets.
- Work the **frontier** — open tickets whose blockers are all closed — one
  at a time.

**Choose the planning skill by fog, not habit:**
- **`/to-tickets`** when the way is charted (deliverables + dependencies
  known).
- **`/wayfinder`** only for huge, foggy work where the route isn't visible
  yet.

**On-ramps onto the main flow:**
- Incoming bugs/requests → **`/triage`**.
- Something broken → **`/diagnosing-bugs`**.
- A huge, foggy effort → **`/wayfinder`**.

**Codebase health:**
- Run **`/improve-codebase-architecture`** every few sessions once real code
  accrues.

**Hygiene:**
- `CONTEXT.md` + `docs/adr/` are the shared language — keep them current.
- **`/handoff`** to bridge context windows; **`/compact`** to continue in
  place.

**Precondition:** run **`/setup-skills`** once to tailor the issue tracker,
triage labels, and doc layout to this project.
