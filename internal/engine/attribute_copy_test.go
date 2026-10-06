// SPDX-License-Identifier: MIT

package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/store/local"
)

func TestPreserveThroughDestinationSymlink(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src"), "new")
	write(t, filepath.Join(d, "target"), "old")
	link := filepath.Join(d, "dst")
	if err := os.Symlink("target", link); err != nil {
		t.Skip(err)
	}
	when := time.Unix(946684800, 0)
	if err := os.Chtimes(filepath.Join(d, "src"), when, when); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "-p", "src", "dst"); n != 0 {
		t.Fatal(n)
	}
	target, err := os.Stat(filepath.Join(d, "target"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if !target.ModTime().Equal(when) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("target mtime = %v, link mtime = %v (was %v)", target.ModTime(), after.ModTime(), before.ModTime())
	}
}

func TestPreserveUsesAccessTimeBeforeReading(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	write(t, src, "source data")
	when := time.Unix(946684800, 0)
	if err := os.Chtimes(src, when, when); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "--reflink=never", "--preserve=timestamps", "src", "dst"); n != 0 {
		t.Fatal(n)
	}
	info, err := os.Stat(filepath.Join(d, "dst"))
	if err != nil {
		t.Fatal(err)
	}
	if got := local.AccessTimeOf(info); !got.Equal(when) {
		t.Fatalf("atime = %v, want the original %v", got, when)
	}
}

func TestParentsPreservesIntermediateDirectoryAttributes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions")
	}
	d := t.TempDir()
	write(t, filepath.Join(d, "src/sub/file"), "data")
	if err := os.Mkdir(filepath.Join(d, "dst"), 0o700); err != nil {
		t.Fatal(err)
	}
	when := time.Unix(946684800, 0)
	for _, name := range []string{"src", "src/sub"} {
		if err := os.Chmod(filepath.Join(d, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(d, name), when, when); err != nil {
			t.Fatal(err)
		}
	}
	if n := run(t, d, "-p", "--parents", "src/sub/file", "dst"); n != 0 {
		t.Fatal(n)
	}
	for _, name := range []string{"dst/src", "dst/src/sub"} {
		info, err := os.Stat(filepath.Join(d, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 || !info.ModTime().Equal(when) {
			t.Fatalf("%s: mode %o mtime %v", name, info.Mode().Perm(), info.ModTime())
		}
	}
}

func TestAttributesOnlyLeavesExistingSymlinkDestination(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "target"), "source")
	write(t, filepath.Join(d, "dst"), "keep")
	if err := os.Symlink("target", filepath.Join(d, "src")); err != nil {
		t.Skip(err)
	}
	if n := run(t, d, "-P", "--attributes-only", "src", "dst"); n != 1 {
		t.Fatalf("failed = %d", n)
	}
	if got := read(t, filepath.Join(d, "dst")); got != "keep" {
		t.Fatal("attributes-only replaced the destination's contents")
	}
}

func TestNoPreserveModeUsesDefaultCreationMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	d := t.TempDir()
	write(t, filepath.Join(d, "src"), "source")
	if err := os.Chmod(filepath.Join(d, "src"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "reference"), nil, 0o666); err != nil {
		t.Fatal(err)
	}
	want, _ := os.Stat(filepath.Join(d, "reference"))
	if n := run(t, d, "--no-preserve=mode", "src", "dst"); n != 0 {
		t.Fatal(n)
	}
	got, err := os.Stat(filepath.Join(d, "dst"))
	if err != nil || got.Mode().Perm() != want.Mode().Perm() {
		t.Fatalf("default creation permissions were not used: %v, %v", got, err)
	}
}

func TestParentsPreservesExplicitDotDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions")
	}
	d := t.TempDir()
	write(t, filepath.Join(d, "src/file"), "contents")
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(d, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "-a", "--parents", "./src/file", "dst"); n != 0 {
		t.Fatal(n)
	}
	info, err := os.Stat(filepath.Join(d, "dst"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("explicit dot's permissions were lost: %v, %v", info, err)
	}
}

func TestParentsFollowsIntermediateDirectorySymlinks(t *testing.T) {
	for _, root := range []bool{false, true} {
		t.Run(map[bool]string{false: "intermediate", true: "root"}[root], func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "src/file"), "contents")
			if err := os.Mkdir(filepath.Join(d, "real"), 0o755); err != nil {
				t.Fatal(err)
			}
			link, target, copied := "dst", "real", "real/src/file"
			if !root {
				if err := os.Mkdir(filepath.Join(d, "dst"), 0o755); err != nil {
					t.Fatal(err)
				}
				link, target, copied = "dst/src", "../real", "real/file"
			}
			if err := os.Symlink(target, filepath.Join(d, link)); err != nil {
				t.Skip(err)
			}
			if n := run(t, d, "-a", "--parents", "./src/file", "dst"); n != 0 {
				t.Fatal(n)
			}
			if read(t, filepath.Join(d, copied)) != "contents" {
				t.Fatal("file did not land through the intermediate directory")
			}
			// Windows reports the target with its own separators; the link is
			// the same one.
			if got, err := os.Readlink(filepath.Join(d, link)); err != nil || filepath.ToSlash(got) != target {
				t.Fatalf("intermediate link changed: %q, %v", got, err)
			}
		})
	}
}

func TestParentsLeavesCompleteExistingParentAttributes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions")
	}
	d := t.TempDir()
	write(t, filepath.Join(d, "src/file"), "contents")
	if err := os.MkdirAll(filepath.Join(d, "dst/src"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{"src": 0o700, "dst": 0o755, "dst/src": 0o750} {
		if err := os.Chmod(filepath.Join(d, path), mode); err != nil {
			t.Fatal(err)
		}
	}
	if n := run(t, d, "-a", "--parents", "./src/file", "dst"); n != 0 {
		t.Fatal(n)
	}
	for path, mode := range map[string]os.FileMode{"dst": 0o755, "dst/src": 0o750} {
		info, err := os.Stat(filepath.Join(d, path))
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("existing parent %s lost its mode: %v, %v", path, info, err)
		}
	}
}
