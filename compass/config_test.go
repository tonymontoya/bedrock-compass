package compass

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

func TestLoad_MissingFile_ReturnsDefaults(t *testing.T) {
	home, _ := os.UserHomeDir()
	cfg, err := Load("/nonexistent/bedrock-compass/config.yaml")
	if err != nil {
		t.Fatalf("missing file must not be an error: %v", err)
	}
	if cfg.Supervisor != "auto" {
		t.Errorf("Supervisor default = %q, want auto", cfg.Supervisor)
	}
	if cfg.Retention != 7 {
		t.Errorf("Retention default = %d, want 7", cfg.Retention)
	}
	if cfg.BackupDir != filepath.Join(home, ".local", "share", "bedrock-compass", "backups") {
		t.Errorf("BackupDir default = %q", cfg.BackupDir)
	}
	if cfg.AuditFile != filepath.Join(home, ".local", "state", "bedrock-compass", "audit.jsonl") {
		t.Errorf("AuditFile default = %q", cfg.AuditFile)
	}
	if cfg.ServerDir != "" {
		t.Errorf("ServerDir default = %q, want empty (autodiscover)", cfg.ServerDir)
	}
	if cfg.IncludePacks {
		t.Error("IncludePacks default must be false (off)")
	}
}

func TestLoad_ExplicitKeysOverrideDefaults(t *testing.T) {
	p := writeFile(t, `
server-dir: /srv/bedrock
supervisor: systemd
service: minecraft-bedrock.service
backup-dir: /var/backups/bedrock
retention: 3
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ServerDir != "/srv/bedrock" {
		t.Errorf("ServerDir = %q", cfg.ServerDir)
	}
	if cfg.Supervisor != "systemd" {
		t.Errorf("Supervisor = %q", cfg.Supervisor)
	}
	if cfg.Service != "minecraft-bedrock.service" {
		t.Errorf("Service = %q", cfg.Service)
	}
	if cfg.BackupDir != "/var/backups/bedrock" {
		t.Errorf("BackupDir = %q", cfg.BackupDir)
	}
	if cfg.Retention != 3 {
		t.Errorf("Retention = %d", cfg.Retention)
	}
	// untouched keys keep their defaults
	if cfg.AuditFile == "" {
		t.Error("AuditFile lost its default")
	}
}

func TestLoad_UnknownKey_Errors(t *testing.T) {
	p := writeFile(t, "servr-dir: /oops")
	_, err := Load(p)
	if err == nil {
		t.Fatal("unknown key must be a hard error")
	}
	if !strings.Contains(err.Error(), "servr-dir") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestLoad_InvalidRetention_Errors(t *testing.T) {
	p := writeFile(t, "retention: 0")
	if _, err := Load(p); err == nil {
		t.Fatal("retention 0 must be rejected")
	}
}

func TestLoad_InvalidSupervisor_Errors(t *testing.T) {
	p := writeFile(t, "supervisor: docker")
	if _, err := Load(p); err == nil {
		t.Fatal("unknown supervisor must be rejected")
	}
}

func TestLoad_MalformedYAML_Errors(t *testing.T) {
	p := writeFile(t, "retention: [unclosed")
	if _, err := Load(p); err == nil {
		t.Fatal("malformed YAML must be rejected")
	}
}

func TestLoad_IncludePacks(t *testing.T) {
	p := writeFile(t, "include-packs: true")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.IncludePacks {
		t.Error("IncludePacks = false, want true")
	}
}

func TestLoad_RemoteKey_Rejected(t *testing.T) {
	// Remote push was removed from v1 (D7 revised 2026-08-16): an operator
	// with an old config must get a hard error, not a silent ignore.
	p := writeFile(t, `
remote:
  enabled: true
  target: backup-host
  dir: /remote/bedrock
`)
	if _, err := Load(p); err == nil {
		t.Fatal("the remote key was removed from v1 config; it must be rejected")
	}
}

func TestLoad_EmptyFile_ReturnsDefaults(t *testing.T) {
	p := writeFile(t, "")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("empty file must not be an error: %v", err)
	}
	if cfg.Retention != 7 || cfg.Supervisor != "auto" {
		t.Errorf("empty file must yield defaults, got %+v", cfg)
	}
}

func TestLoad_Port(t *testing.T) {
	p := writeFile(t, "port: 25000")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 25000 {
		t.Errorf("Port = %d, want 25000", cfg.Port)
	}
}

func TestLoad_PortDefault(t *testing.T) {
	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 0 {
		t.Errorf("Port default = %d, want 0 (autodiscover)", cfg.Port)
	}
}

func TestLoad_ConsoleFifDefault(t *testing.T) {
	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ConsoleFif != "/run/bedrock-console.fifo" {
		t.Errorf("ConsoleFif default = %q, want the ADR-0001 path", cfg.ConsoleFif)
	}
}

func TestLoad_ConsoleFifOverride(t *testing.T) {
	p := writeFile(t, "console-fif: /run/my-server.fifo")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ConsoleFif != "/run/my-server.fifo" {
		t.Errorf("ConsoleFif = %q", cfg.ConsoleFif)
	}
}
