package compass

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePropsFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "server.properties"), []byte(content), 0o644); err != nil {
		t.Fatalf("write server.properties: %v", err)
	}
}

func TestProps_Rows_ParsesFile(t *testing.T) {
	dir := t.TempDir()
	writePropsFile(t, dir,
		"# Bedrock Dedicated Server\n"+
			"max-players=15\n"+
			"\n"+
			"online-mode=true\n"+
			"motd=My World\n"+
			"weird-custom=42\n")
	p := NewProps(&Config{ServerDir: dir}, nil, nil, nil)
	rows, err := p.Rows()
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 (comments and blank lines skipped)", len(rows))
	}
	want := []struct{ key, value string }{
		{"max-players", "15"},
		{"online-mode", "true"},
		{"motd", "My World"},
		{"weird-custom", "42"},
	}
	for i, w := range want {
		if rows[i].Key != w.key || rows[i].Value != w.value {
			t.Errorf("row %d = {%q %q}, want {%q %q}", i, rows[i].Key, rows[i].Value, w.key, w.value)
		}
	}
}

func TestProps_Info_HotKeys(t *testing.T) {
	p := NewProps(&Config{ServerDir: t.TempDir()}, nil, nil, nil)
	for _, key := range []string{"max-players", "difficulty", "allow-cheats"} {
		if !p.Info(key).Hot {
			t.Errorf("Info(%s).Hot = false, want true", key)
		}
	}
}

func TestProps_Info_ColdKeys(t *testing.T) {
	p := NewProps(&Config{ServerDir: t.TempDir()}, nil, nil, nil)
	for _, key := range []string{"server-port", "online-mode", "allow-list", "gamemode", "level-name", "view-distance", "tick-distance", "motd"} {
		info := p.Info(key)
		if info.Hot {
			t.Errorf("Info(%s).Hot = true, want false", key)
		}
		if info.Allowed == "" || info.Help == "" {
			t.Errorf("Info(%s) = %+v, want allowed-values and help text", key, info)
		}
	}
}

func TestProps_Info_UnknownKey(t *testing.T) {
	p := NewProps(&Config{ServerDir: t.TempDir()}, nil, nil, nil)
	info := p.Info("some-future-key")
	if info.Hot {
		t.Error("unknown key must be cold (a hot path requires a verified console command)")
	}
	if info.Allowed == "" || info.Help == "" {
		t.Errorf("Info(unknown) = %+v, want allowed-values and help text", info)
	}
}

func TestProps_Validate_AcceptsCanonical(t *testing.T) {
	p := NewProps(&Config{ServerDir: t.TempDir()}, nil, nil, nil)
	cases := []struct{ key, value, want string }{
		{"max-players", " 32 ", "32"},
		{"allow-cheats", "TRUE", "true"},
		{"difficulty", "HARD", "hard"},
		{"gamemode", "creative", "creative"},
		{"server-port", "25565", "25565"},
		{"motd", "My World", "My World"},
	}
	for _, c := range cases {
		got, err := p.Validate(c.key, c.value)
		if err != nil {
			t.Errorf("Validate(%q, %q) error: %v", c.key, c.value, err)
			continue
		}
		if got != c.want {
			t.Errorf("Validate(%q, %q) = %q, want %q", c.key, c.value, got, c.want)
		}
	}
}

func TestProps_Validate_RejectsOutOfRangeWithAllowedText(t *testing.T) {
	p := NewProps(&Config{ServerDir: t.TempDir()}, nil, nil, nil)
	cases := []struct{ key, value, allowed string }{
		{"max-players", "-1", "0-65535"},
		{"max-players", "999999", "0-65535"},
		{"max-players", "abc", "0-65535"},
		{"server-port", "80", "1024-65535"},
		{"server-port", "70000", "1024-65535"},
		{"difficulty", "ultra", "peaceful|easy|normal|hard"},
		{"allow-cheats", "yes", "true|false"},
		{"tick-distance", "0", "integer >= 1"},
		{"gamemode", "flying", "default|creative|spectator|survival|adventure"},
		{"motd", "   ", "non-empty string"},
	}
	for _, c := range cases {
		_, err := p.Validate(c.key, c.value)
		if err == nil {
			t.Errorf("Validate(%q, %q) should reject", c.key, c.value)
			continue
		}
		if !strings.Contains(err.Error(), c.allowed) {
			t.Errorf("Validate(%q, %q) error = %q, want it to carry the allowed text %q", c.key, c.value, err, c.allowed)
		}
	}
}

func TestProps_Validate_UnknownKeyAcceptsNonEmpty(t *testing.T) {
	p := NewProps(&Config{ServerDir: t.TempDir()}, nil, nil, nil)
	if got, err := p.Validate("some-future-key", "anything"); err != nil || got != "anything" {
		t.Errorf("Validate(unknown, %q) = %q, %v; want accepted", "anything", got, err)
	}
	if _, err := p.Validate("some-future-key", "  "); err == nil {
		t.Error("Validate(unknown, blank) should reject")
	}
}

