package compass

import (
	"context"
	"testing"
	"time"
)

func TestCPUPercent(t *testing.T) {
	prev := cpuTimes{user: 100, idle: 900}
	cur := cpuTimes{user: 200, idle: 1800}
	if got := cpuPercent(prev, cur); got != 10.0 {
		t.Errorf("cpuPercent = %v, want 10", got)
	}
	if got := cpuPercent(cur, cur); got != 0 {
		t.Errorf("no delta should be 0, got %v", got)
	}
}

func TestNetRate(t *testing.T) {
	prev := netCounters{rx: 1000, tx: 2000}
	cur := netCounters{rx: 3000, tx: 8000}
	up, down := netRate(prev, cur, 2*time.Second)
	if up != 3000 || down != 1000 {
		t.Errorf("up=%d down=%d, want 3000/1000", up, down)
	}
	if up, down := netRate(prev, cur, 0); up != 0 || down != 0 {
		t.Error("zero duration should give 0/0")
	}
}

// newFakeTelemetry builds a Telemetry with fixed fake sources and a
// deterministic clock.
func newFakeTelemetry() (*Telemetry, time.Time) {
	now := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	return &Telemetry{
		interval:  time.Second,
		now:       func() time.Time { return now },
		readCPU:   func() (cpuTimes, error) { return cpuTimes{user: 100, idle: 900}, nil },
		readMem:   func() (memInfo, error) { return memInfo{total: 16 << 20, available: 4 << 20}, nil },
		readDisk:  func() (diskUsage, error) { return diskUsage{used: 1 << 30, total: 10 << 30}, nil },
		readNet:   func() (netCounters, error) { return netCounters{rx: 1000, tx: 2000}, nil },
		serverPID: func(context.Context) int { return 200 },
		readRSS:   func(pid int) (uint64, error) { return 10 << 20, nil },
		ping: func(ctx context.Context) (Pong, error) {
			p, _ := ParsePong(samplePong)
			return p, nil
		},
	}, now
}

func TestTelemetry_FirstTickBaseline(t *testing.T) {
	tel, _ := newFakeTelemetry()
	tel.tick(context.Background())
	st := tel.Latest()
	if st.State != "running" {
		t.Errorf("state = %q, want running", st.State)
	}
	if st.Stale {
		t.Error("running server must not be stale")
	}
	if st.Name != "TestServer" || st.Max != 15 || st.Online != 0 {
		t.Errorf("server fields = %+v", st)
	}
	if st.CPUPct != 0 {
		t.Errorf("first tick has no baseline; cpu = %v, want 0", st.CPUPct)
	}
	if st.RAMUsed != 12<<20 || st.RAMTotal != 16<<20 {
		t.Errorf("ram = %d/%d", st.RAMUsed, st.RAMTotal)
	}
	if st.DiskUsed != 1<<30 || st.DiskTotal != 10<<30 {
		t.Errorf("disk = %d/%d", st.DiskUsed, st.DiskTotal)
	}
	if st.RSS != 10<<20 || st.PID != 200 {
		t.Errorf("rss/pid = %d/%d", st.RSS, st.PID)
	}
}

func TestTelemetry_SecondTickRates(t *testing.T) {
	// Verify the rate math on a known baseline: 2 s apart, counters moved.
	prev := prevSample{cpu: cpuTimes{user: 100, idle: 900}, net: netCounters{rx: 1000, tx: 2000}, at: time.Unix(0, 0).UTC()}
	cur := rawSample{cpu: cpuTimes{user: 200, idle: 1800}, net: netCounters{rx: 3000, tx: 8000}}
	now := prev.at.Add(2 * time.Second)
	st := assemble(prev, cur, now)
	if st.CPUPct != 10.0 {
		t.Errorf("cpu = %v, want 10", st.CPUPct)
	}
	if st.NetUp != 3000 || st.NetDown != 1000 {
		t.Errorf("net = %d/%d, want 3000/1000", st.NetUp, st.NetDown)
	}
}

func TestTelemetry_ServerStoppedDegrades(t *testing.T) {
	tel := &Telemetry{
		now:       func() time.Time { return time.Now() },
		readMem:   func() (memInfo, error) { return memInfo{total: 16 << 20, available: 4 << 20}, nil },
		serverPID: func(context.Context) int { return 0 },
	}
	tel.tick(context.Background())
	st := tel.Latest()
	if st.State != "stopped" {
		t.Errorf("state = %q, want stopped", st.State)
	}
	if !st.Stale {
		t.Error("stopped server must be marked stale")
	}
	if st.PID != 0 {
		t.Errorf("pid = %d, want 0", st.PID)
	}
	if st.RAMTotal != 16<<20 {
		t.Error("host metrics must still be populated when the server is stopped")
	}
}

func TestTelemetry_CleanShutdown(t *testing.T) {
	tel := &Telemetry{
		interval: 10 * time.Millisecond,
		readCPU:  func() (cpuTimes, error) { return cpuTimes{}, nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	tel.Start(ctx)
	time.Sleep(30 * time.Millisecond) // let it tick a couple of times
	cancel()
	select {
	case <-tel.Wait():
		// goroutine exited: no leak
	case <-time.After(2 * time.Second):
		t.Fatal("sampling goroutine did not exit after cancel (leak)")
	}
}

func TestPickServerPID_Single(t *testing.T) {
	procs := []Proc{{PID: 100, Name: "systemd"}, {PID: 200, Name: "bedrock_server", Cwd: "/srv"}}
	if got := pickServerPID(procs); got != 200 {
		t.Errorf("pickServerPID = %d, want 200", got)
	}
}

func TestPickServerPID_MultiplePicksOldest(t *testing.T) {
	// Main server (older, lower pid) plus a D14 scratch instance.
	procs := []Proc{
		{PID: 74883, Name: "bedrock_server", Cwd: "/srv/test"},
		{PID: 72833, Name: "bedrock_server", Cwd: "/srv/main"},
	}
	if got := pickServerPID(procs); got != 72833 {
		t.Errorf("pickServerPID = %d, want the oldest 72833", got)
	}
}

func TestPickServerPID_None(t *testing.T) {
	if got := pickServerPID([]Proc{{PID: 1, Name: "init"}}); got != 0 {
		t.Errorf("no server should give 0, got %d", got)
	}
}
