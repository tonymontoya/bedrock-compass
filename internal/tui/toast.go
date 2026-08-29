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

// Render lays out the visible toasts, newest on top. width (default 80) is
// the available terminal columns: a toast longer than it wraps at word
// boundaries instead of running off the right edge (ticket #13 review —
// long error toasts were hard-clipped by the terminal).
func (s *ToastStack) Render(width ...int) string {
	w := 80
	if len(width) > 0 && width[0] > 0 {
		w = width[0]
	}
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
		for _, ln := range wordWrap(strings.TrimSpace(t.Msg), w) {
			lines = append(lines, st.Render(ln))
		}
	}
	return strings.Join(lines, "\n")
}

// wordWrap breaks s into lines of at most width columns, preferring word
// boundaries. A single token longer than the width is hard-split so no line
// can overflow (the terminal would clip it anyway).
func wordWrap(s string, width int) []string {
	if width <= 0 || len(s) <= width {
		return []string{s}
	}
	var lines []string
	for len(s) > width {
		cut := strings.LastIndexByte(s[:width], ' ')
		if cut <= 0 {
			cut = width // no word boundary in range: hard split
		}
		lines = append(lines, s[:cut])
		s = strings.TrimLeft(s[cut:], " ")
	}
	return append(lines, s)
}