func TestProps_Set_Hot_MaxPlayers(t *testing.T) {
	dir := t.TempDir()
	writePropsFile(t, dir, "max-players=15\nonline-mode=true\nmotd=My World\n")
	c, sent := newScriptedConsole(t, map[string]string{"setmaxplayers 32": "ok"})
	trail, err := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	p := NewProps(&Config{ServerDir: dir}, c, trail, nil)

	detail, err := p.Set(context.Background(), "max-players", "32")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !strings.Contains(detail, "applied live") {
		t.Errorf("detail = %q, want a live-apply note", detail)
	}
	if len(*sent) != 1 || (*sent)[0] != "setmaxplayers 32" {
		t.Errorf("console commands = %q, want [\"setmaxplayers 32\"]", *sent)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "server.properties"))
	if want := "max-players=32\nonline-mode=true\nmotd=My World\n"; string(data) != want {
		t.Errorf("file = %q, want %q (value updated, other lines preserved in order)", data, want)
	}
	evs, err := trail.Recent(10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("audit events = %+v, %v; want exactly one", evs, err)
	}
	ev := evs[0]
	if ev.Verb != "config" || ev.Params["key"] != "max-players" || ev.Params["before"] != "15" || ev.Params["after"] != "32" || ev.Result != "ok" {
		t.Errorf("audit = %+v, want verb=config key=max-players before=15 after=32 ok", ev)
	}
}

func TestProps_Set_Hot_ChangesettingKeys(t *testing.T) {
	dir := t.TempDir()
	writePropsFile(t, dir, "difficulty=normal\nallow-cheats=false\n")
	c, sent := newScriptedConsole(t, map[string]string{
		"changesetting difficulty hard":   "ok",
		"changesetting allow-cheats true": "ok",
	})
	p := NewProps(&Config{ServerDir: dir}, c, nil, nil)
	ctx := context.Background()

	if _, err := p.Set(ctx, "difficulty", "hard"); err != nil {
		t.Fatalf("Set difficulty: %v", err)
	}
	if _, err := p.Set(ctx, "allow-cheats", "true"); err != nil {
		t.Fatalf("Set allow-cheats: %v", err)
	}
	if want := []string{"changesetting difficulty hard", "changesetting allow-cheats true"}; !sameStrings(*sent, want) {
		t.Errorf("console commands = %q, want %q", *sent, want)
	}
}

func TestProps_Set_Hot_NoConsoleDegrades(t *testing.T) {
	dir := t.TempDir()
	writePropsFile(t, dir, "max-players=15\n")
	p := NewProps(&Config{ServerDir: dir}, nil, nil, nil)

	detail, err := p.Set(context.Background(), "max-players", "32")
	if err != nil {
		t.Fatalf("Set without a console must still persist; got %v", err)
	}
	if !strings.Contains(detail, "next start") {
		t.Errorf("detail = %q, want a degraded applies-on-next-start note", detail)
	}
	if v := readPropsLine(t, dir, "max-players"); v != "32" {
		t.Errorf("max-players = %q, want 32 (persisted)", v)
	}
}

func TestProps_Set_Hot_LiveApplyFailureStillPersists(t *testing.T) {
	dir := t.TempDir()
	writePropsFile(t, dir, "max-players=15\n")
	c, _ := newScriptedConsole(t, nil)
	c.Write = func(ctx context.Context, line string) error { return context.DeadlineExceeded }
	p := NewProps(&Config{ServerDir: dir}, c, nil, nil)

	detail, err := p.Set(context.Background(), "max-players", "32")
	if err != nil {
		t.Fatalf("a failed live apply must degrade to a note, not an error; got %v", err)
	}
	if !strings.Contains(detail, "live apply failed") {
		t.Errorf("detail = %q, want a live-apply-failure note", detail)
	}
	if v := readPropsLine(t, dir, "max-players"); v != "32" {
		t.Errorf("max-players = %q, want 32 (persisted)", v)
	}
}

func TestProps_Set_InvalidValueNotWritten(t *testing.T) {
	dir := t.TempDir()
	writePropsFile(t, dir, "max-players=15\n")
	c, sent := newScriptedConsole(t, nil)
	trail, _ := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	p := NewProps(&Config{ServerDir: dir}, c, trail, nil)

	_, err := p.Set(context.Background(), "max-players", "-1")
	if err == nil {
		t.Fatal("Set(max-players, -1) should reject")
	}
	if !strings.Contains(err.Error(), "0-65535") {
		t.Errorf("error = %q, want the allowed text", err)
	}
	if v := readPropsLine(t, dir, "max-players"); v != "15" {
		t.Errorf("max-players = %q, want 15 (unchanged)", v)
	}
	if len(*sent) != 0 {
		t.Errorf("console commands = %q, want none (rejected before apply)", *sent)
	}
	evs, _ := trail.Recent(10)
	if len(evs) != 1 || evs[0].Result != "error: "+err.Error() {
		t.Errorf("audit = %+v, want one error event with before=15", evs)
	}
	if evs[0].Params["before"] != "15" || evs[0].Params["after"] != "-1" {
		t.Errorf("audit params = %+v, want before=15 after=-1", evs[0].Params)
	}
}

