// Headless verbs (ticket #15, ADR-0010). Every verb runs on the same
// compass.Stack the TUI uses — no forked logic — and appends one audit event
// per invocation. Mutating verbs require --yes; without it they print what
// would happen and exit 3 (the ADR-0010 confirmation policy), touching
// nothing. Read-only verbs run unconditionally.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// exitConfirmation is the ADR-0010 code for "confirmation required but not
// given": distinct from failure (1) and usage error (2), so scripts can tell
// a refused mutation apart from both.
const exitConfirmation = 3

// wireStack loads the config at cfgPath (the standard path when empty) and
// wires a Stack over it, printing + returning exit code 1 on failure. It is
// the shared headless plumbing behind every verb in this file. Tests swap
// newStackFor for a fake so the run* handlers can be exercised offline.
var newStackFor = func(ctx context.Context, cfg *compass.Config) (*compass.Stack, error) {
	return compass.NewStack(ctx, cfg, version, compass.StackOpts{})
}

func wireStack(name, cfgPath string) (*compass.Stack, context.Context, context.CancelFunc, int) {
	if cfgPath == "" {
		cfgPath = compass.StandardPath()
	}
	cfg, err := compass.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bedrock-compass %s: config: %v\n", name, err)
		return nil, nil, nil, 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	st, err := newStackFor(ctx, cfg)
	if err != nil {
		cancel()
		fmt.Fprintf(os.Stderr, "bedrock-compass %s: wiring: %v\n", name, err)
		return nil, nil, nil, 1
	}
	return st, ctx, cancel, 0
}

// auditCLI appends one headless-invocation event to the trail (ADR-0010):
// verb "cli", params cmd + key args, result ok/error, duration. A nil trail
// (audit disabled or unresolvable) is a no-op — the action itself still ran.
func auditCLI(trail *compass.Trail, cmd string, params map[string]string, err error, start time.Time) {
	if trail == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error: " + err.Error()
	}
	p := map[string]string{"cmd": cmd}
	for k, v := range params {
		p[k] = v
	}
	_ = trail.Append(compass.Event{Verb: "cli", Params: p, Result: result, Duration: time.Since(start)})
}

// gate is the ADR-0010 confirmation step for one mutating verb. Without --yes
// it prints what would happen, records the refusal in the trail (so a stray
// destructive attempt still leaves a trace), and returns exit code 3 without
// touching anything. With --yes it notes the bypass on params — the verb's own
// audit event carries it — and returns 0 to proceed.
func gate(cmd string, yes bool, summary string, trail *compass.Trail, params map[string]string, start time.Time) int {
	if !yes {
		fmt.Fprintf(os.Stderr, "bedrock-compass %s: refusing without --yes\n", cmd)
		fmt.Fprintf(os.Stderr, "  would: %s\n", summary)
		auditCLI(trail, cmd, params, errors.New("confirmation required; pass --yes to proceed"), start)
		return exitConfirmation
	}
	params["bypass"] = "--yes"
	return 0
}

// parseSince parses a --since value as local wall clock (the same
// interpretation journalctl applies): YYYY-MM-DD HH:MM[:SS].
func parseSince(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse %q as YYYY-MM-DD HH:MM[:SS]", s)
}

// humanSize renders a byte count for one line of output.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// --- roster -----------------------------------------------------------------

type playerJSON struct {
	Name string `json:"name"`
	XUID string `json:"xuid,omitempty"`
}

type rosterJSON struct {
	Count  int          `json:"count"`
	Max    int          `json:"max"`
	Online []playerJSON `json:"online"`
}

// runRoster prints the live player list (the console `list` round-trip).
func runRoster(rest []string) int {
	start := time.Now()
	fs := flag.NewFlagSet("roster", flag.ContinueOnError)
	config := fs.String("config", "", "config file path (default: the standard path)")
	showVersion := fs.Bool("version", false, "print version and exit")
	jsonOut := fs.Bool("json", false, "print the roster as JSON")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // ADR-0010: help is ok (exit 0); usage already printed
		}
		return 2
	}
	if *showVersion {
		fmt.Println("bedrock-compass " + version)
		return 0
	}
	st, ctx, cancel, code := wireStack("roster", *config)
	if code != 0 {
		return code
	}
	defer cancel()

	r, err := st.Players.Roster(ctx)
	auditCLI(st.Trail, "roster", nil, err, start)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bedrock-compass roster: %v\n", err)
		return 1
	}
	if *jsonOut {
		out := rosterJSON{Count: r.Count, Max: r.Max}
		for _, p := range r.Online {
			out.Online = append(out.Online, playerJSON{Name: p.Name, XUID: p.XUID})
		}
		enc, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(enc))
		return 0
	}
	fmt.Print(renderRoster(r))
	return 0
}

