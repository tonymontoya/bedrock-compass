// Package tui: the Logs screen (ticket #8). It is a thin renderer over the
// compass Logs service (D9): a live tail of the unit's journal with ~5,000
// lines of scrollback, pause/resume, and filters that compose — level
// (INFO/WARN/ERROR from the server's own "[ts LEVEL]" tag), text search (the
// shell's "/" via SearchFilterer), a player filter built on the shared
// connect/disconnect parser, and historical time-range queries (last hour /
// today / custom). Reader-only: nothing here writes to or rotates logs.
//
// Freshness without manual refresh follows the Players pattern: the shell
// polls status every 2 s and broadcasts it to the active screen; this screen
// piggybacks that tick to poll for new journal lines while its tab is active
// and unpaused. Journal reads are independent processes (no console channel),
// so no mutex is needed — concurrent reads cannot interleave each other's
// output, and state is only mutated inside Update.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// logScrollback caps the in-memory scrollback (the ticket's ~5,000 lines).
const logScrollback = 5000

// logsView selects which entry set is displayed.
type logsView int

const (
	viewLive       logsView = iota // live tail + poll of the journal
	viewHistorical                 // one-shot time-range query result
)

// logsInputMode selects which free-text field (if any) is active. The screen
// uses a single buffer for both; the mode decides what Enter applies.
type logsInputMode int

const (
	logsInputNone   logsInputMode = iota
	logsInputPlayer               // gamertag filter
	logsInputRange                // custom time range: "since[, until]"
)

// Logs is the ticket #8 screen over a resolved Stack's Logs service. A nil
// stack (or one without a journal — tmux deployments with no unit name)
// renders a degraded state.
type Logs struct {
	stack *compass.Stack

	entries []compass.LogEntry // live scrollback (oldest first), capped
	hist    []compass.LogEntry // historical query result, while viewHistorical
	cursor  string             // journal cursor after the last read
	view    logsView
	paused  bool
	loaded  bool // initial snapshot has landed

	loadErr    string // initial snapshot failure ("" when fresh)
	pollErr    string // last poll failure (transient; shown, not toasted)
	rangeErr   string // last range query failure
	rangeLabel string // "all", "last hour", "today", or the custom spec
	since      time.Time
	until      time.Time

	filters compass.LogFilters // Level / Text / Player (time is view-mode driven)

	mode  logsInputMode
	buf   string
	errIn string

	offset int // lines hidden below the viewport; 0 = stuck to the newest line

	width  int
	height int
}

// NewLogs builds the Logs screen over a resolved Stack. The first snapshot is
// kicked off in Init.
func NewLogs(stack *compass.Stack) *Logs {
	return &Logs{stack: stack, width: 80, height: 24}
}

func (l *Logs) Tab() string { return "Logs" }

// CapturingKeys reports whether the screen owns raw keyboard input — i.e. a
// filter field is open. While true the shell routes every key here and none
// of its global shortcuts apply (the gamertag may contain digits, which the
// shell would otherwise read as tab jumps).
func (l *Logs) CapturingKeys() bool { return l.mode != logsInputNone }

// SetSearch applies the shell's "/" search buffer as a text filter (D10); an
// empty string clears it. It composes with every other filter.
func (l *Logs) SetSearch(text string) { l.filters.Text = text }

func (l *Logs) serviceOK() bool { return l.stack != nil && l.stack.Logs != nil }

func (l *Logs) Init() tea.Cmd { return l.snapshotCmd() }

// logsSnapshotMsg carries one initial-load result.
type logsSnapshotMsg struct {
	entries []compass.LogEntry
	cursor  string
	err     string
}

// logsPollMsg carries one live-tail poll's result.
type logsPollMsg struct {
	entries []compass.LogEntry
	cursor  string
	err     string
}

// logsRangeMsg carries one historical time-range query's result.
type logsRangeMsg struct {
	entries []compass.LogEntry
	err     string
}

// snapshotCmd loads the scrollback + tail position in one journal read.
func (l *Logs) snapshotCmd() tea.Cmd {
	if !l.serviceOK() {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		e, cursor, err := l.stack.Logs.Snapshot(ctx, logScrollback)
		if err != nil {
			return logsSnapshotMsg{err: err.Error()}
		}
		return logsSnapshotMsg{entries: e, cursor: cursor}
	}
}

