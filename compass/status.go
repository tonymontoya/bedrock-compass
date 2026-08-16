package compass

// Status is a snapshot of server and host state for display (D8, D10).
//
// It is the shared contract between the telemetry core (which produces it,
// ticket #4) and the TUI status bar (which renders it, ticket #3). It lives
// in the domain core so the model stays TUI-free (D15) and the future v2
// bridge can consume the same shape.
//
// Zero values mean "unknown / not yet available"; the TUI renders those as
// placeholders.
type Status struct {
	State    string  // "running" or "stopped"; "" = unknown
	Name     string  // server-name (server.properties)
	Version  string  // BDS version (pong)
	MOTD     string  // MOTD line 1 (pong)
	Online   int     // live player count (pong)
	Max      int     // max-players (server.properties / pong)
	CPUPct   float64 // host CPU utilization, 0-100
	RAMUsed  uint64  // host RAM used, bytes
	RAMTotal uint64  // host RAM total, bytes
}
