package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/progress"
)

// runWithStdin is run with the interactive prompt fed from a string.
func runWithStdin(t *testing.T, dir string, stdin io.Reader, argv ...string) int64 {
	t.Helper()
	t.Chdir(dir)
	opt, err := cli.Parse(argv)
	if err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	e, err := New(Config{
		Options:  opt,
		Log:      slog.New(slog.DiscardHandler),
		Progress: progress.New(progress.Config{Mode: progress.ModeNever}),
		Stdin:    stdin,
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	failed, runErr := e.Run(context.Background())
	if runErr != nil {
		t.Fatalf("run %v: %v", argv, runErr)
	}
	return failed
}

// Answers piped ahead of the questions must all be heard: one buffered reader
// per prompt used to swallow every line after the first.
func TestInteractivePromptReadsEveryAnswer(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a"), "new a")
	write(t, filepath.Join(dir, "b"), "new b")
	write(t, filepath.Join(dir, "c"), "new c")
	write(t, filepath.Join(dir, "dst", "a"), "old a")
	write(t, filepath.Join(dir, "dst", "b"), "old b")
	write(t, filepath.Join(dir, "dst", "c"), "old c")

	// The scanner asks in order; say yes, no, yes.
	runWithStdin(t, dir, strings.NewReader("y\nn\ny\n"), "-i", "a", "b", "c", "dst")

	if got := read(t, filepath.Join(dir, "dst", "a")); got != "new a" {
		t.Errorf(`first "y" was not honoured: %q`, got)
	}
	if got := read(t, filepath.Join(dir, "dst", "b")); got != "old b" {
		t.Errorf(`the "n" was not honoured: %q`, got)
	}
	if got := read(t, filepath.Join(dir, "dst", "c")); got != "new c" {
		t.Errorf(`second "y" was not honoured: %q`, got)
	}
}

// Once stdin runs dry the remaining prompts answer themselves with "no";
// nothing may be overwritten on the strength of nobody objecting.
func TestPromptWithoutAnswersLeavesFilesAlone(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a"), "new")
	write(t, filepath.Join(dir, "dst", "a"), "old")
	runWithStdin(t, dir, strings.NewReader(""), "-i", "a", "dst")
	if got := read(t, filepath.Join(dir, "dst", "a")); got != "old" {
		t.Errorf("EOF at the prompt overwrote the file: %q", got)
	}
}

// --attributes-only copies attributes onto an existing destination without
// touching its data, exactly as cp does.
func TestAttributesOnlyKeepsDestinationData(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "src"), "source data")
	if err := os.Chmod(filepath.Join(dir, "src"), 0o640); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "dst"), "destination data")

	if failed := run(t, dir, "--attributes-only", "-p", "src", "dst"); failed != 0 {
		t.Fatalf("%d files failed", failed)
	}
	if got := read(t, filepath.Join(dir, "dst")); got != "destination data" {
		t.Errorf("--attributes-only altered the data: %q", got)
	}
	fi, err := os.Stat(filepath.Join(dir, "dst"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode not copied: %v", fi.Mode())
	}
}

// Numbered backups must work for names that contain glob metacharacters.
func TestNumberedBackupWithMetacharacterName(t *testing.T) {
	dir := t.TempDir()
	name := "report[1].pdf"
	write(t, filepath.Join(dir, "src"), "v2")
	write(t, filepath.Join(dir, name), "v1")
	write(t, filepath.Join(dir, name+".~1~"), "v0")

	if failed := run(t, dir, "--backup=numbered", "src", name); failed != 0 {
		t.Fatalf("%d files failed", failed)
	}
	if got := read(t, filepath.Join(dir, name)); got != "v2" {
		t.Errorf("destination: %q", got)
	}
	if got := read(t, filepath.Join(dir, name+".~2~")); got != "v1" {
		t.Errorf("backup: %q", got)
	}
	if got := read(t, filepath.Join(dir, name+".~1~")); got != "v0" {
		t.Errorf("older backup disturbed: %q", got)
	}
}