// pollCmd fetches the lines added after the current cursor. Cheap: only new
// entries are read per call.
func (l *Logs) pollCmd() tea.Cmd {
	if !l.serviceOK() || l.view != viewLive {
		return nil
	}
	cursor := l.cursor
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		e, cur, err := l.stack.Logs.Poll(ctx, cursor)
		if err != nil {
			return logsPollMsg{err: err.Error()}
		}
		return logsPollMsg{entries: e, cursor: cur}
	}
}

// rangeCmd fetches the [since, until] window for historical queries.
func (l *Logs) rangeCmd() tea.Cmd {
	if !l.serviceOK() || l.view != viewHistorical {
		return nil
	}
	since, until := l.since, l.until
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		e, err := l.stack.Logs.Range(ctx, since, until)
		if err != nil {
			return logsRangeMsg{err: err.Error()}
		}
		return logsRangeMsg{entries: e}
	}
}

// Update handles async results, the shell's status poll (piggybacked live
// tail), window resizes, and keys.
func (l *Logs) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case logsSnapshotMsg:
		if m.err != "" {
			l.loadErr = m.err
			return l, nil
		}
		l.entries = capEntries(m.entries)
		l.cursor = m.cursor
		l.loaded = true
		l.loadErr = ""
		l.offset = 0 // stick to the newest line on initial load
		return l, nil
	case logsPollMsg:
		if m.err != "" {
			l.pollErr = m.err
			return l, nil
		}
		l.pollErr = ""
		if len(m.entries) > 0 {
			l.entries = capEntries(append(l.entries, m.entries...))
			l.cursor = m.cursor // only advance when entries actually arrived
		}
		return l, nil
	case logsRangeMsg:
		if m.err != "" {
			l.rangeErr = m.err
			return l, nil
		}
		l.hist = capEntries(m.entries)
		l.rangeErr = ""
		l.offset = 0
		return l, nil
	case tea.WindowSizeMsg:
		l.width, l.height = m.Width, m.Height
		return l, nil
	case statusMsg:
		// The shell polls every 2 s; piggyback it so new lines show up
		// without a manual refresh while this tab is active and unpaused.
		if !l.serviceOK() || l.paused || l.view != viewLive {
			return l, nil
		}
		if l.loaded {
			return l, l.pollCmd()
		}
		return l, l.snapshotCmd() // retry a failed initial load
	case tea.KeyMsg:
		if l.mode != logsInputNone {
			return l.updateInputKey(m)
		}
		return l.updateKey(m)
	}
	return l, nil
}

func (l *Logs) updateKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.Type {
	case tea.KeyUp:
		l.scroll(1)
		return l, nil
	case tea.KeyDown:
		l.scroll(-1)
		return l, nil
	case tea.KeyPgUp:
		l.scroll(l.viewHeight())
		return l, nil
	case tea.KeyPgDown:
		l.scroll(-l.viewHeight())
		return l, nil
	}
	switch {
	case isRune(key, 'p'):
		if l.view == viewLive {
			l.paused = !l.paused
		}
	case isRune(key, 'l'):
		l.cycleLevel()
	case isRune(key, 'f'):
		l.beginInput(logsInputPlayer)
	case isRune(key, 'c'):
		l.beginInput(logsInputRange)
	case isRune(key, 't'):
		return l, l.cycleRangePreset()
	case isRune(key, 'u'):
		if !l.serviceOK() {
			return l, nil
		}
		if l.view == viewHistorical {
			l.offset = 0
			return l, l.rangeCmd()
		}
		l.loaded = false // force a fresh snapshot on the next result
		return l, l.snapshotCmd()
	}
	return l, nil
}

// beginInput enters a free-text filter mode with an empty buffer.
func (l *Logs) beginInput(m logsInputMode) {
	if !l.serviceOK() {
		return
	}
	l.mode = m
	l.buf = ""
	l.errIn = ""
}

// updateInputKey handles keys while a filter field is active. Esc cancels;
// Enter applies (an empty player buffer clears the player filter).
func (l *Logs) updateInputKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.Type {
	case tea.KeyEsc:
		l.mode = logsInputNone
		l.buf = ""
		l.errIn = ""
		return l, nil
	case tea.KeyBackspace:
		if len(l.buf) > 0 {
			l.buf = l.buf[:len(l.buf)-1]
		}
		l.errIn = ""
		return l, nil
	case tea.KeyRunes:
		if len(key.Runes) > 0 && printable(key.Runes[0]) {
			l.buf += string(key.Runes[0])
			l.errIn = ""
		}
		return l, nil
	case tea.KeyEnter:
		val := strings.TrimSpace(l.buf)
		switch l.mode {
		case logsInputPlayer:
			l.filters.Player = val // empty clears the filter
			l.mode = logsInputNone
			l.buf = ""
		case logsInputRange:
			since, until, err := parseRangeSpec(val)
			if err != nil {
				l.errIn = err.Error()
				return l, nil
			}
			l.since, l.until = since, until
			l.rangeLabel = val
			l.view = viewHistorical
			l.offset = 0
			l.mode = logsInputNone
			l.buf = ""
			return l, l.rangeCmd()
		}
	}
	return l, nil
}