// renderRoster is the plain-text roster: one header + one line per player.
func renderRoster(r compass.Roster) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d/%d players online:\n", r.Count, r.Max)
	for _, p := range r.Online {
		if p.XUID != "" {
			fmt.Fprintf(&b, "  %s (xuid %s)\n", p.Name, p.XUID)
		} else {
			fmt.Fprintf(&b, "  %s\n", p.Name)
		}
	}
	return b.String()
}

// --- logs -------------------------------------------------------------------

type logJSON struct {
	Time  string `json:"time,omitempty"`
	Level string `json:"level,omitempty"`
	Text  string `json:"text"`
}

// runLogs prints recent journal lines, optionally narrowed by level and a
// since-bound. Text is printed verbatim: the server's own "[ts LEVEL]" tag is
// already part of it (ParseLogLine keeps it), so nothing is re-printed.
func runLogs(rest []string) int {
	start := time.Now()
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	config := fs.String("config", "", "config file path (default: the standard path)")
	showVersion := fs.Bool("version", false, "print version and exit")
	jsonOut := fs.Bool("json", false, "print entries as a JSON array")
	level := fs.String("level", "", "only entries at this level: INFO, WARN, or ERROR (case-insensitive)")
	since := fs.String("since", "", "entries from this time on: YYYY-MM-DD HH:MM[:SS] (local wall clock)")
	lines := fs.Int("lines", 50, "how many recent lines to read (ignored with --since)")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // ADR-0010: help is ok (exit 0); usage already printed
		}
		return 2
	}
	if *showVersion {
		fmt.Println("bedrock-compass " + version)
		return 0
	}
	if *level != "" {
		switch strings.ToUpper(*level) {
		case "INFO", "WARN", "ERROR":
		default:
			fmt.Fprintln(os.Stderr, "bedrock-compass logs: --level must be INFO, WARN, or ERROR")
			return 2
		}
	}
	if *lines < 1 {
		fmt.Fprintln(os.Stderr, "bedrock-compass logs: --lines must be >= 1")
		return 2
	}
	var sinceT time.Time
	var err error
	if *since != "" {
		sinceT, err = parseSince(*since)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bedrock-compass logs: %v\n", err)
			return 2
		}
	}

	st, ctx, cancel, code := wireStack("logs", *config)
	if code != 0 {
		return code
	}
	defer cancel()

	params := map[string]string{}
	var entries []compass.LogEntry
	switch {
	case st.Logs == nil:
		err = errors.New("no journal resolved (tmux deployments have no unit)")
	default:
		if *since != "" {
			params["since"] = *since
			entries, err = st.Logs.Range(ctx, sinceT, time.Now())
		} else {
			entries, _, err = st.Logs.Snapshot(ctx, *lines)
		}
	}
	if *level != "" {
		params["level"] = strings.ToUpper(*level)
		entries = compass.FilterEntries(entries, compass.LogFilters{Level: *level})
	}
	auditCLI(st.Trail, "logs", params, err, start)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bedrock-compass logs: %v\n", err)
		return 1
	}
	if *jsonOut {
		out := make([]logJSON, 0, len(entries))
		for _, e := range entries {
			lj := logJSON{Text: e.Text}
			if !e.Time.IsZero() {
				lj.Time = e.Time.Format(time.RFC3339)
			}
			lj.Level = e.Level
			out = append(out, lj)
		}
		enc, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(enc))
		return 0
	}
	for _, e := range entries {
		fmt.Println(e.Text)
	}
	return 0
}

// --- allowlist ---------------------------------------------------------------

// extractKnownFlags pulls the headless flag set out of args wherever it
// appears and returns (flagArgs, positionals). The stdlib parser stops at the
// first non-flag argument, so `allowlist add Tony --config X` would otherwise
// misread --config as a second identity; pre-extraction makes flags work in
// any position — including before the subcommand (`--config X allowlist ...`).
// A value-taking flag consumes its following argument.
func extractKnownFlags(args []string) (flagArgs, positionals []string) {
	takesValue := map[string]bool{"--config": true, "-config": true}
	noValue := map[string]bool{"--yes": true, "-yes": true, "--version": true, "-version": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case noValue[a]:
			flagArgs = append(flagArgs, a)
		case takesValue[a]:
			flagArgs = append(flagArgs, a)
			if i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
		default:
			positionals = append(positionals, a)
		}
	}
	return flagArgs, positionals
}

