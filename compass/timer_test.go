package compass

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recExec is a recording ExecFunc: it returns canned output keyed by the
// first argument (the systemctl subcommand) and records every call.
type recExec struct {
	calls   [][]string // each call as [name, arg0, arg1, ...]
	outputs map[string][]byte
	errFor  map[string]error
}

func newRecExec() *recExec {
	return &recExec{
		outputs: map[string][]byte{},
		errFor:  map[string]error{},
	}
}

func (r *recExec) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	// Key on the subcommand (the first non-flag argument), so
	// `systemctl --user enable x` keys as "enable", not "--user".
	key := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			key = a
			break
		}
	}
	if err, ok := r.errFor[key]; ok {
		return nil, err
	}
	return r.outputs[key], nil
}

func (r *recExec) find(sub string) (int, bool) {
	for i, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), sub) {
			return i, true
		}
	}
	return 0, false
}

func testTimer(t *testing.T, re *recExec) *Timer {
	t.Helper()
	dir := t.TempDir()
	return &Timer{
		ConfigDir:  dir,
		Config:     "/home/tony/.config/bedrock-compass/config.yaml",
		BinaryPath: "/usr/local/bin/bedrock-compass",
		Exec:       re.run,
	}
}

func TestScheduleValidate_Daily(t *testing.T) {
	cases := []struct {
		s    Schedule
		want string // "" = ok
	}{
		{Schedule{Cadence: "daily", Time: "02:30", Mode: "live"}, ""},
		{Schedule{Cadence: "daily", Time: "00:00", Mode: "stopped"}, ""},
		{Schedule{Cadence: "daily", Time: "23:59", Mode: "live"}, ""},
		{Schedule{Cadence: "weekly", Time: "03:15", Weekday: 0, Mode: "live"}, ""},
		{Schedule{Cadence: "weekly", Time: "03:15", Weekday: 6, Mode: "stopped"}, ""},
		{Schedule{Cadence: "monthly", Time: "03:15", Mode: "live"}, "cadence"},
		{Schedule{Cadence: "daily", Time: "24:00", Mode: "live"}, "hour"},
		{Schedule{Cadence: "daily", Time: "02:60", Mode: "live"}, "minute"},
		{Schedule{Cadence: "daily", Time: "02:3", Mode: "live"}, "HH:MM"},
		{Schedule{Cadence: "daily", Time: "2:30", Mode: "live"}, "HH:MM"},
		{Schedule{Cadence: "weekly", Time: "03:15", Weekday: 7, Mode: "live"}, "weekday"},
		{Schedule{Cadence: "daily", Time: "02:30", Mode: "both"}, "mode"},
	}
	for _, c := range cases {
		err := c.s.Validate()
		if c.want == "" {
			if err != nil {
				t.Errorf("%+v: Validate = %v, want ok", c.s, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%+v: Validate = ok, want error mentioning %q", c.s, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: err %q, want it to mention %q", c.s, err, c.want)
		}
	}
}

func TestRenderServiceUnit_Live(t *testing.T) {
	got := renderServiceUnit("/usr/local/bin/bedrock-compass", "/cfg.yaml", "live")
	for _, want := range []string{
		"Type=oneshot",
		"ExecStart=/usr/local/bin/bedrock-compass backup-now --mode live --config /cfg.yaml",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("service unit missing %q:\n%s", want, got)
		}
	}
}

func TestRenderTimerUnit_Daily(t *testing.T) {
	got := renderTimerUnit(Schedule{Cadence: "daily", Time: "02:30", Mode: "live"})
	if !strings.Contains(got, "OnCalendar=*-*-* 02:30:00") {
		t.Errorf("daily timer missing OnCalendar 02:30:\n%s", got)
	}
	if !strings.Contains(got, "Persistent=true") {
		t.Errorf("daily timer missing Persistent=true:\n%s", got)
	}
	if !strings.Contains(got, "WantedBy=timers.target") {
		t.Errorf("daily timer missing WantedBy:\n%s", got)
	}
}

func TestRenderTimerUnit_Weekly(t *testing.T) {
	cases := map[int]string{
		0: "Mon", 1: "Tue", 2: "Wed", 3: "Thu", 4: "Fri", 5: "Sat", 6: "Sun",
	}
	for wd, day := range cases {
		got := renderTimerUnit(Schedule{Cadence: "weekly", Time: "03:15", Weekday: wd, Mode: "live"})
		want := "OnCalendar=" + day + ` *-*-* 03:15:00`
		if !strings.Contains(got, want) {
			t.Errorf("weekly weekday %d: missing %q:\n%s", wd, want, got)
		}
	}
}

