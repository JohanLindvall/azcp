package local

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCopyFilePlain(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	data := bytes.Repeat([]byte("payload"), 100_000)
	writeFile(t, src, data)

	var reported int64
	n, err := CopyFile(context.Background(), src, dst, CopyOptions{
		Reflink:  ReflinkNever,
		Progress: func(n int64) { reported += n },
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(data)) || reported != n {
		t.Errorf("wrote %d, reported %d, want %d", n, reported, len(data))
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("content mismatch (%v)", err)
	}
}

func TestCopyFileExclRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	writeFile(t, src, []byte("new"))
	writeFile(t, dst, []byte("old"))
	if _, err := CopyFile(context.Background(), src, dst, CopyOptions{Excl: true}); err == nil {
		t.Fatal("Excl overwrote an existing file")
	}
	if got, _ := os.ReadFile(dst); string(got) != "old" {
		t.Errorf("destination altered: %q", got)
	}
}

func TestCopyFileSparsePreservesContentAndLength(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	// A megabyte of zeros with a small island of data, plus a trailing hole.
	data := make([]byte, 1<<20)
	copy(data[512*1024:], []byte("island"))
	writeFile(t, src, data)

	n, err := CopyFile(context.Background(), src, dst, CopyOptions{
		Reflink: ReflinkNever,
		Sparse:  SparseAlways,
		BufSize: 64 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(data)) {
		t.Errorf("reported %d bytes, want %d", n, len(data))
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("sparse copy corrupted the data (%v)", err)
	}
}

func TestCopyFileCancelled(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	writeFile(t, src, bytes.Repeat([]byte("x"), 1<<20))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CopyFile(ctx, src, dst, CopyOptions{Reflink: ReflinkNever}); err == nil {
		t.Fatal("a cancelled context copied anyway")
	}
}

func TestCopySymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("target/elsewhere", filepath.Join(dir, "link")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if err := CopySymlink(filepath.Join(dir, "link"), filepath.Join(dir, "copy")); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join(dir, "copy"))
	if err != nil || got != "target/elsewhere" {
		t.Errorf("link points at %q (%v)", got, err)
	}
}

func TestApplyAttrsModeAndTimes(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	writeFile(t, src, []byte("s"))
	writeFile(t, dst, []byte("d"))
	if err := os.Chmod(src, 0o750); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2020, 4, 5, 6, 7, 8, 0, time.UTC)
	if err := os.Chtimes(src, when, when); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	if errs := ApplyAttrs(src, dst, fi, Preserve{Mode: true, Timestamps: true}, false); len(errs) != 0 {
		t.Fatalf("ApplyAttrs: %v", errs)
	}
	di, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o750 {
		t.Errorf("mode %v", di.Mode())
	}
	if !di.ModTime().Equal(when) {
		t.Errorf("mtime %v, want %v", di.ModTime(), when)
	}
}

// A symlink met while following links may point back up its own tree. The walk
// must visit everything exactly once — the root included, which an earlier
// version forgot to record, walking the whole tree twice.
func TestWalkAllFollowLinkToRootVisitsOnce(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "file"), []byte("x"))
	if err := os.Symlink(root, filepath.Join(root, "sub", "back")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}

	s := New(slog.New(slog.DiscardHandler), true)
	u, err := uri.Parse(root, uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	err = s.WalkAll(context.Background(), u, nil, func(n *store.Node) error {
		counts[n.URL.Path]++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range counts {
		if c != 1 {
			t.Errorf("%s visited %d times", p, c)
		}
	}
	if len(counts) != 3 { // sub, file, sub/back
		t.Errorf("visited %d entries, want 3: %v", len(counts), counts)
	}
}
