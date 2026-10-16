package compass

import (
	"testing"
	"time"
)

// feedAndExpect drives a Harvest with a scripted timeline and asserts where it completes.
func feedAndExpect(t *testing.T, h *Harvest, samples []Sample) (doneAt int, errAtDone error) {
	t.Helper()
	for i, s := range samples {
		done, err := h.Feed(s)
		if done {
			return i, err
		}
	}
	return -1, nil
}

func TestHarvest_SettlesAfterQuietPeriod(t *testing.T) {
	h := &Harvest{Quiesce: 500 * time.Millisecond, Timeout: 15 * time.Second}
	base := time.Now()
	samples := []Sample{
		{Count: 2, At: base},
		{Count: 5, At: base.Add(100 * time.Millisecond)},
		{Count: 9, At: base.Add(250 * time.Millisecond)},
		{Count: 9, At: base.Add(700 * time.Millisecond)}, // 450 ms quiet < 500 → still settling
		{Count: 9, At: base.Add(800 * time.Millisecond)}, // 550 ms quiet → settled
	}
	doneAt, err := feedAndExpect(t, h, samples)
	if doneAt != len(samples)-1 {
		t.Fatalf("settled at sample %d, want %d", doneAt, len(samples)-1)
	}
	if err != nil {
		t.Fatalf("settled with error %v, want nil", err)
	}
}

func TestHarvest_LateLineExtendsWindow(t *testing.T) {
	h := &Harvest{Quiesce: 100 * time.Millisecond, Timeout: 10 * time.Second}
	base := time.Now()
	samples := []Sample{
		{Count: 1, At: base.Add(10 * time.Millisecond)},
		{Count: 1, At: base.Add(90 * time.Millisecond)},  // 80 ms quiet < 100 → not done
		{Count: 2, At: base.Add(120 * time.Millisecond)}, // late line extends the window
		{Count: 2, At: base.Add(250 * time.Millisecond)}, // 130 ms quiet → settled
	}
	doneAt, err := feedAndExpect(t, h, samples)
	if doneAt != len(samples)-1 {
		t.Fatalf("settled at sample %d, want %d (late line must extend window)", doneAt, len(samples)-1)
	}
	if err != nil {
		t.Fatalf("settled with error %v, want nil", err)
	}
}

func TestHarvest_TimeoutWinsOverQuiescence(t *testing.T) {
	h := &Harvest{Quiesce: 50 * time.Millisecond, Timeout: 200 * time.Millisecond}
	base := time.Now()
	samples := []Sample{
		{Count: 1, At: base},
		{Count: 1, At: base.Add(300 * time.Millisecond)}, // both quiesced and timed out → timeout reported
	}
	doneAt, err := feedAndExpect(t, h, samples)
	if doneAt != 1 {
		t.Fatalf("done at sample %d, want 1", doneAt)
	}
	if err != ErrTimeout {
		t.Fatalf("err=%v, want ErrTimeout", err)
	}
}

func TestHarvest_TimeoutWithContinuousOutput(t *testing.T) {
	h := &Harvest{Quiesce: 1 * time.Second, Timeout: 300 * time.Millisecond}
	base := time.Now()
	samples := []Sample{
		{Count: 1, At: base},
		{Count: 2, At: base.Add(50 * time.Millisecond)},
		{Count: 3, At: base.Add(100 * time.Millisecond)},
		{Count: 4, At: base.Add(150 * time.Millisecond)},
		{Count: 5, At: base.Add(350 * time.Millisecond)}, // past the cap, never quiesced
	}
	doneAt, err := feedAndExpect(t, h, samples)
	if doneAt != len(samples)-1 {
		t.Fatalf("done at sample %d, want %d", doneAt, len(samples)-1)
	}
	if err != ErrTimeout {
		t.Fatalf("err=%v, want ErrTimeout", err)
	}
}

func TestHarvest_SilentCommandSettles(t *testing.T) {
	h := &Harvest{Quiesce: 50 * time.Millisecond, Timeout: 5 * time.Second}
	base := time.Now()
	samples := []Sample{
		{Count: 0, At: base},
		{Count: 0, At: base.Add(60 * time.Millisecond)}, // quiet from the start → settled
	}
	doneAt, err := feedAndExpect(t, h, samples)
	if doneAt != len(samples)-1 {
		t.Fatalf("done at sample %d, want %d", doneAt, len(samples)-1)
	}
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
}

// For a command whose reply the caller must parse, silence is not an answer: it
// must not settle on zero lines — that is how the `Saving...` line was lost in the
// live incident (B-04), because the first response landed 641 ms after the send and
// the harvest had already settled at 500 ms.
func TestHarvest_RequiredFirstLineIgnoresSilence(t *testing.T) {
	h := &Harvest{Quiesce: 50 * time.Millisecond, Timeout: 200 * time.Millisecond, RequireFirstLine: true}
	base := time.Now()
	samples := []Sample{
		{Count: 0, At: base},
		{Count: 0, At: base.Add(60 * time.Millisecond)},  // quiet, but no line yet → must not settle
		{Count: 0, At: base.Add(120 * time.Millisecond)}, // same
		{Count: 0, At: base.Add(210 * time.Millisecond)}, // hard cap wins instead of a silent settle
	}
	doneAt, err := feedAndExpect(t, h, samples)
	if doneAt != len(samples)-1 {
		t.Fatalf("done at sample %d (err=%v), want %d", doneAt, err, len(samples)-1)
	}
	if err != ErrTimeout {
		t.Fatalf("err=%v, want ErrTimeout", err)
	}
}

func TestHarvest_FirstSampleNeverDone(t *testing.T) {
	h := &Harvest{Quiesce: 1 * time.Millisecond, Timeout: 1 * time.Millisecond}
	done, err := h.Feed(Sample{Count: 99, At: time.Now()})
	if done {
		t.Fatalf("first sample reported done (err=%v); it only establishes the baseline", err)
	}
}
