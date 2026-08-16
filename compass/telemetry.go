package compass

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// cpuTimes is one aggregate /proc/stat "cpu" sample.
type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func (c cpuTimes) total() uint64 {
	return c.user + c.nice + c.system + c.idle + c.iowait + c.irq + c.softirq + c.steal
}

func (c cpuTimes) idleAll() uint64 { return c.idle + c.iowait }

// memInfo is the /proc/meminfo values we need (bytes).
type memInfo struct {
	total, available uint64
}

func (m memInfo) used() uint64 {
	if m.total <= m.available {
		return 0
	}
	return m.total - m.available
}

// diskUsage is a statfs result (bytes).
type diskUsage struct{ used, total uint64 }

// netCounters is the summed /proc/net/dev byte counters (bytes).
type netCounters struct{ rx, tx uint64 }

// rawSample is one snapshot of all raw counters the Telemetry consumes.
type rawSample struct {
	cpu  cpuTimes
	mem  memInfo
	disk diskUsage
	net  netCounters
	rss  uint64
	pid  int
	pong *Pong // nil when the server is not reachable / running
}

// prevSample holds the previous cpu/net counters and timestamp for deltas.
type prevSample struct {
	cpu cpuTimes
	net netCounters
	at  time.Time
}

// Telemetry samples host + server telemetry on a fixed tick and exposes the
// latest Status snapshot for the TUI to poll (D8). It is safe for concurrent
// use. The sampling goroutine exits when its context is cancelled, so there
// are no goroutine leaks.
type Telemetry struct {
	interval time.Duration
	now      func() time.Time

	// Injectable sources (tests supply fakes; production reads /proc + pings).
	serverPID func(ctx context.Context) int
	ping      func(ctx context.Context) (Pong, error)
	readCPU   func() (cpuTimes, error)
	readMem   func() (memInfo, error)
	readDisk  func() (diskUsage, error)
	readNet   func() (netCounters, error)
	readRSS   func(pid int) (uint64, error)

	mu   sync.RWMutex
	prev prevSample
	last Status
	done chan struct{}
}

// NewTelemetry wires the production sources: the real /proc readers, a disk
// probe on the server dir (or /), and a pong probe on 127.0.0.1:port. The
// server process is autodiscovered best-effort; absence degrades the status
// to "stopped" rather than failing.
func NewTelemetry(ctx context.Context, cfg *Config, interval time.Duration) (*Telemetry, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if cfg == nil {
		cfg = defaults()
	}
	port := cfg.Port
	if port <= 0 {
		port = 19132
	}
	// Best-effort server process (the telemetry must stay useful even when a
	// D14 scratch instance is running alongside the main server, so we do not
	// hard-fail on multiple bedrock_server processes).
	pid := resolveServerPID(ctx, cfg)
	diskPath := cfg.ServerDir
	if pid > 0 {
		if cwd := procCwd(pid); cwd != "" {
			diskPath = cwd
		}
	}
	if diskPath == "" {
		diskPath = "/"
	}
	return &Telemetry{
		interval:  interval,
		serverPID: func(context.Context) int { return pid },
		ping:      NewPinger("127.0.0.1:" + strconv.Itoa(port)).Ping,
		readCPU:   readCPUStat,
		readMem:   readMemInfo,
		readDisk:  func() (diskUsage, error) { return readDiskUsage(diskPath) },
		readNet:   readNetDev,
		readRSS:   readRSS,
	}, nil
}

// Start launches the sampling goroutine: an immediate first sample, then one
// per interval until ctx is cancelled. It returns once the goroutine is up.
func (t *Telemetry) Start(ctx context.Context) {
	t.done = make(chan struct{})
	go t.loop(ctx)
}

func (t *Telemetry) loop(ctx context.Context) {
	defer close(t.done)
	t.tick(ctx)
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.tick(ctx)
		}
	}
}

// Wait returns a channel closed once the sampling goroutine has exited. Call
// after cancelling the context passed to Start.
func (t *Telemetry) Wait() <-chan struct{} { return t.done }

