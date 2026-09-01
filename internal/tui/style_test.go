package tui

import (
	"os"
	"path/filepath"
	"testing"
)

// isEmoji reports whether r is a pictographic emoji — the class of characters
// the house rule "no emoji in code" forbids as UI text. Deliberate TUI design
// glyphs are NOT emoji and are allowed: the navigation arrows (U+2191/2193),
// the ▸ cursor (U+25B8), the ❯ prompt (U+276F), and the █ block cursor (U+2588)
// are geometric shapes and dingbats, not emoji.
func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF: // pictographs, emoticons, transport, symbols
		return true
	case r >= 0x2600 && r <= 0x26FF: // misc symbols (includes U+26A0 warning sign)
		return true
	case r >= 0xFE00 && r <= 0xFE0F: // variation selectors (emoji presentation)
		return true
	}
	return false
}

// TestNoEmojiInSource enforces the house rule "no emoji in code" across the
// whole tui package, so a stray pictographic character cannot slip into any
// rendered line. This is the regression guard for the UI/UX "no emoji" bar.
func TestNoEmojiInSource(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var checked int
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".go" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		checked++
		for _, r := range string(data) {
			if isEmoji(r) {
				t.Errorf("%s contains emoji U+%04X (house rule: no emoji in code)", e.Name(), r)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no .go files checked — is this running in the tui package dir?")
	}
}