// hasHelp reports whether args carry an explicit -h/--help anywhere — the
// ADR-0010 contract treats help as ok (exit 0) for every verb, including the
// ones that pre-scan their arguments before flag parsing.
func hasHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// runAllowlist manages the join gate's list: `allowlist add|remove <identity>`.
// Both are mutating and gated by --yes (ADR-0010). Flags may appear in any
// position (extractKnownFlags pulls them out before parsing), so a leading
// --config does not masquerade as the action.
func runAllowlist(rest []string) int {
	// --version and -h/--help are honored before the positional check so
	// `allowlist --version` prints the version (exit 0) like every other verb,
	// and `allowlist -h` prints usage (exit 0, ADR-0010: help is ok).
	for _, a := range rest {
		if a == "--version" || a == "-version" {
			fmt.Println("bedrock-compass " + version)
			return 0
		}
	}
	if hasHelp(rest) {
		fmt.Fprintln(os.Stderr, "usage: bedrock-compass allowlist <add|remove> <identity> [--yes]")
		return 0
	}

	flagArgs, positionals := extractKnownFlags(rest)
	if len(positionals) == 0 {
		fmt.Fprintln(os.Stderr, "usage: bedrock-compass allowlist <add|remove> <identity> [--yes]")
		return 2
	}
	sub := positionals[0]
	if sub != "add" && sub != "remove" {
		fmt.Fprintf(os.Stderr, "bedrock-compass allowlist: unknown action %q (want add or remove)\n", sub)
		return 2
	}

	start := time.Now()
	fs := flag.NewFlagSet("allowlist "+sub, flag.ContinueOnError)
	config := fs.String("config", "", "config file path (default: the standard path)")
	yes := fs.Bool("yes", false, "confirm this change (required; ADR-0010)")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	args := append(fs.Args(), positionals[1:]...)
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: bedrock-compass allowlist %s <identity> [--yes]\n", sub)
		return 2
	}
	identity := args[0]

	st, ctx, cancel, code := wireStack("allowlist "+sub, *config)
	if code != 0 {
		return code
	}
	defer cancel()

	params := map[string]string{"action": sub, "identity": identity}
	verb := "allowlist " + sub
	summary := fmt.Sprintf("%s %s on the allowlist (atomic write, reload into a running server, verify)", sub, identity)
	if code := gate(verb, *yes, summary, st.Trail, params, start); code != 0 {
		return code
	}

	var detail string
	var err error
	switch sub {
	case "add":
		detail, err = st.Access.Add(ctx, identity)
	default: // remove
		detail, err = st.Access.Remove(ctx, identity)
	}
	auditCLI(st.Trail, verb, params, err, start)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bedrock-compass %s: %v\n", verb, err)
		return 1
	}
	fmt.Printf("%s: %s\n", verb, detail)
	return 0
}

// --- restore ------------------------------------------------------------------

// runRestore rolls the server back to one backup. Mutating and gated by --yes;
// the refusal message names what would happen (including the backup's age and
// size when its manifest is readable). Undo = restoring the safety-net id the
// success line prints.
func runRestore(rest []string) int {
	start := time.Now()
	// --version and -h/--help are honored before the positional check so
	// `restore --version` prints the version (exit 0) like every other verb,
	// and `restore -h` prints usage (exit 0, ADR-0010: help is ok) instead of
	// being misread as a backup id that trips the gate.
	for _, a := range rest {
		if a == "--version" || a == "-version" {
			fmt.Println("bedrock-compass " + version)
			return 0
		}
	}
	if hasHelp(rest) {
		fmt.Fprintln(os.Stderr, "usage: bedrock-compass restore <backup-id> [--yes]")
		return 0
	}
	flagArgs, positionals := extractKnownFlags(rest)
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	config := fs.String("config", "", "config file path (default: the standard path)")
	yes := fs.Bool("yes", false, "confirm this restore (required; ADR-0010)")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	args := append(fs.Args(), positionals...)
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: bedrock-compass restore <backup-id> [--yes]")
		return 2
	}
	id := args[0]

	st, ctx, cancel, code := wireStack("restore", *config)
	if code != 0 {
		return code
	}
	defer cancel()

	summary := fmt.Sprintf("restore backup %s (stops the server; a safety-net snapshot is taken first)", id)
	if b, ierr := st.Engine.Inspect(id); ierr == nil {
		summary = fmt.Sprintf("restore backup %s from %s (%s mode, %s) — stops the server; safety-net snapshot taken first",
			id, b.Created.Format("2006-01-02 15:04"), b.Mode, humanSize(b.TotalSize))
	}
	params := map[string]string{"id": id}
	if code := gate("restore", *yes, summary, st.Trail, params, start); code != 0 {
		return code
	}

	res, err := st.Engine.Restore(ctx, id)
	auditCLI(st.Trail, "restore", params, err, start)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bedrock-compass restore: %v\n", err)
		return 1
	}
	fmt.Printf("restored %s (undo: bedrock-compass restore %s --yes)\n", id, res.SafetyNet)
	return 0
}

