package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// --files-from is how a pipeline hands over more sources than a command line
// holds. Listed names are sources like any other, and the destination keeps
// its place as the last thing typed.
func TestFilesFromAddsSources(t *testing.T) {
	list := filepath.Join(t.TempDir(), "list.txt")
	if err := os.WriteFile(list, []byte("a.txt\r\n\nsub/b.txt\nazure://acct/c/k\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	listed := []string{"a.txt", "sub/b.txt", "azure://acct/c/k"}

	o := mustParse(t, "--files-from", list, "dst")
	if !slices.Equal(o.Sources, listed) || o.Dest != "dst" {
		t.Errorf("sources %v -> %q", o.Sources, o.Dest)
	}
	o = mustParse(t, "--files-from="+list, "typed.txt", "dst")
	if want := append(slices.Clone(listed), "typed.txt"); !slices.Equal(o.Sources, want) || o.Dest != "dst" {
		t.Errorf("with a typed source: %v -> %q", o.Sources, o.Dest)
	}
	o = mustParse(t, "-t", "dir", "--files-from", list)
	if !slices.Equal(o.Sources, listed) || o.Dest != "dir" {
		t.Errorf("with -t: %v -> %q", o.Sources, o.Dest)
	}
}

func TestFilesFromReadsStandardInput(t *testing.T) {
	saved := stdin
	t.Cleanup(func() { stdin = saved })
	stdin = strings.NewReader("x\ny\n")
	o := mustParse(t, "--files-from=-", "dst")
	if !slices.Equal(o.Sources, []string{"x", "y"}) || o.Dest != "dst" {
		t.Errorf("from stdin: %v -> %q", o.Sources, o.Dest)
	}
}

// A list that cannot be read is a problem with the file, not with the command
// line, so it is reported without pointing at --help.
func TestFilesFromMissingListIsNotAUsageError(t *testing.T) {
	_, err := Parse([]string{"--files-from", filepath.Join(t.TempDir(), "nope"), "dst"})
	if err == nil {
		t.Fatal("a missing list was accepted")
	}
	var ue *UsageError
	if errors.As(err, &ue) {
		t.Errorf("a missing list was reported as a usage error: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot read the file list") {
		t.Errorf("error = %v", err)
	}
}
