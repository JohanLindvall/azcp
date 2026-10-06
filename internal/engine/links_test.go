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

	"github.com/JohanLindvall/azcp/internal/store/local"
)

func inode(t *testing.T, path string) local.FileID {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	id, _, ok := local.IDOf(path, fi)
	if !ok {
		t.Skip("no file identity on this platform")
	}
	return id
}

func TestArchivePreservesHardLinkedSymlinks(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/target"), "contents")
	if err := os.Symlink("target", filepath.Join(d, "src/a")); err != nil {
		t.Skip(err)
	}
	if err := os.Link(filepath.Join(d, "src/a"), filepath.Join(d, "src/b")); err != nil {
		t.Skip(err)
	}
	if inode(t, filepath.Join(d, "src/a")) != inode(t, filepath.Join(d, "src/b")) {
		t.Skip("platform does not hard-link the symlink itself")
	}
	if n := run(t, d, "-a", "src", "dst"); n != 0 {
		t.Fatal(n)
	}
	if inode(t, filepath.Join(d, "dst/a")) != inode(t, filepath.Join(d, "dst/b")) {
		t.Fatal("archive split hard-linked symlinks")
	}
	if target, err := os.Readlink(filepath.Join(d, "dst/b")); err != nil || target != "target" {
		t.Fatalf("link target = %q, %v", target, err)
	}
}

func TestUpdatePreservesLinksToSkippedDestination(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/a"), "source")
	if err := os.Link(filepath.Join(d, "src/a"), filepath.Join(d, "src/b")); err != nil {
		t.Skip(err)
	}
	write(t, filepath.Join(d, "dst/a"), "newer destination")
	before, err := os.Stat(filepath.Join(d, "dst/a"))
	if err != nil {
		t.Fatal(err)
	}
	when := time.Unix(946684800, 0)
	if err := os.Chtimes(filepath.Join(d, "src/a"), when, when); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "-au", "src/a", "src/b", "dst"); n != 0 {
		t.Fatal(n)
	}
	if inode(t, filepath.Join(d, "dst/a")) != inode(t, filepath.Join(d, "dst/b")) || read(t, filepath.Join(d, "dst/b")) != "newer destination" {
		t.Fatal("-u did not use the retained destination to preserve hard links")
	}
	after, err := os.Stat(filepath.Join(d, "dst/a"))
	if err != nil || !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
		t.Fatalf("linking changed the retained file's attributes: %v, %v", after, err)
	}
}

func TestUpdateLinksTwoSkippedDestinationsWithoutPreserve(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/a"), "source")
	if err := os.Link(filepath.Join(d, "src/a"), filepath.Join(d, "src/b")); err != nil {
		t.Skip(err)
	}
	when := time.Unix(946684800, 0)
	if err := os.Chtimes(filepath.Join(d, "src/a"), when, when); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(d, "dst/a"), "retained first")
	write(t, filepath.Join(d, "dst/b"), "retained second")
	if n := run(t, d, "-ub", "src/a", "src/b", "dst"); n != 0 {
		t.Fatal(n)
	}
	if inode(t, filepath.Join(d, "dst/a")) != inode(t, filepath.Join(d, "dst/b")) || read(t, filepath.Join(d, "dst/b")) != "retained first" {
		t.Fatal("-u did not link two skipped destinations")
	}
	if exists(filepath.Join(d, "dst/b~")) {
		t.Fatal("-u backed up a skipped destination")
	}
}

func TestFailedArchiveCopyDoesNotOverrideUpdateForLaterLink(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/a"), "source")
	if err := os.Link(filepath.Join(d, "src/a"), filepath.Join(d, "src/b")); err != nil {
		t.Skip(err)
	}
	write(t, filepath.Join(d, "dst/a"), "unwritable")
	write(t, filepath.Join(d, "dst/b"), "newer destination")
	if err := os.Chmod(filepath.Join(d, "dst/a"), 0o400); err != nil {
		t.Fatal(err)
	}
	if f, err := os.OpenFile(filepath.Join(d, "dst/a"), os.O_WRONLY, 0); err == nil {
		f.Close()
		t.Skip("caller can write read-only files")
	}
	for path, when := range map[string]int64{"dst/a": 1000000000, "src/a": 1700000000, "dst/b": 1900000000} {
		stamp := time.Unix(when, 0)
		if err := os.Chtimes(filepath.Join(d, path), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if n := run(t, d, "-au", "src/a", "src/b", "dst"); n != 1 {
		t.Fatalf("failed = %d, want 1", n)
	}
	if read(t, filepath.Join(d, "dst/b")) != "newer destination" {
		t.Fatal("a failed earlier copy caused -u to replace a newer destination")
	}
}

func TestArchiveUpdateDryRunCompletesWithLinkedSources(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/a"), "source")
	if err := os.Link(filepath.Join(d, "src/a"), filepath.Join(d, "src/b")); err != nil {
		t.Skip(err)
	}
	if err := os.Mkdir(filepath.Join(d, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(d)
	e := newEngine(t, "-au", "--dry-run", "src/a", "src/b", "dst")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if n, err := e.Run(ctx); n != 0 || err != nil {
		t.Fatalf("dry run waited for work it never started: failures %d, %v", n, err)
	}
	if exists(filepath.Join(d, "dst/a")) || exists(filepath.Join(d, "dst/b")) {
		t.Fatal("dry run created destinations")
	}
}

// Hard-linked files stay linked in the copy even though the two names are
// copied by different workers at the same time. Twenty pairs make the race —
// both names of a pair in flight at once — all but certain if the claim logic
// regresses.
func TestHardLinksSurviveParallelCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	const pairs = 20
	for i := 0; i < pairs; i++ {
		a := filepath.Join(src, fmt.Sprintf("a%02d", i))
		write(t, a, strings.Repeat("x", 4096))
		if err := os.Link(a, filepath.Join(src, fmt.Sprintf("b%02d", i))); err != nil {
			t.Skipf("cannot hard link here: %v", err)
		}
	}
	inode(t, filepath.Join(src, "a00")) // skip early where identity is absent

	if failed := run(t, dir, "-a", "-j", "8", "src", "dst"); failed != 0 {
		t.Fatalf("%d files failed", failed)
	}
	for i := 0; i < pairs; i++ {
		a := filepath.Join(dir, "dst", fmt.Sprintf("a%02d", i))
		b := filepath.Join(dir, "dst", fmt.Sprintf("b%02d", i))
		if inode(t, a) != inode(t, b) {
			t.Fatalf("pair %02d arrived as two separate files", i)
		}
		if got := read(t, b); got != strings.Repeat("x", 4096) {
			t.Fatalf("pair %02d content wrong", i)
		}
	}
}

// A file with one name must not be tracked at all, and a tree mixing linked
// and unlinked files must not cross-link them.
func TestUnlinkedFilesStayUnlinked(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "src", "solo"), "alone")
	write(t, filepath.Join(dir, "src", "first"), "pair")
	if err := os.Link(filepath.Join(dir, "src", "first"), filepath.Join(dir, "src", "second")); err != nil {
		t.Skipf("cannot hard link here: %v", err)
	}
	if failed := run(t, dir, "-a", "src", "dst"); failed != 0 {
		t.Fatalf("%d files failed", failed)
	}
	if inode(t, filepath.Join(dir, "dst", "solo")) == inode(t, filepath.Join(dir, "dst", "first")) {
		t.Fatal("an unrelated file was linked in")
	}
	if inode(t, filepath.Join(dir, "dst", "first")) != inode(t, filepath.Join(dir, "dst", "second")) {
		t.Fatal("the linked pair was split")
	}
}
