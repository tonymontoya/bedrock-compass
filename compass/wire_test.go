package compass

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func stackCfg(t *testing.T) *Config {
	t.Helper()
	return &Config{
		ServerDir:    t.TempDir(),
		BackupDir:    filepath.Join(t.TempDir(), "backups"),
		AuditFile:    filepath.Join(t.TempDir(), "audit.jsonl"),
		Supervisor:   "systemd",
		Service:      "minecraft-bedrock.service",
		Retention:    7,
		IncludePacks: false,
	}
}

func TestNewStack_SystemdWiresEverything(t *testing.T) {
	st, err := NewStack(context.Background(), stackCfg(t), "1.26.430", StackOpts{
		ConfigPath: "/home/tony/.config/bedrock-compass/config.yaml",
		BinaryPath: "/usr/local/bin/bedrock-compass",
	})
	if err != nil {
		t.Fatalf("NewStack: %v", err)
	}
	if st.Engine == nil {
		t.Error("Engine is nil")
	}
	if st.Console == nil {
		t.Error("Console is nil")
	}
	if st.Sup == nil {
		t.Error("Sup is nil, want a systemd supervisor")
	}
	if st.Timer == nil {
		t.Error("Timer is nil")
	}
	if st.Timer.Unit != DefaultTimerName {
		t.Errorf("Timer.Unit = %q, want %q", st.Timer.Unit, DefaultTimerName)
	}
	if st.Timer.Config != "/home/tony/.config/bedrock-compass/config.yaml" {
		t.Errorf("Timer.Config = %q", st.Timer.Config)
	}
	if st.Timer.BinaryPath != "/usr/local/bin/bedrock-compass" {
		t.Errorf("Timer.BinaryPath = %q", st.Timer.BinaryPath)
	}
	if st.Engine.Retention != 7 {
		t.Errorf("Engine.Retention = %d, want 7", st.Engine.Retention)
	}
}

func TestNewStack_NilCfgUsesDefaults(t *testing.T) {
	st, err := NewStack(context.Background(), nil, "", StackOpts{})
	if err != nil {
		t.Fatalf("NewStack: %v", err)
	}
	if st.Cfg == nil {
		t.Fatal("Cfg is nil")
	}
	if st.Engine == nil || st.Timer == nil {
		t.Error("Engine/Timer must be non-nil with default config")
	}
}

func TestNewStack_TmuxHasNoSupervisor(t *testing.T) {
	cfg := stackCfg(t)
	cfg.Supervisor = "tmux"
	st, err := NewStack(context.Background(), cfg, "", StackOpts{})
	if err != nil {
		t.Fatalf("NewStack: %v", err)
	}
	if st.Sup != nil {
		t.Errorf("tmux supervisor = %v, want nil in v1 (#13)", st.Sup)
	}
	// The engine still exists: live backups only need the console.
	if st.Engine == nil {
		t.Error("Engine is nil")
	}
}

func TestStackBackupNow_InvalidMode(t *testing.T) {
	st, err := NewStack(context.Background(), stackCfg(t), "", StackOpts{})
	if err != nil {
		t.Fatalf("NewStack: %v", err)
	}
	if _, err := st.BackupNow(context.Background(), "both"); err == nil {
		t.Fatal("invalid mode must error")
	} else if !strings.Contains(err.Error(), "'live' or 'stopped'") {
		t.Errorf("error should name the valid modes, got: %v", err)
	}
}

func TestStackBackupNow_NoEngine(t *testing.T) {
	st := &Stack{}
	if _, err := st.BackupNow(context.Background(), ModeLive); err == nil {
		t.Fatal("nil engine must error")
	}
}
