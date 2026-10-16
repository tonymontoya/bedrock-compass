//go:build integration

package compass

import (
	"context"
	"strings"
	"testing"
	"time"
)

// These tests exercise the Logs service (#8) against the scratch rig's real
// journal (unit bedrock-compass-test on the test host). They self-skip when
// the rig is not active and are opt-in: built with -tags integration and
// shipped to the rig host (make integ RIG_HOST=<rig host>).

func TestIntegration_Logs_SnapshotAndLiveTail(t *testing.T) {
	skipIfRigDown(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	logs := &Logs{Journal: &Journal{Unit: rigUnit()}}

	// Initial snapshot: a bounded tail plus the cursor after it.
	entries, cursor, err := logs.Snapshot(ctx, 50)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("snapshot returned no entries; the rig's journal should have lines")
	}
	if cursor == "" {
		t.Fatal("snapshot must return a cursor for live-tail polling")
	}
	// Journal order: tagged timestamps are non-decreasing (untagged systemd
	// messages carry no server timestamp and are skipped).
	for i := 1; i < len(entries); i++ {
		if entries[i].Time.IsZero() || entries[i-1].Time.IsZero() {
			continue
		}
		if entries[i].Time.Before(entries[i-1].Time) {
			t.Fatalf("entries not in journal order at %d: %v < %v", i, entries[i].Time, entries[i-1].Time)
		}
	}

	// Live tail: a console command produces journal output; polling from the
	// snapshot cursor must pick it up.
	c := rigConsole(t)
	out, err := c.Send(ctx, "list")
	if err != nil {
		t.Fatalf("Send(list): %v\n%s", err, out)
	}

	var got []LogEntry
	for i := 0; i < 20; i++ { // up to ~10 s for the journal to catch up
		pctx, pcancel := context.WithTimeout(context.Background(), 15*time.Second)
		got, _, err = logs.Poll(pctx, cursor)
		pcancel()
		if err != nil {
			t.Fatalf("Poll after list: %v", err)
		}
		for _, e := range got {
			if strings.Contains(e.Text, "players online") {
				return // the response is in the journal and was tailed
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("live tail never picked up the list response; last poll had %d entries:\n%s", len(got), joinEntries(got))
}

func TestIntegration_Logs_Range(t *testing.T) {
	skipIfRigDown(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	logs := &Logs{Journal: &Journal{Unit: rigUnit()}}
	now := time.Now()

	entries, err := logs.Range(ctx, now.Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("Range(last hour): %v", err)
	}
	tagged := 0
	for _, e := range entries {
		if e.Time.IsZero() {
			continue // systemd's own messages carry no server timestamp
		}
		tagged++
		if e.Time.Before(now.Add(-time.Hour)) || e.Time.After(now) {
			t.Fatalf("entry outside the last-hour window: %q at %v", e.Text, e.Time)
		}
	}
	if tagged == 0 {
		t.Fatal("last-hour range had no server-tagged entries; the rig has been active")
	}

	// A window a year in the past must be empty.
	old := now.AddDate(0, 0, -365)
	past, err := logs.Range(ctx, old, old.Add(time.Hour))
	if err != nil {
		t.Fatalf("Range(past): %v", err)
	}
	if len(past) != 0 {
		t.Fatalf("a year-old window should be empty; got %d entries", len(past))
	}
}

func joinEntries(e []LogEntry) string {
	var b strings.Builder
	for _, x := range e {
		b.WriteString(x.Text)
		b.WriteByte('\n')
	}
	return b.String()
}
