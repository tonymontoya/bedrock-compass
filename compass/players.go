// Package compass: the Players service (ticket #6). It is the single
// execution path for the live roster and the per-player actions (kick /
// op / deop / allow): one console round-trip each, classified against the
// failure markers Bedrock actually emits, audit-logged through the Trail
// (D11). The TUI's Players screen is a thin renderer over it; nothing else
// knows about the other.
//
// Two data sources feed the screen:
//   - the live roster: the console `list` command ("There are N/M players
//     online:" plus one line per player);
//   - "recently connected": the unit's journal, mined for the shared
//     connect/disconnect lines (D3b). ParsePlayerLine is exported because
//     the Logs screen (ticket #8, D9) reuses it.
package compass

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Player identifies one player by gamertag and, when known, XUID. The tool
// never asks the operator for an XUID (D3b): actions resolve gamertag-first.
type Player struct {
	Name string // gamertag
	XUID string // "" when unknown
}

// ID returns the identifier to send in a console command: gamertag-first
// (D3b) — the server resolves the XUID itself, and Bedrock's player commands
// take the name. The XUID is only used when no gamertag is known at all.
func (p Player) ID() string {
	if p.Name != "" {
		return p.Name
	}
	return p.XUID
}

// Roster is one `list` round-trip's result.
type Roster struct {
	Online []Player // players currently in the world
	Count  int      // from the "There are N/M" header
	Max    int      // max-players, same header
}

// RecentPlayer is one distinct player mined from journal connect/disconnect
// lines (D3b): survives the player leaving, which is what makes "allow after
// they leave" possible.
type RecentPlayer struct {
	Name     string
	XUID     string // "" when no line carried one
	LastSeen time.Time
	State    string // last event seen: "connected" or "disconnected"
}

// PlayerEventKind classifies a journal line about player presence.
type PlayerEventKind int

const (
	PlayerEventNone PlayerEventKind = iota
	PlayerEventConnected
	PlayerEventDisconnected
)

var (
	// listHeaderRE matches the `list` command's header line, e.g.
	// "There are 2/15 players online:" (case-insensitive; verified live).
	listHeaderRE = regexp.MustCompile(`(?i)there are (\d+)/(\d+) players online`)

	// playerTagRE extracts the server's own timestamp from its log tag
	// "[2026-08-19 01:54:21:901 INFO]" so recently-connected rows can show
	// when they were last seen. Milliseconds are dropped (second precision).
	playerTagRE = regexp.MustCompile(`\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}):\d{3}\s+\w+\]`)

	// xuidDigitsRE pulls the digit run following "xuid:" in a log line.
	xuidDigitsRE = regexp.MustCompile(`xuid:\s*(\d+)`)
)

// ParsePlayerLine extracts the gamertag and XUID from one server log line:
//
//	Player connected: <name>, xuid: <digits>
//	Player disconnected: <name>, xuid: <digits>[, pfid: <hex>]
//
// It accepts a full journalctl line or just the message part (the syslog
// prefix and the server's own "[ts LEVEL]" tag are skipped). Any other line
// yields ok=false. This is the shared parser D9 reuses for the Logs screen.
func ParsePlayerLine(line string) (name, xuid string, kind PlayerEventKind, ok bool) {
	var marker string
	switch {
	case strings.Contains(line, "Player connected:"):
		marker = "Player connected:"
		kind = PlayerEventConnected
	case strings.Contains(line, "Player disconnected:"):
		marker = "Player disconnected:"
		kind = PlayerEventDisconnected
	default:
		return "", "", PlayerEventNone, false
	}
	rest := line[strings.Index(line, marker)+len(marker):]
	name = rest
	if i := strings.IndexByte(rest, ','); i >= 0 {
		name = rest[:i]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", PlayerEventNone, false
	}
	if m := xuidDigitsRE.FindStringSubmatch(rest); m != nil {
		xuid = m[1]
	}
	return name, xuid, kind, true
}

// ParseRoster parses the harvested output of a `list` command into a Roster.
// The per-player line format is defensive: it accepts "Name [xuid: N]",
// "Name (N)", and a bare gamertag — the exact shape has not been captured
// live with players online, so all three are handled and XUIDs missing from
// `list` are backfilled by the caller from journal history. Lines that are
// connect/disconnect events (interleaved journal noise) are never treated as
// roster rows, and more rows than the header's count are trimmed to it.
func ParseRoster(out string) (Roster, error) {
	var r Roster
	started := false
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if !started {
			m := listHeaderRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			r.Count, _ = strconv.Atoi(m[1])
			r.Max, _ = strconv.Atoi(m[2])
			started = true
			continue
		}
		if line == "" {
			continue
		}
		if _, _, _, isEvent := ParsePlayerLine(line); isEvent {
			continue // journal event interleaved into the harvest window
		}
		r.Online = append(r.Online, parseRosterRow(line))
	}
	if !started {
		return Roster{}, errors.New("no roster line in list output")
	}
	if r.Count > 0 && len(r.Online) > r.Count {
		r.Online = r.Online[:r.Count] // trim interleaved noise past the count
	}
	return r, nil
}

