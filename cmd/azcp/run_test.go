package main

import (
	"os"
	"path/filepath"
	"testing"
)

// runIn drives the real entry point inside dir and reports its exit status.
func runIn(t *testing.T, dir string, argv ...string) int {
	t.Helper()
	t.Chdir(dir)
	return run(argv)
}

func TestRunCopiesAFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "src"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runIn(t, dir, "--no-progress", "src", "dst"); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	got, err := os.ReadFile(filepath.Join(dir, "dst"))
	if err != nil || string(got) != "payload" {
		t.Fatalf("dst: %q, %v", got, err)
	}
}

// cp's exit statuses are part of the contract: 1 when a file could not be
// copied, 2 when the command line itself was wrong.
func TestRunExitStatuses(t *testing.T) {
	dir := t.TempDir()
	if code := runIn(t, dir, "--no-progress", "no-such-file", "dst"); code != exitFail {
		t.Errorf("missing source: exit %d, want %d", code, exitFail)
	}
	if code := runIn(t, dir, "--not-an-option"); code != exitUsage {
		t.Errorf("bad option: exit %d, want %d", code, exitUsage)
	}
	if code := runIn(t, dir, "--no-progress", "lonely"); code != exitUsage {
		t.Errorf("missing operand: exit %d, want %d", code, exitUsage)
	}
}

func TestRunHelpAndVersion(t *testing.T) {
	dir := t.TempDir()
	if code := runIn(t, dir, "--help"); code != exitOK {
		t.Errorf("--help: exit %d", code)
	}
	if code := runIn(t, dir, "--version"); code != exitOK {
		t.Errorf("--version: exit %d", code)
	}
}

func TestRunRecursiveTreeWithJSONSummary(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"tree/a.txt", "tree/sub/b.txt"} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if code := runIn(t, dir, "--no-progress", "--output=json", "-r", "tree", "out"); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	for _, p := range []string{"out/a.txt", "out/sub/b.txt"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
}
