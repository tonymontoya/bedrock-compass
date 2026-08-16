package tui

import (
	"strings"
	"time"
)

// ToastKind classifies a toast so the shell can color it (D10).
type ToastKind int

const (
	ToastInfo ToastKind = iota
	ToastSuccess
	ToastError
)

// Toast is one transient notification; it is visible until Expires.
type Toast struct {
	Msg     string
	Kind    ToastKind
	Expires time.Time
}

// ToastStack holds the toasts currently visible, in arrival order.
type ToastStack struct {
	Items []Toast
}

// Add appends a toast that expires ttl after now.
func (s *ToastStack) Add(now time.Time, msg string, kind ToastKind, ttl time.Duration) {
	s.Items = append(s.Items, Toast{Msg: msg, Kind: kind, Expires: now.Add(ttl)})
}

// Prune drops toasts that have expired at or before now.
func (s *ToastStack) Prune(now time.Time) {
	kept := s.Items[:0]
	for _, t := range s.Items {
		if t.Expires.After(now) {
			kept = append(kept, t)
		}
	}
	s.Items = kept
}

// Active reports whether any toast is currently visible.
func (s *ToastStack) Active() bool { return len(s.Items) > 0 }

// Render lays out the visible toasts, newest on top.
func (s *ToastStack) Render() string {
	if len(s.Items) == 0 {
		return ""
	}
	var lines []string
	for i := len(s.Items) - 1; i >= 0; i-- {
		t := s.Items[i]
		var st = toastInfo
		switch t.Kind {
		case ToastSuccess:
			st = toastOK
		case ToastError:
			st = toastErr
		}
		lines = append(lines, st.Render(strings.TrimSpace(t.Msg)))
	}
	return strings.Join(lines, "\n")
}
