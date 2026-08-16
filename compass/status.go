package compass

import "time"

// Status is a snapshot of server and host state for display (D8, D10).
//
// It is the shared contract between the telemetry core (which produces it,
// ticket #4) and the TUI (which renders it: status bar in #3, dashboard in
// #5). It lives in the domain core so the model stays TUI-free (D15) and the
// future v2 bridge can consume the same shape.
//
// Zero values mean "unknown / not yet available"; the TUI renders those as
// placeholders. Stale marks server-specific data that is no longer live
// (the server is stopped), while host metrics stay current.
type Status struct {
	// Server (from the pong advertisement + process table).
	State   string        // "running" | "stopped" | "" (unknown)
	Name    string        // server-name (pong / server.properties)
	Version string        // BDS version (pong)
	MOTD    string        // MOTD line 1 (pong)
	GUID    string        // 64-bit server GUID (pong)
	Online  int           // live player count (pong)
	Max     int           // max-players (pong / server.properties)
	Uptime  time.Duration // server uptime (0 = unknown)
	PID     int           // server process pid (0 = not running)

	// Host telemetry (sampled from /proc + statfs).
	CPUPct    float64 // host CPU utilization, 0-100
	RAMUsed   uint64  // host RAM used, bytes
	RAMTotal  uint64  // host RAM total, bytes
	DiskUsed  uint64  // server-partition disk used, bytes
	DiskTotal uint64  // server-partition disk total, bytes
	NetUp     uint64  // network transmit rate, bytes/sec
	NetDown   uint64  // network receive rate, bytes/sec
	RSS       uint64  // server-process resident memory, bytes

	// Freshness.
	Stale   bool      // true when the server is stopped (server data not live)
	Sampled time.Time // when this snapshot was taken
}
