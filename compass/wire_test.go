package compass

import (
	"context"
	"os"
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
	cfg := stackCfg(t)
	st, err := NewStack(context.Background(), cfg, "1.26.430", StackOpts{
		ConfigPath: "/etc/bedrock-compass/config.yaml",
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
	if st.Timer.Config != "/etc/bedrock-compass/config.yaml" {
		t.Errorf("Timer.Config = %q", st.Timer.Config)
	}
	if st.Timer.BinaryPath != "/usr/local/bin/bedrock-compass" {
		t.Errorf("Timer.BinaryPath = %q", st.Timer.BinaryPath)
	}
	if st.Engine.Retention != 7 {
		t.Errorf("Engine.Retention = %d, want 7", st.Engine.Retention)
	}
	if st.Logs == nil || st.Logs.Journal == nil {
		t.Fatal("Logs is nil, want the journal read surface for systemd deployments")
	}
	if st.Logs.Journal.Unit != "minecraft-bedrock.service" {
		t.Errorf("Logs.Journal.Unit = %q", st.Logs.Journal.Unit)
	}
	if st.RawConsole == nil || st.RawConsole.Console == nil {
		t.Fatal("RawConsole is nil or has no channel, want the verbatim console surface")
	}
	if st.RawConsole.Trail == nil {
		t.Error("RawConsole.Trail is nil, want the audit trail (raw commands are always audited)")
	}

	// Props (#11) is built with the resolved config, console, trail and the
	// restart path.
	if st.Props == nil {
		t.Error("Props is nil, want the server.properties surface")
	}
	if st.Props.Trail == nil {
		t.Error("Props.Trail is nil, want the audit trail")
	}
	if st.Props.Lifecycle == nil {
		t.Error("Props.Lifecycle is nil, want the restart path for cold keys")
	}
	if st.Props.Console == nil {
		t.Error("Props.Console is nil, want the console channel for hot apply")
	}
	if st.Props.Cfg == nil {
		t.Error("Props.Cfg is nil, want the resolved config")
	}
	if st.Props.Cfg.ServerDir != cfg.ServerDir {
		t.Errorf("Props.Cfg.ServerDir = %q, want the resolved server dir", st.Props.Cfg.ServerDir)
	}

	// Provision (#14) is built unconditionally with a real joinability probe
	// and the audit trail: a fresh host with nothing running is exactly the
	// case it exists to serve.
	if st.Provision == nil {
		t.Error("Provision is nil, want the install/adopt surface")
	}
	if st.Provision.Trail == nil {
		t.Error("Provision.Trail is nil, want the audit trail")
	}
	if st.Provision.Env == nil {
		t.Error("Provision.Env is nil, want the resolved host env")
	}
	if st.Provision.Verify == nil {
		t.Error("Provision.Verify is nil, want the joinability probe")
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
	// A configured unit name (cfg.Service) yields a journal surface even for
	// tmux deployments — resolveChannels pins it before branching on kind.
	if st.Logs == nil || st.Logs.Journal == nil {
		t.Fatal("Logs is nil, want the journal read surface")
	}
	if st.Logs.Journal.Unit != "minecraft-bedrock.service" {
		t.Errorf("Logs.Journal.Unit = %q", st.Logs.Journal.Unit)
	}
	// The raw console surface exists even with no resolved channel (no pane):
	// Send degrades to a clean, audited error rather than panicking.
	if st.RawConsole == nil {
		t.Fatal("RawConsole is nil, want the verbatim console surface")
	}
	if st.RawConsole.Console != nil {
		t.Error("RawConsole.Console must be nil when no pane resolved")
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

func TestPortFromProperties(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		body string
		want int
	}{
		{"present", "level-name=World\nserver-port=19142\nallow-list=false\n", 19142},
		{"absent key", "level-name=World\nallow-list=false\n", 0},
		{"unparseable", "server-port=notanumber\n", 0},
		{"nonpositive", "server-port=-5\n", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, "server.properties"), []byte(c.body), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := portFromProperties(dir); got != c.want {
				t.Errorf("portFromProperties = %d, want %d", got, c.want)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		if got := portFromProperties(filepath.Join(dir, "nope")); got != 0 {
			t.Errorf("portFromProperties(missing) = %d, want 0", got)
		}
	})
}

// TestNewStack_AutodiscoversPort locks the documented "Port 0 = autodiscover"
// behavior: when the config does not pin a port, NewStack reads server-port=
// from the resolved server dir so consumers (telemetry, lifecycle, access
// firewall commands) target the real port instead of the BDS default.
func TestNewStack_AutodiscoversPort(t *testing.T) {
	cfg := stackCfg(t)
	if err := os.WriteFile(filepath.Join(cfg.ServerDir, "server.properties"),
		[]byte("level-name=World\nserver-port=19142\nallow-list=false\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, err := NewStack(context.Background(), cfg, "", StackOpts{})
	if err != nil {
		t.Fatalf("NewStack: %v", err)
	}
	if st.Cfg.Port != 19142 {
		t.Errorf("Cfg.Port = %d, want 19142 (autodiscovered)", st.Cfg.Port)
	}

	// An explicit config port always wins over the file.
	cfg2 := stackCfg(t)
	cfg2.Port = 25000
	if err := os.WriteFile(filepath.Join(cfg2.ServerDir, "server.properties"),
		[]byte("server-port=19142\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	st2, err := NewStack(context.Background(), cfg2, "", StackOpts{})
	if err != nil {
		t.Fatalf("NewStack: %v", err)
	}
	if st2.Cfg.Port != 25000 {
		t.Errorf("Cfg.Port = %d, want 25000 (explicit wins)", st2.Cfg.Port)
	}
}
