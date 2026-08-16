package compass

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// installFakeJournalctl puts a fake journalctl on PATH that records its args
// to a file and prints a canned stdout, exiting with a canned status.
func installFakeJournalctl(t *testing.T, stdout string, exit int) (argsFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell shim test")
	}
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	stdoutFile := filepath.Join(dir, "stdout")
	if err := os.WriteFile(stdoutFile, []byte(stdout), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> \"$JT_ARGS_FILE\"\n" +
		"cat \"$JT_STDOUT_FILE\"\n" +
		"exit " + strconv.Itoa(exit) + "\n"
	fake := filepath.Join(dir, "journalctl")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("JT_ARGS_FILE", argsFile)
	t.Setenv("JT_STDOUT_FILE", stdoutFile)
	return argsFile
}

func readArgs(t *testing.T, argsFile string) []string {
	t.Helper()
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("fake journalctl never ran: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestJournal_Cursor(t *testing.T) {
	argsFile := installFakeJournalctl(t,
		"Aug 16 02:21:13 minecraft sh[74883]: [2026-08-16 02:21:13:882 INFO] Server started.\n"+
			"-- cursor: s=80754b;i=88dd;b=b73b\n", 0)
	j := &Journal{Unit: "minecraft-bedrock"}
	cursor, err := j.Cursor(context.Background())
	if err != nil {
		t.Fatalf("Cursor: %v", err)
	}
	if cursor != "s=80754b;i=88dd;b=b73b" {
		t.Fatalf("cursor=%q", cursor)
	}
	args := readArgs(t, argsFile)
	want := "-u minecraft-bedrock --show-cursor -n 1 --no-pager"
	if strings.Join(args, " ") != want {
		t.Fatalf("args=%q, want %q", strings.Join(args, " "), want)
	}
}

func TestJournal_Cursor_NoEntries(t *testing.T) {
	installFakeJournalctl(t, "-- No entries --\n", 1)
	j := &Journal{Unit: "minecraft-bedrock"}
	cursor, err := j.Cursor(context.Background())
	if err != nil {
		t.Fatalf("Cursor with no entries should be a clean empty, got %v", err)
	}
	if cursor != "" {
		t.Fatalf("cursor=%q, want empty", cursor)
	}
}

func TestJournal_NewSince_ParsesMessages(t *testing.T) {
	installFakeJournalctl(t,
		"Aug 16 02:21:29 minecraft sh[74883]: [2026-08-16 02:21:29:146 INFO] §2--- Showing help page 1 of 4 ---\n"+
			"Aug 16 02:21:29 minecraft sh[74883]: /? <page: int>\n"+
			"Aug 16 02:21:29 minecraft sh[74883]: /deop <player: target>\n"+
			"Aug 16 02:21:29 minecraft sh[74883]: [2026-08-16 02:21:29:146 INFO]\n", 0)
	j := &Journal{Unit: "minecraft-bedrock"}
	lines, err := j.NewSince(context.Background(), "")
	if err != nil {
		t.Fatalf("NewSince: %v", err)
	}
	// Note: the final journal entry is a log line whose content is empty; its
	// message is the BDS tag itself. Faithful capture keeps it.
	want := []string{
		"[2026-08-16 02:21:29:146 INFO] §2--- Showing help page 1 of 4 ---",
		"/? <page: int>",
		"/deop <player: target>",
		"[2026-08-16 02:21:29:146 INFO]",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines %q, want %d %q", len(lines), lines, len(want), want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestJournal_NewSince_AfterCursor(t *testing.T) {
	argsFile := installFakeJournalctl(t, "", 0)
	j := &Journal{Unit: "minecraft-bedrock"}
	if _, err := j.NewSince(context.Background(), "TOK123"); err != nil {
		t.Fatalf("NewSince: %v", err)
	}
	args := readArgs(t, argsFile)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-u minecraft-bedrock") ||
		!strings.Contains(joined, "--after-cursor TOK123") ||
		!strings.Contains(joined, "--no-pager") {
		t.Fatalf("args=%q", joined)
	}
}

func TestJournal_NewSince_EmptyTokenOmitsCursor(t *testing.T) {
	argsFile := installFakeJournalctl(t, "", 0)
	j := &Journal{Unit: "minecraft-bedrock"}
	if _, err := j.NewSince(context.Background(), ""); err != nil {
		t.Fatalf("NewSince: %v", err)
	}
	joined := strings.Join(readArgs(t, argsFile), " ")
	if strings.Contains(joined, "--after-cursor") {
		t.Fatalf("empty token must not pass --after-cursor: %q", joined)
	}
}

func TestJournal_NewSince_NoEntries(t *testing.T) {
	installFakeJournalctl(t, "-- No entries --\n", 1)
	j := &Journal{Unit: "minecraft-bedrock"}
	lines, err := j.NewSince(context.Background(), "TOK")
	if err != nil {
		t.Fatalf("no new entries should be clean, got %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("lines=%q, want empty", lines)
	}
}