// parseRosterRow parses one per-player line of `list` output:
// "Name [xuid: N]", "Name (N)", or a bare gamertag.
func parseRosterRow(line string) Player {
	if i := strings.Index(line, "[xuid:"); i >= 0 {
		name := strings.TrimSpace(line[:i])
		digits := digitsIn(line[i+len("[xuid:"):]) // " 1111111111111111]" -> the run
		if name != "" && digits != "" {
			return Player{Name: name, XUID: digits}
		}
		if name != "" {
			return Player{Name: name}
		}
	}
	if i := strings.LastIndex(line, "("); i >= 0 && strings.HasSuffix(line, ")") {
		inner := line[i+1 : len(line)-1]
		if isDigits(inner) {
			name := strings.TrimSpace(line[:i])
			if name != "" {
				return Player{Name: name, XUID: inner}
			}
		}
	}
	return Player{Name: line}
}

// digitsIn returns the first run of digits in s ("" when there is none).
func digitsIn(s string) string {
	start := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			return s[start:i]
		}
	}
	if start >= 0 {
		return s[start:]
	}
	return ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Players is the ticket #6 service: roster reads and per-player actions over
// one console channel, audit-logged. Console may be nil (a deployment with no
// resolved channel) — every method then fails cleanly instead of panicking.
type Players struct {
	Console *Console
	Trail   *Trail
	// Journal mines the unit's log for recently-connected players (D3b).
	// Nil when the deployment has no journal (e.g. a tmux setup); Recent
	// then degrades to an error the screen surfaces as "unavailable".
	Journal *Journal
}

// NewPlayers wires the service over already-resolved seams (the Stack's).
func NewPlayers(console *Console, trail *Trail, journal *Journal) *Players {
	return &Players{Console: console, Trail: trail, Journal: journal}
}

// Roster sends `list` and parses its reply. A parseable roster is still an
// error when the send itself failed (e.g. timeout with partial output).
func (s *Players) Roster(ctx context.Context) (Roster, error) {
	if s.Console == nil {
		return Roster{}, errors.New("no console channel resolved")
	}
	out, err := s.Console.Send(ctx, "list")
	r, perr := ParseRoster(out)
	if perr != nil {
		if err != nil {
			return Roster{}, err
		}
		return Roster{}, fmt.Errorf("console list: %w (output: %q)", perr, out)
	}
	return r, err
}

// Kick disconnects one player: `kick <gamertag>` (D3b gamertag-first). The
// reply is classified against the failure markers Bedrock emits for a target
// that matches nobody. detail is "" on success (the server prints nothing —
// verified live) and err carries the verbatim failure line when it did not.
func (s *Players) Kick(ctx context.Context, p Player) (string, error) {
	return s.playerAction(ctx, "kick", "kick "+p.ID(), "", p)
}

// Op grants operator status: `op <gamertag>`.
func (s *Players) Op(ctx context.Context, p Player) (string, error) {
	return s.playerAction(ctx, "op", "op "+p.ID(), "", p)
}

// Deop removes operator status: `deop <gamertag>`.
func (s *Players) Deop(ctx context.Context, p Player) (string, error) {
	return s.playerAction(ctx, "deop", "deop "+p.ID(), "", p)
}

// Allow adds the player to the allowlist — Bedrock's join gate (D3a). The
// command carries the gamertag when known (the server resolves the XUID on
// first join), else the XUID: `allowlist add <identity>`. This is the handoff
// into the Access flow (ticket #7 builds the full screen around it). On
// success detail carries the server's confirmation line.
func (s *Players) Allow(ctx context.Context, p Player) (string, error) {
	return s.playerAction(ctx, "allow", "allowlist add "+p.ID(), "to the allowlist", p)
}

// playerAction sends one console command for a player, classifies the reply,
// and records the outcome in the audit trail (D11). successMarker is the
// substring that confirms success when the server prints one ("to the
// allowlist" for allow); empty means silence-on-success (kick/op/deop print
// nothing on success — verified live against 1.26.43). The returned detail
// is a short success note for the toast; err carries the verbatim failure
// line when the server rejected the command.
func (s *Players) playerAction(ctx context.Context, action, command, successMarker string, p Player) (string, error) {
	if s.Console == nil {
		err := errors.New("no console channel resolved")
		s.audit(action, p, err)
		return "", err
	}
	out, sendErr := s.Console.Send(ctx, command)
	detail, ok := classifyReply(out, successMarker)
	var actErr error
	if !ok {
		actErr = errors.New(detail) // detail holds the verbatim failure line
	} else if sendErr != nil {
		actErr = fmt.Errorf("%w (partial output: %q)", sendErr, out)
		detail = ""
	}
	s.audit(action, p, actErr)
	return detail, actErr
}

