package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
