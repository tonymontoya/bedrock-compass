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
