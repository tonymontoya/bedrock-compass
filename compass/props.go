// Package compass: the server.properties surface (ticket #11). It is the
// TUI-free domain core behind the Config screen: it reads the file's keys,
// classifies each as hot (applies via the console without a restart) or cold
// (applies on restart), validates values against each key's allowed set, and
// writes changes atomically with before/after audit (D11). Hot keys map to
// the console commands verified in research/01 (Q5): max-players via
// `setmaxplayers`, difficulty and allow-cheats via `changesetting`.
package compass

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// KeyInfo is the metadata for one server.properties key: whether it can be
// applied without a restart, the values it accepts (shown as help and in
// validation errors), and a one-line description for the operator.
type KeyInfo struct {
	Hot     bool
	Allowed string
	Help    string
	check   func(string) (string, error)
}

// PropRow is one server.properties key with its current on-disk value and
// its metadata.
type PropRow struct {
	Key   string
	Value string
	Info  KeyInfo
}

// Props is the ticket #11 service over already-resolved seams: Cfg supplies
// the server dir (where server.properties lives), Console applies hot
// changes to a running server (nil = no channel; hot changes then persist
// and apply on next start), Trail records every mutation (D11), and
// Lifecycle restarts the server so queued cold changes take effect.
type Props struct {
	Cfg       *Config
	Console   *Console
	Trail     *Trail
	Lifecycle *Lifecycle
}

// NewProps wires the service. A nil cfg or unresolved ServerDir still yields
// a usable Props: reads fail with a clear "server dir not resolved" error
// rather than panicking.
func NewProps(cfg *Config, console *Console, trail *Trail, lifecycle *Lifecycle) *Props {
	return &Props{Cfg: cfg, Console: console, Trail: trail, Lifecycle: lifecycle}
}

// Rows reads server.properties and returns its keys in file order, each with
// its current value and metadata. Comment (#) and blank lines are skipped, as
// are lines without an '='. The file is the source of truth for which keys
// exist; the metadata table supplies classification and help for known keys.
func (p *Props) Rows() ([]PropRow, error) {
	if p.Cfg == nil || p.Cfg.ServerDir == "" {
		return nil, errors.New("server dir not resolved")
	}
	data, err := os.ReadFile(filepath.Join(p.Cfg.ServerDir, "server.properties"))
	if err != nil {
		return nil, fmt.Errorf("read server.properties: %w", err)
	}
	var rows []PropRow
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		key, value, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		rows = append(rows, PropRow{Key: key, Value: strings.TrimSpace(value), Info: p.Info(key)})
	}
	return rows, nil
}

// Info returns the metadata for key. Known BDS keys come from the canonical
// table (research/01 Q5: the only hot keys are max-players, difficulty, and
// allow-cheats; ADR-0008: even reloadconfig does not move the other keys at
// runtime, so everything else is cold). Unknown keys degrade to cold +
// free-form so the screen can still render and edit them honestly.
func (p *Props) Info(key string) KeyInfo {
	if k, ok := keyTable[strings.ToLower(key)]; ok {
		return k
	}
	return KeyInfo{Hot: false, Allowed: "free-form", Help: "unknown key (applies on restart)"}
}