// cycleLevel walks the level filter: all -> INFO -> WARN -> ERROR -> all.
func (l *Logs) cycleLevel() {
	switch strings.ToUpper(l.filters.Level) {
	case "":
		l.filters.Level = "INFO"
	case "INFO":
		l.filters.Level = "WARN"
	case "WARN":
		l.filters.Level = "ERROR"
	default:
		l.filters.Level = ""
	}
}

// cycleRangePreset walks the time range: all -> last hour -> today -> all.
// Leaving "all" switches to historical view and fetches the window; returning
// to "all" restores the live tail (a fresh snapshot re-anchors it).
func (l *Logs) cycleRangePreset() tea.Cmd {
	switch l.rangeLabel {
	case "", "all":
		l.setPreset("last hour")
		return l.rangeCmd()
	case "last hour":
		l.setPreset("today")
		return l.rangeCmd()
	default:
		l.view = viewLive
		l.rangeLabel = "all"
		l.since, l.until = time.Time{}, time.Time{}
		l.offset = 0
		l.loaded = false
		return l.snapshotCmd()
	}
}

func (l *Logs) setPreset(name string) {
	now := time.Now()
	switch name {
	case "last hour":
		l.since = now.Add(-time.Hour)
		l.until = time.Time{} // open-ended: the window keeps growing
	case "today":
		y, m, d := now.Date()
		l.since = time.Date(y, m, d, 0, 0, 0, 0, now.Location())
		l.until = time.Time{}
	}
	l.rangeLabel = name
	l.view = viewHistorical
	l.offset = 0
}

