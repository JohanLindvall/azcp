// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHardLinkFollowsSourceUnlessNoDereference(t *testing.T) {
	for _, flag := range []string{"-l", "-lP"} {
		t.Run(flag, func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "source"), "contents")
			if err := os.Symlink("source", filepath.Join(d, "link")); err != nil {
				t.Skip(err)
			}
			if n := run(t, d, flag, "link", "copy"); n != 0 {
				t.Fatal(n)
			}
			want := "source"
			if flag == "-lP" {
				want = "link"
			}
			if inode(t, filepath.Join(d, want)) != inode(t, filepath.Join(d, "copy")) {
				t.Fatal("linked the wrong source inode")
			}
		})
	}
}

func TestAliasedSourcesAndDestinationsKeepOperandOrder(t *testing.T) {
	for _, readFirst := range []bool{false, true} {
		for _, symlink := range []bool{false, true} {
			t.Run(fmt.Sprintf("readFirst=%v/symlink=%v", readFirst, symlink), func(t *testing.T) {
				d := t.TempDir()
				first := strings.Repeat("first", 1<<20)
				write(t, filepath.Join(d, "src/a"), first)
				write(t, filepath.Join(d, "src/b"), "last")
				if err := os.Mkdir(filepath.Join(d, "dst"), 0o755); err != nil {
					t.Fatal(err)
				}
				source, dest := "src/b", "dst/a"
				if readFirst {
					source, dest = "src/a", "dst/b"
				}
				link := os.Link
				if symlink {
					link = os.Symlink
				}
				if err := link(filepath.Join(d, source), filepath.Join(d, dest)); err != nil {
					t.Skip(err)
				}
				if n := run(t, d, "--reflink=never", "--sparse=never", "-j8", "src/a", "src/b", "dst"); n != 0 {
					t.Fatal(n)
				}
				if read(t, filepath.Join(d, "dst/a")) != first {
					t.Fatal("a source was changed before its earlier copy finished reading")
				}
				wantLast := first
				if readFirst {
					wantLast = "last"
				}
				if read(t, filepath.Join(d, "dst/b")) != wantLast {
					t.Fatal("a source was read before its earlier writer finished")
				}
			})
		}
	}
}

func TestUpdateWeighsTheDestinationSymlinkTarget(t *testing.T) {
	for _, newer := range []bool{false, true} {
		t.Run(fmt.Sprint(newer), func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "src"), "new")
			write(t, filepath.Join(d, "target"), "old")
			if err := os.Symlink("target", filepath.Join(d, "dst")); err != nil {
				t.Skip(err)
			}
			mtime := time.Unix(1700000000, 0)
			if err := os.Chtimes(filepath.Join(d, "src"), mtime, mtime); err != nil {
				t.Fatal(err)
			}
			delta := -time.Hour
			if newer {
				delta = time.Hour
			}
			if err := os.Chtimes(filepath.Join(d, "target"), mtime.Add(delta), mtime.Add(delta)); err != nil {
				t.Fatal(err)
			}
			if n := run(t, d, "-u", "src", "dst"); n != 0 {
				t.Fatal(n)
			}
			want := "new"
			if newer {
				want = "old"
			}
			if read(t, filepath.Join(d, "target")) != want {
				t.Fatal("-u weighed the link's mtime")
			}
		})
	}
}

func TestPOSIXDanglingDestinationIsMissingUnderOverwriteOptions(t *testing.T) {
	t.Setenv("POSIXLY_CORRECT", "1")
	for _, flag := range []string{"-n", "--update=none", "-u", "-i"} {
		t.Run(flag, func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "src"), "new")
			if err := os.Symlink("target", filepath.Join(d, "dst")); err != nil {
				t.Skip(err)
			}
			if n := runWithStdin(t, d, strings.NewReader("n\n"), flag, "src", "dst"); n != 0 {
				t.Fatal(n)
			}
			if read(t, filepath.Join(d, "target")) != "new" {
				t.Fatal("a dangling link's target was not created")
			}
		})
	}
}

func TestInteractiveHardLinkReplacesAnAcceptedDestination(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "source"), "new")
	write(t, filepath.Join(d, "copy"), "old")
	if n := runWithStdin(t, d, strings.NewReader("y\n"), "-li", "source", "copy"); n != 0 {
		t.Fatal(n)
	}
	if inode(t, filepath.Join(d, "source")) != inode(t, filepath.Join(d, "copy")) {
		t.Fatal("accepted destination was not linked")
	}
}

func TestIncludeFollowsDirectorySymlinks(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "real", "file.txt"), "included")
	write(t, filepath.Join(d, "real", "file.bin"), "excluded")
	if err := os.Mkdir(filepath.Join(d, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(d, "real"), filepath.Join(d, "src", "linked")); err != nil {
		t.Skip(err)
	}
	if n := run(t, d, "-rL", "--include=*.txt", "src", "dst"); n != 0 {
		t.Fatal(n)
	}
	if read(t, filepath.Join(d, "dst", "linked", "file.txt")) != "included" || exists(filepath.Join(d, "dst", "linked", "file.bin")) {
		t.Fatal("file filter was applied to a directory link")
	}
}

