package engine

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A backslash is an ordinary character in a Unix file name. The expansion is
// written beside the destination, and "beside" has to be worked out the way
// the platform spells paths, or the temporary file lands in a directory that
// does not exist.
func TestDecompressFileNameWithBackslash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a backslash is a separator here")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, `odd\name.txt.gz`)
	want := payload()
	if err := os.WriteFile(path, compress(t, "gzip", want), 0o644); err != nil {
		t.Fatal(err)
	}

	final, err := decompressFile(path, "gzip")
	if err != nil {
		t.Fatal(err)
	}
	if final != filepath.Join(dir, `odd\name.txt`) {
		t.Errorf("expanded to %q", final)
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the expanded content differs from the original")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the compressed original was left behind")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the directory holds %d entries; a temporary file was left behind", len(entries))
	}
}
