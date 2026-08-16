package compass

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("module root (go.mod) not found above the compass package")
		}
		dir = parent
	}
}

// TestNoTUIImports guards D15: the domain core must stay TUI-free so a
// future non-TUI consumer (the v2 in-game bridge) can link against it.
func TestNoTUIImports(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	root := findModuleRoot(t)
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "./compass/...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var banned []string
	for _, imp := range strings.Fields(string(out)) {
		if strings.Contains(imp, "bedrock-compass/internal/tui") ||
			strings.HasPrefix(imp, "github.com/charmbracelet/") {
			banned = append(banned, imp)
		}
	}
	if len(banned) > 0 {
		t.Fatalf("domain core imports TUI packages (forbidden by D15): %v", banned)
	}
}
