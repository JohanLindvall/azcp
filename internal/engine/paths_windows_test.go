package engine

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSelfDirectoryShortNameAliases(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "source directory/file"), "data")
	long, err := filepath.EvalSymlinks(d)
	if err != nil {
		t.Fatal(err)
	}
	p, err := windows.UTF16PtrFromString(long)
	if err != nil {
		t.Fatal(err)
	}
	n, err := windows.GetShortPathName(p, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, n)
	n, err = windows.GetShortPathName(p, &buf[0], uint32(len(buf)))
	if err != nil || n >= uint32(len(buf)) {
		t.Fatalf("GetShortPathName: length = %d, error = %v", n, err)
	}
	short := windows.UTF16ToString(buf[:n])
	if short == long {
		t.Skip("filesystem does not provide short directory names")
	}
	for _, tc := range []struct {
		name, cwd, src, dst string
	}{
		{"short cwd", short, "source directory", filepath.Join(long, "source directory/new")},
		{"long cwd", long, "source directory", filepath.Join(short, "source directory/new")},
		{"absolute source", short, filepath.Join(long, "source directory"), "source directory/new"},
		{"missing parents", short, "source directory", filepath.Join(long, "source directory/new/nested")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Dry-run prevents recursive writes if the safety check regresses.
			if n := run(t, tc.cwd, "--dry-run", "-r", tc.src, tc.dst); n != 1 {
				t.Fatalf("failed = %d, want 1", n)
			}
		})
	}
}