// --- audit ---------------------------------------------------------------------

// runAudit prints the operation trail, most-recent-first. The read happens
// before this invocation's own event is appended, so a run never lists itself.
func runAudit(rest []string) int {
	start := time.Now()
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	config := fs.String("config", "", "config file path (default: the standard path)")
	showVersion := fs.Bool("version", false, "print version and exit")
	jsonOut := fs.Bool("json", false, "print events as a JSON array")
	limit := fs.Int("limit", 20, "how many recent events to print")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // ADR-0010: help is ok (exit 0); usage already printed
		}
		return 2
	}
	if *showVersion {
		fmt.Println("bedrock-compass " + version)
		return 0
	}

	st, _, cancel, code := wireStack("audit", *config)
	if code != 0 {
		return code
	}
	defer cancel()

	if st.Trail == nil {
		err := errors.New("no audit trail resolved (audit-file unset or unwritable)")
		fmt.Fprintf(os.Stderr, "bedrock-compass audit: %v\n", err)
		return 1
	}
	evs, rerr := st.Trail.Recent(*limit)
	auditCLI(st.Trail, "audit", map[string]string{"limit": strconv.Itoa(*limit)}, rerr, start)
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "bedrock-compass audit: %v\n", rerr)
		return 1
	}
	if *jsonOut {
		out := make([]compass.Event, len(evs)) // non-nil so an empty trail prints [] not null
		copy(out, evs)
		enc, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(enc))
		return 0
	}
	for _, ev := range evs {
		fmt.Println(auditLine(ev))
	}
	return 0
}

// auditLine renders one event: time, verb[/action], result, sorted k=v params,
// and the duration when recorded. Params are sorted so output is stable for a
// given trail (map iteration order is not). CLI invocation events render their
// command as the verb label (the cmd param is already shown there), so an
// allowlist add reads "allowlist add ok ..." rather than "cli/add ...".
func auditLine(ev compass.Event) string {
	var b strings.Builder
	b.WriteString(ev.Time.Local().Format("2006-01-02 15:04:05"))
	b.WriteByte(' ')
	verb := ev.Verb
	if verb == "cli" {
		if c := ev.Params["cmd"]; c != "" {
			verb = c
		}
	}
	b.WriteString(verb)
	if a := ev.Params["action"]; a != "" && ev.Verb != "cli" {
		b.WriteString("/" + a)
	}
	b.WriteByte(' ')
	b.WriteString(ev.Result)
	keys := make([]string, 0, len(ev.Params))
	for k := range ev.Params {
		if k == "action" || (ev.Verb == "cli" && k == "cmd") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%s", k, ev.Params[k])
	}
	if ev.Duration > 0 {
		fmt.Fprintf(&b, " (%s)", ev.Duration.Round(time.Millisecond))
	}
	return b.String()
}

// --- help ------------------------------------------------------------------------

// usageText lists every verb with a one-line description and an example
// (ticket #15 acceptance), plus the global flags and exit codes.
func usageText() string {
	return `bedrock-compass — TUI manager for a Minecraft Bedrock Dedicated Server.

With no subcommand it runs the interactive TUI. Headless verbs:

  status                  server state, unit, pid, uptime, live probe        [--json]
  roster                  who is online now                                  [--json]
  logs                    recent journal lines      --level L --since T --lines N [--json]
  allowlist add <id>      put a gamertag/XUID on the allowlist               [--yes]
  allowlist remove <id>   take one off                                        [--yes]
  backup-now              run one backup now          --mode live|stopped    [--yes]
  restore <id>            roll back to a backup (safety-net snapshot first)  [--yes]
  audit                   the operation trail, newest first                  [--limit N --json]
  start | stop | restart  lifecycle actions over the supervisor
  adopt                   claim an existing deployment into config

Global flags: --config PATH (default ~/.config/bedrock-compass/config.yaml),
--version. Mutating verbs refuse without --yes and exit 3 (ADR-0010); every
invocation is audit-logged. Exit codes: 0 ok, 1 runtime error, 2 usage error,
3 confirmation required.

Examples:
  bedrock-compass status
  bedrock-compass roster --json
  bedrock-compass logs --level WARN --since "2026-08-25 09:00"
  bedrock-compass allowlist add Tony75264 --yes
  bedrock-compass backup-now --mode live --yes
  bedrock-compass restore <backup-id> --yes
`
}