// Latest returns the most recent snapshot.
func (t *Telemetry) Latest() Status {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.last
}

func (t *Telemetry) tick(ctx context.Context) {
	raw := rawSample{}
	if t.readCPU != nil {
		raw.cpu, _ = t.readCPU()
	}
	if t.readMem != nil {
		raw.mem, _ = t.readMem()
	}
	if t.readDisk != nil {
		raw.disk, _ = t.readDisk()
	}
	if t.readNet != nil {
		raw.net, _ = t.readNet()
	}
	if t.serverPID != nil {
		raw.pid = t.serverPID(ctx)
	}
	if raw.pid > 0 && t.readRSS != nil {
		raw.rss, _ = t.readRSS(raw.pid)
	}
	if t.ping != nil {
		if p, err := t.ping(ctx); err == nil {
			raw.pong = &p
		}
	}
	now := time.Now()
	if t.now != nil {
		now = t.now()
	}
	st := assemble(t.prev, raw, now)
	t.mu.Lock()
	t.prev = prevSample{cpu: raw.cpu, net: raw.net, at: now}
	t.last = st
	t.mu.Unlock()
}

// assemble computes the Status from the raw sample, using the previous sample
// for rate metrics (CPU, net). Pure and deterministic given its inputs.
func assemble(prev prevSample, raw rawSample, now time.Time) Status {
	st := Status{Sampled: now, PID: raw.pid, RSS: raw.rss}
	if raw.pong != nil {
		st.State = "running"
		st.Name = raw.pong.Name
		st.Version = raw.pong.Version
		st.MOTD = raw.pong.MOTD
		st.GUID = raw.pong.GUID
		st.Online = raw.pong.Online
		st.Max = raw.pong.MaxPlayers
	} else if raw.pid > 0 {
		st.State = "running" // process present; pong failed this tick
	} else {
		st.State = "stopped"
	}
	st.Stale = st.State != "running"

	if !prev.at.IsZero() {
		st.CPUPct = cpuPercent(prev.cpu, raw.cpu)
		st.NetUp, st.NetDown = netRate(prev.net, raw.net, now.Sub(prev.at))
	}
	st.RAMUsed, st.RAMTotal = raw.mem.used(), raw.mem.total
	st.DiskUsed, st.DiskTotal = raw.disk.used, raw.disk.total
	return st
}