// keyTable is the canonical metadata for the server.properties keys BDS 1.26
// documents. Values are validated by check (nil = free-form, non-empty).
var keyTable = map[string]KeyInfo{
	"allow-cheats":            {Hot: true, Allowed: "true|false", Help: "allow cheats in the world (hot: changesetting)", check: checkBool},
	"allow-list":              {Allowed: "true|false", Help: "allowlist gate — rejects every player not on the list (see Access screen)", check: checkBool},
	"broadcast-to-lan":        {Allowed: "true|false", Help: "broadcast the world to the local network", check: checkBool},
	"broadcast-radius":        {Allowed: "integer >= 0", Help: "radius of the LAN broadcast", check: checkNonNegative},
	"debug":                   {Allowed: "true|false", Help: "enable debug output", check: checkBool},
	"difficulty":              {Hot: true, Allowed: "peaceful|easy|normal|hard", Help: "world difficulty (hot: changesetting)", check: checkEnum("peaceful", "easy", "normal", "hard")},
	"enable-lan-world-upload": {Allowed: "true|false", Help: "allow uploading the world over the local network", check: checkBool},
	"force-gamemode":          {Allowed: "true|false", Help: "force every player into the set gamemode", check: checkBool},
	"gamemode":                {Allowed: "default|creative|spectator|survival|adventure", Help: "default gamemode for new players", check: checkEnum("default", "creative", "spectator", "survival", "adventure")},
	"level-name":              {Allowed: "non-empty string", Help: "world folder name (world-creation only)", check: checkNonEmpty},
	"level-seed":              {Allowed: "non-empty string", Help: "world seed (world-creation only)", check: checkNonEmpty},
	"level-type":              {Allowed: "non-empty string", Help: "world type (world-creation only)", check: checkNonEmpty},
	"max-players":             {Hot: true, Allowed: "0-65535", Help: "max concurrent players (hot: setmaxplayers)", check: checkRange(0, 65535)},
	"server-name":             {Allowed: "non-empty string", Help: "server name shown in the server list", check: checkNonEmpty},
	"max-thread-cores":        {Allowed: "integer >= 1", Help: "max thread cores the server may use", check: checkPositive},
	"motd":                    {Allowed: "non-empty string", Help: "server name shown in the server list", check: checkNonEmpty},
	"online-mode":             {Allowed: "true|false", Help: "require Xbox sign-in; the server needs Microsoft services reachable", check: checkBool},
	"server-port":             {Allowed: "1024-65535", Help: "UDP game port", check: checkRange(1024, 65535)},
	"tick-distance":           {Allowed: "integer >= 1", Help: "how far from players the world ticks", check: checkPositive},
	"view-distance":           {Allowed: "integer >= 1", Help: "rendering distance in chunks", check: checkPositive},
	"white-list":              {Allowed: "true|false", Help: "legacy alias of allow-list", check: checkBool},
}

// Validate canonicalizes value for key and returns it, or an error that
// carries the key's allowed-values text so the caller can show it to the
// operator. Unknown keys accept any non-empty value.
func (p *Props) Validate(key, value string) (string, error) {
	info := p.Info(key)
	check := info.check
	if check == nil {
		check = checkNonEmpty
	}
	canonical, err := check(value)
	if err != nil {
		return "", fmt.Errorf("invalid value for %s: %v (allowed: %s)", key, err, info.Allowed)
	}
	return canonical, nil
}

// Set changes key to value. It validates the value (the error carries the
// allowed-values text), writes server.properties atomically with every other
// line preserved in order, and then applies the change:
//
//   - hot keys go through the console (setmaxplayers / changesetting) and
//     report "applied live"; with no console, or when the live apply fails,
//     the persisted value is kept and the detail degrades to "applies on
//     next start" (the Access.SetGate house pattern — the durable write is
//     the primary effect);
//   - cold keys report "queued — applies on restart": the file IS the queue
//     (BDS reads server.properties at startup); the caller offers the
//     restart (Restart) at the restart tier.
//
// Every path is audit-logged with the before/after values (D11).
func (p *Props) Set(ctx context.Context, key, value string) (string, error) {
	key = strings.ToLower(key) // canonical form for table lookups, the written line, and the audit
	if p.Cfg == nil || p.Cfg.ServerDir == "" {
		err := errors.New("server dir not resolved")
		p.audit(key, "", value, err)
		return "", err
	}
	path := filepath.Join(p.Cfg.ServerDir, "server.properties")
	data, err := os.ReadFile(path)
	if err != nil {
		err = fmt.Errorf("read server.properties: %w", err)
		p.audit(key, "", value, err)
		return "", err
	}
	before := valueOf(string(data), key)

	canonical, err := p.Validate(key, value)
	if err != nil {
		p.audit(key, before, value, err)
		return "", err
	}
	if err := writeKey(path, key, canonical, string(data)); err != nil {
		p.audit(key, before, canonical, err)
		return "", err
	}

	info := p.Info(key)
	if !info.Hot {
		p.audit(key, before, canonical, nil)
		return "queued — applies on restart", nil
	}
	cmd, ok := hotCommand(key, canonical)
	if !ok {
		err := fmt.Errorf("hot key %s has no console command", key)
		p.audit(key, before, canonical, err)
		return "", err
	}
	if p.Console == nil {
		p.audit(key, before, canonical, nil)
		return "persisted; applies on next start (no console)", nil
	}
	if _, cerr := p.Console.Send(ctx, cmd); cerr != nil {
		p.audit(key, before, canonical, nil)
		return "persisted; live apply failed (" + cerr.Error() + ") — applies on next start", nil
	}
	p.audit(key, before, canonical, nil)
	return "applied live (" + cmd + ")", nil
}

