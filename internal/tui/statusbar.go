package tui

import (
	"fmt"
	"strings"

	"github.com/tonymontoya/bedrock-compass/compass"
)

// renderStatus builds the persistent status bar (D10) from a Status snapshot.
// Zero/empty fields render as "—" placeholders until the telemetry core
// (ticket #4) fills them in.
func renderStatus(s compass.Status, width int) string {
	fields := []string{
		stateField(s.State),
		orDash(s.Name),
		orDash(s.Version),
		playersField(s),
		"cpu " + cpuField(s),
		"ram " + ramField(s),
	}
	return barStyle.Width(width).Render(strings.Join(fields, "  "))
}

func stateField(state string) string {
	switch state {
	case "running":
		return stateRun.Render("● running")
	case "stopped":
		return stateStop.Render("● stopped")
	default:
		return stateUnknown.Render("● unknown")
	}
}

func playersField(s compass.Status) string {
	if s.Max > 0 {
		return fmt.Sprintf("%d/%d players", s.Online, s.Max)
	}
	if s.Online > 0 {
		return fmt.Sprintf("%d players", s.Online)
	}
	return "— players"
}

func cpuField(s compass.Status) string {
	if s.CPUPct > 0 {
		return fmt.Sprintf("%.0f%%", s.CPUPct)
	}
	return "—"
}

func ramField(s compass.Status) string {
	if s.RAMTotal > 0 {
		return fmt.Sprintf("%s/%s", humanBytes(s.RAMUsed), humanBytes(s.RAMTotal))
	}
	return "—"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// humanBytes renders a byte count in binary units (KiB/MiB/…).
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	if exp >= len(units) {
		return fmt.Sprintf("%d B", b)
	}
	return fmt.Sprintf("%.1f %s", float64(b)/float64(div), units[exp])
}