// cpuPercent returns 0-100 host CPU utilization from two /proc/stat samples.
func cpuPercent(prev, cur cpuTimes) float64 {
	dTotal := int64(cur.total()) - int64(prev.total())
	if dTotal <= 0 {
		return 0
	}
	dIdle := int64(cur.idleAll()) - int64(prev.idleAll())
	pct := 100.0 * float64(dTotal-dIdle) / float64(dTotal)
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

// netRate returns (up, down) bytes/sec between two samples over dt.
func netRate(prev, cur netCounters, dt time.Duration) (up, down uint64) {
	if dt <= 0 {
		return 0, 0
	}
	sec := dt.Seconds()
	if sec <= 0 {
		return 0, 0
	}
	dtx := int64(cur.tx) - int64(prev.tx)
	if dtx < 0 {
		dtx = 0
	}
	drx := int64(cur.rx) - int64(prev.rx)
	if drx < 0 {
		drx = 0
	}
	return uint64(float64(dtx) / sec), uint64(float64(drx) / sec)
}

// pickServerPID chooses a single bedrock_server pid from the process list:
// the lone instance, else the oldest (lowest pid) when several are up. 0 when
// none is running. Pure so it can be tested offline.
func pickServerPID(procs []Proc) int {
	var servers []Proc
	for _, p := range procs {
		if p.Name == serverProcName {
			servers = append(servers, p)
		}
	}
	if len(servers) == 0 {
		return 0
	}
	best := servers[0]
	for _, s := range servers[1:] {
		if s.PID < best.PID {
			best = s
		}
	}
	return best.PID
}

// unitMainPID returns the MainPID of a systemd unit (0 when unavailable).
func unitMainPID(unit string) int {
	out, err := exec.Command("systemctl", "show", unit, "-p", "MainPID", "--value").Output()
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return pid
}

// procPPID returns the parent pid from /proc/<pid>/stat (0 when unreadable).
func procPPID(pid int) int {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	// comm (field 2) may contain spaces or parens; skip past the last ')'.
	s := string(data)
	idx := strings.LastIndexByte(s, ')')
	if idx < 0 {
		return 0
	}
	fields := strings.Fields(s[idx+1:])
	if len(fields) < 2 { // fields[0]=state(3), fields[1]=ppid(4)
		return 0
	}
	ppid, _ := strconv.Atoi(fields[1])
	return ppid
}

// resolveServerPID returns a best-effort bedrock_server pid for telemetry:
// the instance owned by the configured service unit when set, else the lone
// running instance, else the oldest when several are up. 0 when none runs.
func resolveServerPID(ctx context.Context, cfg *Config) int {
	procs, err := procsFromProc(ctx)
	if err != nil {
		return 0
	}
	if cfg != nil && cfg.Service != "" {
		if mp := unitMainPID(cfg.Service); mp > 0 {
			for _, s := range procs {
				if s.Name == serverProcName && procPPID(s.PID) == mp {
					return s.PID
				}
			}
		}
	}
	return pickServerPID(procs)
}

// readCPUStat reads the aggregate "cpu" line of /proc/stat.
func readCPUStat() (cpuTimes, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		f := strings.Fields(line)[1:]
		if len(f) < 4 {
			return cpuTimes{}, errors.New("short cpu line in /proc/stat")
		}
		var c cpuTimes
		for i, v := range f {
			n, _ := strconv.ParseUint(v, 10, 64)
			switch i {
			case 0:
				c.user = n
			case 1:
				c.nice = n
			case 2:
				c.system = n
			case 3:
				c.idle = n
			case 4:
				c.iowait = n
			case 5:
				c.irq = n
			case 6:
				c.softirq = n
			case 7:
				c.steal = n
			}
		}
		return c, nil
	}
	return cpuTimes{}, errors.New("cpu line not found in /proc/stat")
}

// readMemInfo reads MemTotal/MemAvailable from /proc/meminfo.
func readMemInfo() (memInfo, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return memInfo{}, err
	}
	var mi memInfo
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			mi.total, _ = strconv.ParseUint(f[1], 10, 64)
		case "MemAvailable:":
			mi.available, _ = strconv.ParseUint(f[1], 10, 64)
		}
	}
	mi.total *= 1024
	mi.available *= 1024
	return mi, nil
}

// readDiskUsage statfs's path (bytes).
func readDiskUsage(path string) (diskUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return diskUsage{}, err
	}
	bsize := uint64(st.Bsize)
	return diskUsage{
		total: st.Blocks * bsize,
		used:  (st.Blocks - st.Bfree) * bsize,
	}, nil
}

// readNetDev sums the byte counters across all interfaces in /proc/net/dev.
func readNetDev() (netCounters, error) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return netCounters{}, err
	}
	var nc netCounters
	lines := strings.Split(string(data), "\n")
	for i := 2; i < len(lines); i++ { // skip the two header lines
		colon := strings.IndexByte(lines[i], ':')
		if colon < 0 {
			continue
		}
		f := strings.Fields(lines[i][colon+1:])
		if len(f) < 9 {
			continue
		}
		if rx, err := strconv.ParseUint(f[0], 10, 64); err == nil {
			nc.rx += rx
		}
		if tx, err := strconv.ParseUint(f[8], 10, 64); err == nil {
			nc.tx += tx
		}
	}
	return nc, nil
}

// readRSS returns the server process's resident memory (VmRSS, bytes).
func readRSS(pid int) (uint64, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				if kb, err := strconv.ParseUint(f[1], 10, 64); err == nil {
					return kb * 1024, nil
				}
			}
		}
	}
	return 0, errors.New("VmRSS not found in /proc/<pid>/status")
}
