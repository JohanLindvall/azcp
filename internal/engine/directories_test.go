package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRecursiveDirectoryPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions")
	}
	for _, mode := range []os.FileMode{0o700, 0o750, 0o500} {
		for _, preserve := range []bool{false, true} {
			t.Run(fmt.Sprintf("%o/preserve=%t", mode, preserve), func(t *testing.T) {
				d := t.TempDir()
				write(t, filepath.Join(d, "src/sub/file"), "contents")
				for _, path := range []string{"src", "src/sub"} {
					if err := os.Chmod(filepath.Join(d, path), mode); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() {
					for _, path := range []string{"src", "src/sub", "dst", "dst/sub"} {
						_ = os.Chmod(filepath.Join(d, path), 0o700)
					}
				})
				args := []string{"-r", "src", "dst"}
				if preserve {
					args = append(args, "--preserve=mode")
				}
				if n := run(t, d, args...); n != 0 {
					t.Fatal(n)
				}
				for _, path := range []string{"dst", "dst/sub"} {
					info, err := os.Stat(filepath.Join(d, path))
					if err != nil {
						t.Fatal(err)
					}
					if got := info.Mode().Perm(); got != mode {
						t.Errorf("%s mode = %o, want %o", path, got, mode)
					}
				}
				if got := read(t, filepath.Join(d, "dst/sub/file")); got != "contents" {
					t.Fatal("directory contents were not copied")
				}
			})
		}
	}
}

func TestRecursiveCopyRequiresDestinationParent(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/file"), "contents")
	if n := run(t, d, "-r", "src", "missing/dst"); n != 1 {
		t.Fatalf("failed = %d, want 1", n)
	}
	if exists(filepath.Join(d, "missing")) {
		t.Fatal("created an unrequested parent directory")
	}
}