// Restart makes queued cold changes take effect: the file already holds the
// new values (BDS reads server.properties at startup), so a restart is the
// apply step. Delegates to Lifecycle, which audits the restart itself.
func (p *Props) Restart(ctx context.Context) error {
	if p.Lifecycle == nil {
		return errors.New("no lifecycle resolved (cannot restart the server)")
	}
	return p.Lifecycle.Restart(ctx)
}

// hotCommand maps a hot key to its console command (research/01 Q5).
func hotCommand(key, value string) (string, bool) {
	switch strings.ToLower(key) {
	case "max-players":
		return "setmaxplayers " + value, true
	case "difficulty":
		return "changesetting difficulty " + value, true
	case "allow-cheats":
		return "changesetting allow-cheats " + value, true
	}
	return "", false
}

// valueOf returns the current on-disk value of key ("" when absent). Matching
// is case-insensitive: BDS reads property keys case-insensitively, so a
// mixed-case line in a hand-edited file is the same key.
func valueOf(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if k, v, ok := strings.Cut(t, "="); ok && strings.EqualFold(strings.TrimSpace(k), key) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// writeKey sets key=value in the file at path, preserving every other line
// in order. A missing key is appended at the end. The trailing newline of a
// newline-terminated file is preserved exactly once (no stray blank line).
// The write is atomic (temp+rename), so a crash mid-write never leaves a
// torn file.
func writeKey(path, key, value, current string) error {
	lines := strings.Split(current, "\n")
	trailingNL := len(lines) > 0 && lines[len(lines)-1] == ""
	if trailingNL {
		lines = lines[:len(lines)-1]
	}
	out := make([]string, 0, len(lines)+1)
	found := false
	for _, line := range lines {
		if k, _, ok := strings.Cut(strings.TrimSpace(line), "="); ok && strings.EqualFold(strings.TrimSpace(k), key) {
			// Replace in place with the canonical (lowercase) key spelling.
			out = append(out, key+"="+value)
			found = true
			continue
		}
		out = append(out, line)
	}
	if !found {
		out = append(out, key+"="+value)
	}
	result := strings.Join(out, "\n")
	if trailingNL {
		result += "\n"
	}
	if err := writeFileAtomic(path, []byte(result)); err != nil {
		return fmt.Errorf("write server.properties: %w", err)
	}
	return nil
}

// audit appends one trail event, best effort (a missing trail must not break
// a config action).
func (p *Props) audit(key, before, after string, err error) {
	if p.Trail == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error: " + err.Error()
	}
	_ = p.Trail.Append(Event{
		Verb:   "config",
		Params: map[string]string{"key": key, "before": before, "after": after},
		Result: result,
	})
}

// checkBool accepts true/false case-insensitively and canonicalizes to
// lowercase (the form BDS writes).
func checkBool(value string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "true" || v == "false" {
		return v, nil
	}
	return "", errors.New("not a boolean")
}

// checkRange accepts an integer in [min, max].
func checkRange(min, max int) func(string) (string, error) {
	return func(value string) (string, error) {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < min || n > max {
			return "", errors.New("not an integer in range")
		}
		return strconv.Itoa(n), nil
	}
}

// checkNonNegative accepts an integer >= 0.
func checkNonNegative(value string) (string, error) {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n < 0 {
		return "", errors.New("not a non-negative integer")
	}
	return strconv.Itoa(n), nil
}

// checkPositive accepts an integer >= 1.
func checkPositive(value string) (string, error) {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n < 1 {
		return "", errors.New("not a positive integer")
	}
	return strconv.Itoa(n), nil
}

// checkEnum accepts one of the listed values (case-insensitive).
func checkEnum(values ...string) func(string) (string, error) {
	return func(value string) (string, error) {
		v := strings.ToLower(strings.TrimSpace(value))
		for _, allowed := range values {
			if v == allowed {
				return v, nil
			}
		}
		return "", errors.New("not one of the allowed values")
	}
}

// checkNonEmpty accepts any non-empty value.
func checkNonEmpty(value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", errors.New("empty value")
	}
	return v, nil
}
