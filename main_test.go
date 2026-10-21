package main

import (
	"os"
	"path/filepath"
	"testing"
)

func testRun(t *testing.T, args ...string) error {
	t.Helper()
	old := os.Args
	os.Args = append([]string{"msg"}, args...)
	defer func() { os.Args = old }()
	account = ""
	return run()
}

func TestRunCanonicalizesAliasBeforeStateWrites(t *testing.T) {
	dir := configTestDir(t)
	writeTestConfig(t, filepath.Join(dir, "config.json"), `{"accounts":{"droid":{"jid":"droid@example.com","password":"s"}},"accountAliases":{"robot":"droid"}}`)
	if err := testRun(t, "--as", "robot", "check", "--no-receipt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "next_poll_due.droid")); err != nil {
		t.Fatal("missing canonical poll state:", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "next_poll_due.robot")); !os.IsNotExist(err) {
		t.Fatal("alias must not have separate state:", err)
	}
	if err := testRun(t, "--as", "missing", "check", "--no-receipt"); err == nil {
		t.Fatal("unknown account must fail before writing state")
	}
	if _, err := os.Stat(filepath.Join(dir, "next_poll_due.missing")); !os.IsNotExist(err) {
		t.Fatal("unknown account wrote state:", err)
	}
}

func TestRunDaemonManagementSurvivesBrokenConfig(t *testing.T) {
	dir := configTestDir(t)
	for _, raw := range []string{`{`, `{"accounts":{}}`} {
		writeTestConfig(t, filepath.Join(dir, "config.json"), raw)
		for _, cmd := range []string{"status", "stop"} {
			// No pidfiles exist in this isolated directory: no live daemon is
			// signalled, but management must remain usable without valid JSON.
			if err := testRun(t, "--as", "droid", cmd); err != nil {
				t.Fatalf("%s with broken/removed config: %v", cmd, err)
			}
		}
		if err := testRun(t, "--as", "droid", "listen"); err == nil {
			t.Fatal("listen must not bypass config validation")
		}
	}
}