// parseRangeSpec parses a custom range spec "since[, until]" with second or
// minute precision, local wall clock (the same interpretation journalctl
// applies to --since/--until). An omitted until is open-ended.
func parseRangeSpec(spec string) (time.Time, time.Time, error) {
	if spec == "" {
		return time.Time{}, time.Time{}, errRangeEmpty()
	}
	sinceStr := spec
	untilStr := ""
	if i := strings.Index(spec, ","); i >= 0 {
		sinceStr = spec[:i]
		untilStr = spec[i+1:]
	}
	since, err := parseRangeTime(strings.TrimSpace(sinceStr))
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	var until time.Time
	if strings.TrimSpace(untilStr) != "" {
		until, err = parseRangeTime(strings.TrimSpace(untilStr))
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	return since, until, nil
}

func parseRangeTime(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, &rangeParseError{value: s}
}

type rangeParseError struct{ value string }

func (e *rangeParseError) Error() string {
	return "cannot parse " + e.value + " as YYYY-MM-DD HH:MM[:SS]"
}

func errRangeEmpty() error { return &rangeParseError{value: "(empty)"} }

// scroll moves the viewport by n lines (positive = toward older lines),
// clamped to the displayed set.
func (l *Logs) scroll(n int) {
	max := len(l.displayEntries()) - l.viewHeight()
	if max < 0 {
		max = 0
	}
	l.offset += n
	if l.offset < 0 {
		l.offset = 0
	}
	if l.offset > max {
		l.offset = max
	}
}

// displayEntries returns the set currently shown (live scrollback or the
// historical result), already narrowed by every active filter.
func (l *Logs) displayEntries() []compass.LogEntry {
	var src []compass.LogEntry
	if l.view == viewHistorical {
		src = l.hist
	} else {
		src = l.entries
	}
	return compass.FilterEntries(src, l.filters)
}

// viewHeight is the number of entry lines that fit between the filter bar and
// the hint line. The shell chrome (tab row, divider, status bar) plus this
// screen's own four fixed lines are subtracted from the terminal height.
func (l *Logs) viewHeight() int {
	h := l.height - 7
	if h < 4 {
		h = 4
	}
	return h
}

// View renders the title, the filter bar, the input line (when a field is
// open), the scrollback viewport, and the key hints.
func (l *Logs) View() string {
	lines := []string{titleStyle.Render("Logs"), ""}

	if !l.serviceOK() {
		lines = append(lines, toastErr.Render("  logs unavailable (no journal resolved — tmux deployments have no unit)"))
		lines = append(lines, hintStyle.Render("  u refresh    / search"))
		return join(lines)
	}

	unit := ""
	if l.stack.Logs.Journal != nil {
		unit = " (" + l.stack.Logs.Journal.Unit + ")"
	}
	lines[0] = titleStyle.Render("Logs" + unit)

	// Filter bar: active filters are highlighted, the view mode is named.
	mode := "live"
	if l.view == viewHistorical {
		mode = "range:" + l.rangeLabel
	}
	if l.paused {
		mode += "  [paused]"
	}
	lines = append(lines, filterBar(mode, l.filters))

	switch l.mode {
	case logsInputPlayer:
		lines = append(lines, promptStyle.Render("  player> "+l.buf))
		if l.errIn != "" {
			lines = append(lines, toastErr.Render("  "+l.errIn))
		}
	case logsInputRange:
		lines = append(lines, promptStyle.Render("  range> "+l.buf+"   (since[, until] — YYYY-MM-DD HH:MM[:SS])"))
		if l.errIn != "" {
			lines = append(lines, toastErr.Render("  "+l.errIn))
		}
	}

	if l.loadErr != "" && !l.loaded {
		lines = append(lines, toastErr.Render("  "+l.loadErr), "")
	} else if l.pollErr != "" {
		lines = append(lines, hintStyle.Render("  (last poll failed: "+l.pollErr+")"))
	}

	filtered := l.displayEntries()
	vh := l.viewHeight()
	maxOffset := len(filtered) - vh
	if maxOffset < 0 {
		maxOffset = 0
	}
	if l.offset > maxOffset {
		l.offset = maxOffset
	}
	start := len(filtered) - vh - l.offset
	if start < 0 {
		start = 0
	}
	end := start + vh
	if end > len(filtered) {
		end = len(filtered)
	}
	w := l.width - 2
	if w < 10 {
		w = 10
	}
	if len(filtered) == 0 {
		lines = append(lines, hintStyle.Render("  (no lines match the current filters)"))
	} else {
		for _, e := range filtered[start:end] {
			lines = append(lines, "  "+renderLogLine(e, w))
		}
		if l.offset > 0 {
			lines = append(lines, hintStyle.Render(fmt.Sprintf("  … %d older line(s) — PgUp to scroll", l.offset)))
		}
	}

	lines = append(lines, hintStyle.Render(
		"  ↑/↓ scroll · PgUp/PgDn page · p pause · l level · f player · t range · c custom range · u refresh · / search"))
	return join(lines)
}

// filterBar renders the active filters as one compact line.
func filterBar(mode string, f compass.LogFilters) string {
	parts := []string{mode}
	if f.Level != "" {
		parts = append(parts, promptStyle.Render("level:"+f.Level))
	} else {
		parts = append(parts, "level:all")
	}
	if f.Text != "" {
		parts = append(parts, promptStyle.Render(`search:"`+f.Text+`"`))
	}
	if f.Player != "" {
		parts = append(parts, promptStyle.Render("player:"+f.Player))
	}
	return strings.Join(parts, "  ")
}

// renderLogLine renders one entry: the server's own "[ts LEVEL]" tag (when
// present) is colored by level, the rest stays plain. Untagged lines are
// dimmed — they carry no timestamp to anchor them.
func renderLogLine(e compass.LogEntry, width int) string {
	if e.Level == "" {
		return hintStyle.Render(truncateRunes(e.Text, width))
	}
	tagEnd := strings.Index(e.Text, "] ")
	var tag, rest string
	if tagEnd >= 0 {
		tag = e.Text[:tagEnd+1]
		rest = " " + e.Text[tagEnd+2:] // the "] " separator is re-added: it was consumed as a boundary
	} else {
		tag = e.Level
		rest = " " + e.Text
	}
	style := lvlInfo
	switch strings.ToUpper(e.Level) {
	case "WARN":
		style = lvlWarn
	case "ERROR":
		style = lvlError
	}
	return style.Render(tag) + truncateRunes(rest, width-len(tag))
}

// capEntries trims a scrollback to logScrollback lines (oldest dropped).
func capEntries(e []compass.LogEntry) []compass.LogEntry {
	if len(e) <= logScrollback {
		return e
	}
	out := make([]compass.LogEntry, logScrollback)
	copy(out, e[len(e)-logScrollback:])
	return out
}

// truncateRunes shortens s to at most n runes, appending an ellipsis when it
// actually truncates.
func truncateRunes(s string, n int) string {
	if n <= 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
