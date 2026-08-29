// Package compass is the TUI-free domain core of bedrock-compass.
//
// It is a public (non-internal) package on purpose: the v2 in-game bridge
// must be able to link against it (D15). Nothing in this package may import
// the TUI; the depguard test enforces that.
package compass

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the resolved runtime configuration. Empty fields mean
// "autodiscover" (D13).
type Config struct {
	// ServerDir holds the directory containing server.properties.
	// Empty = autodiscover from the running process.
	ServerDir string `yaml:"server-dir"`
	// Supervisor is "auto", "systemd", or "tmux".
	Supervisor string `yaml:"supervisor"`
	// Service names the systemd unit when Supervisor is systemd.
	Service string `yaml:"service"`
	// Pane names the tmux pane (session:pane) when Supervisor is tmux.
	Pane string `yaml:"pane"`
	// BackupDir is the local backup root, outside the server dir (D7).
	BackupDir string `yaml:"backup-dir"`
	// Retention is how many backups to keep; oldest beyond it are pruned.
	Retention int `yaml:"retention"`
	// AuditFile is the JSONL audit trail path (D11).
	AuditFile string `yaml:"audit-file"`
	// IncludePacks also backs up behavior_packs/ and resource_packs/.
	// Off by default: shared packs are operator-managed content, not world
	// state (D7).
	IncludePacks bool `yaml:"include-packs"`
	// Port is the server's UDP port; 0 = autodiscover from server.properties.
	Port int `yaml:"port"`
	// ConsoleFif is the path to the server's console FIFO (ADR-0001).
	// Empty = /run/bedrock-console.fifo.
	ConsoleFif string `yaml:"console-fif"`
}

func defaults() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		Supervisor: "auto",
		BackupDir:  filepath.Join(home, ".local", "share", "bedrock-compass", "backups"),
		Retention:  7,
		AuditFile:  filepath.Join(home, ".local", "state", "bedrock-compass", "audit.jsonl"),
		ConsoleFif: "/run/bedrock-console.fifo",
	}
}

// Load reads the configuration at path. A missing file is not an error:
// it yields the documented defaults (autodiscover mode). Unknown keys are
// a hard error so operator typos surface immediately.
func Load(path string) (*Config, error) {
	cfg := defaults()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.Retention < 1 {
		return fmt.Errorf("retention must be >= 1, got %d", c.Retention)
	}
	switch c.Supervisor {
	case "", "auto", "systemd", "tmux":
	default:
		return fmt.Errorf("supervisor must be auto, systemd, or tmux, got %q", c.Supervisor)
	}
	return nil
}

// StandardPath is the default config location:
// $XDG_CONFIG_HOME/bedrock-compass/config.yaml, falling back to
// ~/.config/bedrock-compass/config.yaml.
func StandardPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "bedrock-compass", "config.yaml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "bedrock-compass", "config.yaml")
}

// LoadStandard loads from StandardPath().
func LoadStandard() (*Config, error) { return Load(StandardPath()) }

// Save persists the config to path, creating the parent directory when
// needed. It writes to a temp file in the same directory and renames it
// into place, so a crash mid-write never leaves a torn config. Used by
// adopt (#13) to record the deployment the tool has claimed.
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create config dir: %w", err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename config into place: %w", err)
	}
	return nil
}