func TestProps_Set_AppendsMissingKey(t *testing.T) {
	dir := t.TempDir()
	writePropsFile(t, dir, "online-mode=true\n")
	p := NewProps(&Config{ServerDir: dir}, nil, nil, nil)
	if _, err := p.Set(context.Background(), "max-players", "10"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "server.properties"))
	if want := "online-mode=true\nmax-players=10\n"; string(data) != want {
		t.Errorf("file = %q, want %q (key appended)", data, want)
	}
}

func TestProps_Set_MixedCaseKeyLineIsReplaced(t *testing.T) {
	// BDS reads property keys case-insensitively; a hand-edited file with a
	// mixed-case line must be updated in place, not duplicated.
	dir := t.TempDir()
	writePropsFile(t, dir, "Max-Players=15\nmotd=World\n")
	c, sent := newScriptedConsole(t, map[string]string{"setmaxplayers 32": "ok"})
	p := NewProps(&Config{ServerDir: dir}, c, nil, nil)

	detail, err := p.Set(context.Background(), "max-players", "32")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !strings.Contains(detail, "applied live") {
		t.Errorf("detail = %q, want a live-apply note (max-players is hot)", detail)
	}
	if len(*sent) != 1 || (*sent)[0] != "setmaxplayers 32" {
		t.Errorf("console commands = %q, want [\"setmaxplayers 32\"]", *sent)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "server.properties"))
	if want := "max-players=32\nmotd=World\n"; string(data) != want {
		t.Errorf("file = %q, want %q (the mixed-case line replaced, no duplicate)", data, want)
	}
}

func TestProps_Set_Cold_QueuedAndAudited(t *testing.T) {
	dir := t.TempDir()
	writePropsFile(t, dir, "online-mode=true\nallow-list=false\n")
	c, sent := newScriptedConsole(t, nil)
	trail, _ := NewTrail(filepath.Join(t.TempDir(), "audit.jsonl"))
	p := NewProps(&Config{ServerDir: dir}, c, trail, nil)

	detail, err := p.Set(context.Background(), "online-mode", "false")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if want := "queued — applies on restart"; detail != want {
		t.Errorf("detail = %q, want %q", detail, want)
	}
	if len(*sent) != 0 {
		t.Errorf("console commands = %q, want none (cold keys never touch the console)", *sent)
	}
	if v := readPropsLine(t, dir, "online-mode"); v != "false" {
		t.Errorf("online-mode = %q, want false (persisted)", v)
	}
	if v := readPropsLine(t, dir, "allow-list"); v != "false" {
		t.Errorf("allow-list = %q, want false (other lines untouched)", v)
	}
	evs, _ := trail.Recent(10)
	if len(evs) != 1 || evs[0].Verb != "config" || evs[0].Params["key"] != "online-mode" ||
		evs[0].Params["before"] != "true" || evs[0].Params["after"] != "false" || evs[0].Result != "ok" {
		t.Errorf("audit = %+v, want key=online-mode before=true after=false ok", evs)
	}
}

func TestProps_Restart_DelegatesToLifecycle(t *testing.T) {
	var calls []string
	lifecycle := &Lifecycle{Sup: &Supervisor{
		Stop:  func(ctx context.Context) error { calls = append(calls, "stop"); return nil },
		Start: func(ctx context.Context) error { calls = append(calls, "start"); return nil },
	}}
	p := NewProps(&Config{ServerDir: t.TempDir()}, nil, nil, lifecycle)
	if err := p.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if want := []string{"stop", "start"}; !sameStrings(calls, want) {
		t.Errorf("supervisor calls = %q, want %q (a restart)", calls, want)
	}
}

func TestProps_Restart_NoLifecycle(t *testing.T) {
	p := NewProps(&Config{ServerDir: t.TempDir()}, nil, nil, nil)
	if err := p.Restart(context.Background()); err == nil {
		t.Fatal("Restart without a lifecycle should error")
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestProps_Rows_MissingFile(t *testing.T) {
	p := NewProps(&Config{ServerDir: t.TempDir()}, nil, nil, nil)
	if _, err := p.Rows(); err == nil {
		t.Fatal("Rows on a missing server.properties should error")
	}
}

func TestProps_Rows_NoServerDir(t *testing.T) {
	p := NewProps(&Config{}, nil, nil, nil)
	if _, err := p.Rows(); err == nil {
		t.Fatal("Rows without a server dir should error")
	}
}
