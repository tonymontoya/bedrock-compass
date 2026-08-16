package compass

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Event is one audit record. Every mutating action in the tool lands here
// (D11: the audit trail is kept).
type Event struct {
	Time     time.Time         `json:"time"`
	Verb     string            `json:"verb"`
	Params   map[string]string `json:"params,omitempty"`
	Result   string            `json:"result"` // "ok" or "error: <reason>"
	Duration time.Duration     `json:"duration,omitempty"`
}

// Trail is an append-only JSONL audit log.
type Trail struct {
	path string
}

// NewTrail prepares the audit trail at path, creating parent directories
// as needed.
func NewTrail(path string) (*Trail, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create audit dir: %w", err)
	}
	return &Trail{path: path}, nil
}

// Append writes one event. A zero Time is stamped with now (UTC). Each
// append is a single O_APPEND write, so concurrent appenders cannot
// interleave lines.
func (t *Trail) Append(ev Event) error {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal audit event: %w", err)
	}
	f, err := os.OpenFile(t.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open audit trail: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append audit event: %w", err)
	}
	return nil
}

// Recent returns up to n most-recent events, newest first. Malformed lines
// are skipped: the trail is append-only, and a torn tail line must not
// poison reads.
func (t *Trail) Recent(n int) ([]Event, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(t.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open audit trail: %w", err)
	}
	defer f.Close()

	var all []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		all = append(all, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan audit trail: %w", err)
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	out := make([]Event, len(all))
	for i, ev := range all {
		out[len(all)-1-i] = ev
	}
	return out, nil
}
