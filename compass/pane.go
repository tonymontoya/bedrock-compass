package compass

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
)

// Pane reads a tmux pane's content (history + visible lines) via
// `tmux capture-pane`, for servers supervised under tmux (ADR-0001).
type Pane struct {
	Target string // tmux pane target, e.g. "bedrock:0.0"
	Exec   ExecFunc
}

// Baseline captures the pane and returns an opaque token marking its end.
func (p *Pane) Baseline(ctx context.Context) (string, error) {
	lines, err := p.capture(ctx)
	if err != nil {
		return "", fmt.Errorf("tmux baseline for pane %q: %w", p.Target, err)
	}
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(lines, "\n"))), nil
}

// NewSince returns the lines appended to the pane after the baseline.
// A pane that was cleared, reset, or shrunken fails cleanly instead of
// returning a guess.
func (p *Pane) NewSince(ctx context.Context, token string) ([]string, error) {
	baseline, err := decodeToken(token)
	if err != nil {
		return nil, fmt.Errorf("tmux baseline token: %w", err)
	}
	current, err := p.capture(ctx)
	if err != nil {
		return nil, fmt.Errorf("tmux read for pane %q: %w", p.Target, err)
	}
	return tailAfter(baseline, current)
}

func decodeToken(token string) ([]string, error) {
	b, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, nil
	}
	return strings.Split(string(b), "\n"), nil
}

// tailAfter returns the lines appended after the baseline.
func tailAfter(baseline, current []string) ([]string, error) {
	if len(current) < len(baseline) {
		return nil, fmt.Errorf("pane content shrank (%d -> %d lines); baseline invalid", len(baseline), len(current))
	}
	for i, l := range baseline {
		if current[i] != l {
			return nil, fmt.Errorf("pane content changed under the baseline (cleared or reset?); baseline invalid")
		}
	}
	return current[len(baseline):], nil
}

func (p *Pane) capture(ctx context.Context) ([]string, error) {
	exec := p.Exec
	if exec == nil {
		exec = Exec
	}
	out, err := exec(ctx, "tmux", "capture-pane", "-t", p.Target, "-p", "-S", "-")
	if err != nil {
		return nil, err
	}
	text := strings.TrimRight(string(out), "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}
