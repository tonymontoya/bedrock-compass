package compass

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAppend_Recent_Order(t *testing.T) {
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	for i, verb := range []string{"backup", "kick", "restart"} {
		ev := Event{Time: base.Add(time.Duration(i) * time.Minute), Verb: verb, Result: "ok"}
		if verb == "kick" {
			ev.Params = map[string]string{"player": "Tony75264"}
		}
		if err := trail.Append(ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, err := trail.Recent(2)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Recent(2) returned %d events", len(got))
	}
	if got[0].Verb != "restart" || got[1].Verb != "kick" {
		t.Errorf("most-recent-first order broken: %q, %q", got[0].Verb, got[1].Verb)
	}
	if got[1].Params["player"] != "Tony75264" {
		t.Errorf("params lost: %+v", got[1].Params)
	}
}

func TestAppend_ZeroTime_Getstamped(t *testing.T) {
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	before := time.Now().Add(-time.Second)
	if err := trail.Append(Event{Verb: "status", Result: "ok"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := trail.Recent(1)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if got[0].Time.Before(before) || got[0].Time.After(time.Now().Add(time.Second)) {
		t.Errorf("zero Time must be stamped with now, got %v", got[0].Time)
	}
}

func TestNewTrail_CreatesParentDirs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "b", "c", "audit.jsonl")
	if _, err := NewTrail(p); err != nil {
		t.Fatalf("NewTrail must create parent dirs: %v", err)
	}
}

func TestRecent_MalformedLines_Skipped(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(p, []byte("not-json\n{\"verb\":\"kick\",\"result\":\"ok\"}\n{broken\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	trail, err := NewTrail(p)
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	got, err := trail.Recent(10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 || got[0].Verb != "kick" {
		t.Errorf("malformed lines must be skipped, got %+v", got)
	}
}

func TestRecent_EmptyFile(t *testing.T) {
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	got, err := trail.Recent(10)
	if err != nil {
		t.Fatalf("Recent on empty file: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 events, got %+v", got)
	}
}

func TestAppend_Parallel(t *testing.T) {
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	const perGoroutine = 25
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if err := trail.Append(Event{Verb: "parallel", Result: "ok"}); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("parallel Append: %v", e)
	}
	got, err := trail.Recent(1000)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != perGoroutine*2 {
		t.Errorf("want %d events, got %d (lost appends?)", perGoroutine*2, len(got))
	}
}