// classifyReply inspects a harvested console reply. It returns (line, false)
// when the reply carries one of Bedrock's known failure markers for a player
// command that matched nobody — line is the most informative such line,
// verbatim: "Could not find player …" / "No targets matched selector"
// (kick/op/deop on an unknown target) and "Could not add … to the allowlist"
// (duplicate allowlist entry), all verified live against 1.26.43. When a
// reply carries several markers, the one that names the target wins — it is
// what the operator needs to see in the toast. Otherwise it returns
// (successLine, true): successLine is the confirming line when successMarker
// was found, else "".
func classifyReply(out string, successMarker string) (string, bool) {
	var generic, specific string // "No targets matched selector" vs naming lines
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch {
		case strings.Contains(line, "Could not find player"),
			strings.Contains(line, "Could not add"):
			if specific == "" {
				specific = line
			}
		case strings.Contains(line, "No targets matched selector"):
			if generic == "" {
				generic = line
			}
		}
	}
	if specific != "" || generic != "" {
		if specific != "" {
			return specific, false
		}
		return generic, false
	}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if successMarker != "" && strings.Contains(line, successMarker) {
			return line, true
		}
	}
	return "", true
}

// audit appends one event per player action (D11: every mutating action is
// recorded). A nil trail (audit disabled) is a no-op.
func (s *Players) audit(action string, p Player, err error) {
	if s.Trail == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error: " + err.Error()
	}
	params := map[string]string{"action": action, "player": p.Name}
	if p.XUID != "" {
		params["xuid"] = p.XUID
	}
	_ = s.Trail.Append(Event{Verb: "player", Params: params, Result: result})
}

// recentJournalTail bounds how many journal entries Recent scans per call.
// 500 is plenty for "who was here recently" (a busy world logs a handful of
// lines per join/leave) and keeps the periodic refresh cheap on a
// long-running server.
const recentJournalTail = 500

// Recent returns the distinct players seen in the unit's journal tail,
// newest activity first (D3b). It is the production implementation behind
// the screen's "recently connected" list; it degrades to an error when the
// deployment has no journal (tmux setups), which the screen surfaces as
// "unavailable".
func (s *Players) Recent(ctx context.Context) ([]RecentPlayer, error) {
	if s.Journal == nil {
		return nil, errors.New("no journal for this deployment")
	}
	lines, err := s.Journal.RecentLines(ctx, recentJournalTail)
	if err != nil {
		return nil, fmt.Errorf("journal read for player history: %w", err)
	}
	return MinePlayerHistory(lines), nil
}

// MinePlayerHistory is the pure mining step behind Recent (and what tests
// exercise offline): it folds server log lines into distinct players keyed
// by XUID when one was seen, else by lowercased gamertag — so a line without
// an XUID still correlates with earlier ones. The result is capped at 25
// entries and ordered newest activity first.
func MinePlayerHistory(lines []string) []RecentPlayer {
	type entry struct {
		name  string
		xuid  string
		state string
		at    time.Time
	}
	byKey := map[string]*entry{}
	var order []string               // keys in last-activity order (journal order)
	nameIndex := map[string]string{} // lowercased name -> key, so a line without an XUID still correlates

	for _, line := range lines {
		name, xuid, kind, ok := ParsePlayerLine(line)
		if !ok {
			continue
		}
		key := xuid
		if key == "" {
			if k, known := nameIndex[strings.ToLower(name)]; known {
				key = k // same player seen earlier with an XUID
			} else {
				key = strings.ToLower(name)
			}
		}
		state := "disconnected"
		if kind == PlayerEventConnected {
			state = "connected"
		}
		at := parsePlayerTag(line)
		e, exists := byKey[key]
		if !exists {
			e = &entry{name: name}
			byKey[key] = e
			order = append(order, key)
		} else {
			for i, k := range order { // move-to-end keeps last-activity order
				if k == key {
					order = append(order[:i], order[i+1:]...)
					break
				}
			}
			order = append(order, key)
		}
		e.name = name // latest line wins (names are stable per XUID anyway)
		if xuid != "" {
			e.xuid = xuid
		}
		nameIndex[strings.ToLower(name)] = key
		e.state = state
		if at.After(e.at) || e.at.IsZero() {
			e.at = at
		}
	}

	const cap = 25
	if len(order) > cap {
		order = order[len(order)-cap:]
	}
	out := make([]RecentPlayer, 0, len(order))
	for i := len(order) - 1; i >= 0; i-- { // newest activity first
		e := byKey[order[i]]
		out = append(out, RecentPlayer{Name: e.name, XUID: e.xuid, LastSeen: e.at, State: e.state})
	}
	return out
}

// parsePlayerTag pulls the server's own timestamp out of a log line's
// "[ts LEVEL]" tag; zero time when absent (e.g. tmux pane lines).
func parsePlayerTag(line string) time.Time {
	m := playerTagRE.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02 15:04:05", m[1])
	if err != nil {
		return time.Time{}
	}
	return t
}
