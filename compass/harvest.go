package compass

import (
	"errors"
	"time"
)

// ErrTimeout is reported by Harvest when the hard timeout elapses before
// output settles. The command may still be running; whatever output was
// captured is still returned to the caller.
var ErrTimeout = errors.New("console output did not settle within timeout")

// Sample is one observation of a command's output stream during harvesting.
type Sample struct {
	Count int       // total lines seen since the send baseline
	At    time.Time // when the observation was taken
}

// Harvest is the completion policy for a console command. The Bedrock
// console has no prompt (verified live on 1.26.43; research/01), so a
// command is complete when its output goes quiet for Quiesce, hard-capped
// by Timeout.
type Harvest struct {
	Quiesce time.Duration // quiet span that settles output (e.g. 500 ms)
	Timeout time.Duration // hard cap from the first sample (e.g. 15 s)

	start      time.Time
	lastChange time.Time
	lastCount  int
	primed     bool
}

// Feed consumes one sample and reports whether harvesting may stop.
// done with a nil error means the output settled; done with ErrTimeout
// means the hard cap was hit (output may be incomplete).
func (h *Harvest) Feed(s Sample) (bool, error) {
	if !h.primed {
		h.primed = true
		h.start = s.At
		h.lastChange = s.At
		h.lastCount = s.Count
		return false, nil
	}
	if s.At.Sub(h.start) >= h.Timeout {
		return true, ErrTimeout
	}
	if s.Count > h.lastCount {
		h.lastCount = s.Count
		h.lastChange = s.At
		return false, nil
	}
	if s.At.Sub(h.lastChange) >= h.Quiesce {
		return true, nil
	}
	return false, nil
}