func TestDeleteRetainsParentsOfExcludedFiles(t *testing.T) {
	for _, dry := range []bool{false, true} {
		t.Run(map[bool]string{false: "real", true: "dry"}[dry], func(t *testing.T) {
			d := t.TempDir()
			t.Chdir(d)
			if err := os.Mkdir("src", 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, "dst/extra/deep/protected.keep", "keep")
			write(t, "dst/extra/remove", "remove")
			args := []string{"-rT", "--delete", "--exclude=*.keep", "src", "dst"}
			if dry {
				args = append([]string{"--dry-run"}, args...)
			}
			e := newEngine(t, args...)
			if n, err := e.Run(context.Background()); err != nil || n != 0 {
				t.Fatalf("run: %d, %v", n, err)
			}
			if e.Deleted() != 1 {
				t.Fatalf("deletions = %d, want only the unprotected file", e.Deleted())
			}
			if read(t, "dst/extra/deep/protected.keep") != "keep" {
				t.Fatal("excluded file changed")
			}
			if exists("dst/extra/remove") != dry {
				t.Fatal("dry-run changed deletion behavior")
			}
		})
	}
}

func TestNumberedBackupBeyondMachineInteger(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src"), "new")
	write(t, filepath.Join(d, "dst"), "old")
	write(t, filepath.Join(d, "dst.~9999999999999999999999999~"), "older")
	if n := run(t, d, "--backup=numbered", "src", "dst"); n != 0 {
		t.Fatal(n)
	}
	if read(t, filepath.Join(d, "dst.~10000000000000000000000000~")) != "old" {
		t.Fatal("backup sequence wrapped")
	}
}

func TestDiagnosticNamesCannotControlTheTerminal(t *testing.T) {
	for name, want := range map[string]string{
		"a\nb": "'a'$'\\n''b'", "a\x1bb": "'a'$'\\033''b'", "a'b": `"a'b"`, "a'b\"c": "'a'\\''b\"c'",
	} {
		if got := quote(name); got != want {
			t.Errorf("quote(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestAliasedDestinationsKeepOperandOrder(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "hardlink", true: "symlink"}[symlink], func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "src/a"), strings.Repeat("first", 1<<20))
			write(t, filepath.Join(d, "src/b"), "last")
			write(t, filepath.Join(d, "dst/a"), "original")
			link := os.Link
			if symlink {
				link = os.Symlink
			}
			if err := link(filepath.Join(d, "dst/a"), filepath.Join(d, "dst/b")); err != nil {
				t.Skip(err)
			}
			if n := run(t, d, "--reflink=never", "--sparse=never", "-j8", "src/a", "src/b", "dst"); n != 0 {
				t.Fatal(n)
			}
			for _, name := range []string{"a", "b"} {
				if got := read(t, filepath.Join(d, "dst", name)); got != "last" {
					t.Fatalf("%s contains %d bytes of mixed or out-of-order output", name, len(got))
				}
			}
		})
	}
}

func TestPreserveLinksSeparatesUnrelatedDestinationLinks(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/a"), "first")
	write(t, filepath.Join(d, "src/b"), "second")
	write(t, filepath.Join(d, "dst/a"), "original")
	if err := os.Link(filepath.Join(d, "dst/a"), filepath.Join(d, "dst/b")); err != nil {
		t.Skip(err)
	}
	if n := run(t, d, "-a", "src/a", "src/b", "dst"); n != 0 {
		t.Fatal(n)
	}
	if read(t, filepath.Join(d, "dst/a")) != "first" || read(t, filepath.Join(d, "dst/b")) != "second" {
		t.Fatal("unrelated source files were merged by destination hard links")
	}
	if inode(t, filepath.Join(d, "dst/a")) == inode(t, filepath.Join(d, "dst/b")) {
		t.Fatal("destination names remain linked")
	}
}

func TestPreserveLinksDoesNotAllowCopyingOntoTheSource(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src"), "data")
	if err := os.Link(filepath.Join(d, "src"), filepath.Join(d, "dst")); err != nil {
		t.Skip(err)
	}
	if n := run(t, d, "-a", "src", "dst"); n != 1 {
		t.Fatalf("failures = %d, want same-file failure", n)
	}
	if inode(t, filepath.Join(d, "src")) != inode(t, filepath.Join(d, "dst")) {
		t.Fatal("same-file rejection broke a link")
	}
}

func TestUpdateStillRejectsTheSameFile(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src"), "contents")
	if err := os.Link(filepath.Join(d, "src"), filepath.Join(d, "dst")); err != nil {
		t.Skip(err)
	}
	if n := run(t, d, "-u", "src", "dst"); n != 1 {
		t.Fatalf("failures = %d, want same-file failure before comparing timestamps", n)
	}
}

func TestRecursiveHardLinkDereferencesByDefault(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/file"), "contents")
	if err := os.Symlink("file", filepath.Join(d, "src/link")); err != nil {
		t.Skip(err)
	}
	if n := run(t, d, "-rl", "src", "dst"); n != 0 {
		t.Fatal(n)
	}
	if inode(t, filepath.Join(d, "src/file")) != inode(t, filepath.Join(d, "dst/link")) {
		t.Fatal("recursive -l linked the source symlink instead of its target")
	}
}

func TestHardLinkNoDereferenceReplacesExistingFile(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "target"), "contents")
	write(t, filepath.Join(d, "dst"), "old")
	if err := os.Symlink("target", filepath.Join(d, "src")); err != nil {
		t.Skip(err)
	}
	if n := run(t, d, "-lP", "src", "dst"); n != 0 {
		t.Fatal(n)
	}
	if inode(t, filepath.Join(d, "src")) != inode(t, filepath.Join(d, "dst")) {
		t.Fatal("did not hard-link the source symlink")
	}
}
