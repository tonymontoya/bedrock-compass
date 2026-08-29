package tui

import (
	"strings"
	"testing"
	"time"
)

func TestToastStack_Prune(t *testing.T) {
	base := time.Now()
	s := &ToastStack{}
	s.Add(base, "a", ToastInfo, time.Second)
	s.Add(base, "b", ToastError, 2*time.Second)

	s.Prune(base.Add(500 * time.Millisecond))
	if len(s.Items) != 2 {
		t.Fatalf("after 500ms want 2 toasts, got %d", len(s.Items))
	}
	s.Prune(base.Add(1500 * time.Millisecond))
	if len(s.Items) != 1 || s.Items[0].Msg != "b" {
		t.Fatalf("after 1500ms want [b], got %v", s.Items)
	}
}

func TestToastStack_RenderNewestOnTop(t *testing.T) {
	base := time.Now()
	s := &ToastStack{}
	s.Add(base, "first", ToastInfo, time.Hour)
	s.Add(base, "second", ToastSuccess, time.Hour)
	out := s.Render()
	i1, i2 := strings.Index(out, "first"), strings.Index(out, "second")
	if i1 < 0 || i2 < 0 || i2 > i1 {
		t.Fatalf("want newest (second) above first, got:\n%s", out)
	}
}

// TestToastStack_RenderWrapsLongToasts pins ticket #13's review fix: a toast
// longer than the terminal width must wrap at word boundaries instead of
// running off the right edge and being hard-clipped.
func TestToastStack_RenderWrapsLongToasts(t *testing.T) {
	base := time.Now()
	s := &ToastStack{}
	long := "adopt: discover failed: two bedrock_server processes on this host (prod + scratch); stop one or use an explicit config"
	s.Add(base, long, ToastError, time.Hour)

	const w = 40
	out := s.Render(w)
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		t.Fatalf("long toast must wrap to multiple lines at width %d, got one line:\n%s", w, out)
	}
	for i, ln := range lines {
		if l := displayWidth(ln); l > w {
			t.Errorf("line %d is %d columns wide, want <= %d:\n%q", i+1, l, w, ln)
		}
	}
	// Wrapping must not lose text: every word of the original appears.
	for _, word := range strings.Fields(long) {
		if !strings.Contains(out, word) {
			t.Errorf("wrapped output lost word %q:\n%s", word, out)
		}
	}
}

// TestToastStack_RenderShortToastUnchanged pins that a toast within the width
// renders as a single line (no spurious wrapping).
func TestToastStack_RenderShortToastUnchanged(t *testing.T) {
	base := time.Now()
	s := &ToastStack{}
	s.Add(base, "stop: ok", ToastSuccess, time.Hour)

	out := s.Render(80)
	if strings.Contains(out, "\n") {
		t.Errorf("short toast must stay on one line, got:\n%s", out)
	}
	if !strings.Contains(out, "stop: ok") {
		t.Errorf("toast text missing from render:\n%s", out)
	}
}

// displayWidth counts the visible columns of a rendered (styled) line. The
// toast styles are color-only (no borders/padding), so stripping ANSI SGR
// sequences leaves exactly the printed characters — all ASCII in these tests.
func displayWidth(s string) int {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
		case r == 0x1b:
			inEsc = true
		default:
			b.WriteRune(r)
		}
	}
	return len([]rune(b.String()))
}