func TestTimerCreate_WritesUnitsAndEnables(t *testing.T) {
	re := newRecExec()
	tm := testTimer(t, re)
	re.outputs["is-loaded"] = []byte("loaded\n")
	re.outputs["is-enabled"] = []byte("enabled\n")
	re.outputs["is-active"] = []byte("active\n")
	re.outputs["list-timers"] = []byte("Mon 02:30:00 2h Mon 02:30:00 bedrock-compass-backup.timer - active\n")

	st, err := tm.Create(context.Background(), Schedule{Cadence: "daily", Time: "02:30", Mode: "live"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Unit files written.
	svcPath := filepath.Join(reDir(t, tm), "bedrock-compass-backup.service")
	if _, err := os.Stat(svcPath); err != nil {
		t.Fatalf("service unit not written: %v", err)
	}
	tmPath := filepath.Join(reDir(t, tm), "bedrock-compass-backup.timer")
	if _, err := os.Stat(tmPath); err != nil {
		t.Fatalf("timer unit not written: %v", err)
	}

	// systemctl sequence: daemon-reload -> enable -> start.
	for _, sub := range []string{"daemon-reload", "enable bedrock-compass-backup.timer", "start bedrock-compass-backup.timer"} {
		if _, ok := re.find(sub); !ok {
			t.Errorf("Create did not run %q; calls: %v", sub, re.calls)
		}
	}

	// Status reflects the fake.
	if !st.Loaded || !st.Enabled || !st.Active {
		t.Errorf("status = %+v, want loaded+enabled+active", st)
	}
	if st.NextRun == "" {
		t.Error("NextRun is empty, want the parsed next time")
	}
}

// reDir recovers the timer's config dir (we pass it via testTimer, but the
// test needs the path to stat the files).
func reDir(t *testing.T, tm *Timer) string {
	t.Helper()
	if tm.ConfigDir != "" {
		return tm.ConfigDir
	}
	t.Fatal("timer has no config dir")
	return ""
}

func TestTimerCreate_InvalidSchedule_ErrorsBeforeIO(t *testing.T) {
	re := newRecExec()
	tm := testTimer(t, re)
	if _, err := tm.Create(context.Background(), Schedule{Cadence: "nope", Time: "02:30", Mode: "live"}); err == nil {
		t.Fatal("invalid schedule must error")
	}
	if len(re.calls) != 0 {
		t.Errorf("invalid schedule must not touch systemctl; calls: %v", re.calls)
	}
}

func TestTimerCreate_SystemctlFailure_SurfacesCommand(t *testing.T) {
	re := newRecExec()
	tm := testTimer(t, re)
	re.errFor["enable"] = errors.New("Access denied")
	_, err := tm.Create(context.Background(), Schedule{Cadence: "daily", Time: "02:30", Mode: "live"})
	if err == nil {
		t.Fatal("expected error on enable failure")
	}
	if !strings.Contains(err.Error(), "enable") || !strings.Contains(err.Error(), "Access denied") {
		t.Errorf("error should surface the failing command + cause, got: %v", err)
	}
}

func TestTimerStatus_NotLoaded(t *testing.T) {
	re := newRecExec()
	tm := testTimer(t, re)
	re.outputs["is-loaded"] = []byte("not-loaded\n")
	st, err := tm.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Loaded || st.Enabled || st.Active {
		t.Errorf("not-loaded status = %+v, want all false", st)
	}
}

func TestTimerDisable_RecordsStopAndDisable(t *testing.T) {
	re := newRecExec()
	tm := testTimer(t, re)
	if err := tm.Disable(context.Background()); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	for _, sub := range []string{"stop bedrock-compass-backup.timer", "disable bedrock-compass-backup.timer"} {
		if _, ok := re.find(sub); !ok {
			t.Errorf("Disable did not run %q; calls: %v", sub, re.calls)
		}
	}
}

func TestTimerRemove_DisablesThenDeletes(t *testing.T) {
	re := newRecExec()
	tm := testTimer(t, re)
	if err := tm.Remove(context.Background()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	svcPath := filepath.Join(reDir(t, tm), "bedrock-compass-backup.service")
	tmPath := filepath.Join(reDir(t, tm), "bedrock-compass-backup.timer")
	for _, p := range []string{svcPath, tmPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("Remove should delete %s, stat = %v", p, err)
		}
	}
}
