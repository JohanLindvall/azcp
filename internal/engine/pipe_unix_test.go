// SPDX-License-Identifier: MIT

//go:build unix

package engine

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/gzip"
)

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skipf("cannot make a fifo here: %v", err)
	}
}

// feedFifo writes data into a fifo once something opens it for reading. A
// write that fails shows up as what the reader got, so it is not reported
// here, where the test may already be over.
func feedFifo(path, data string) {
	go func() {
		if f, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
			_, _ = io.WriteString(f, data)
			f.Close()
		}
	}()
}

// drainFifo reads a fifo to its end once something opens it for writing, and
// returns a wait for what it read that fails the test if nothing ever comes.
func drainFifo(t *testing.T, path string) func() []byte {
	type result struct {
		b   []byte
		err error
	}
	got := make(chan result, 1)
	go func() {
		b, err := os.ReadFile(path)
		got <- result{b, err}
	}()
	t.Cleanup(func() {
		// Releases a reader still waiting for a writer that never came.
		if f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
	return func() []byte {
		t.Helper()
		select {
		case r := <-got:
			if r.err != nil {
				t.Fatal(r.err)
			}
			return r.b
		case <-time.After(30 * time.Second):
			t.Fatal("nothing was written into the fifo")
			return nil
		}
	}
}

// cp reads a fifo it is given by name, which is how a pipe — /dev/stdin, or
// <(…) — gets copied. It is read to its end, whatever its size says, and in
// more than one read.
func TestNamedFifoIsReadToItsEnd(t *testing.T) {
	dir := t.TempDir()
	mkfifo(t, filepath.Join(dir, "in"))
	data := strings.Repeat("0123456789", 100_000)
	feedFifo(filepath.Join(dir, "in"), data)
	if failed := run(t, dir, "in", "out"); failed != 0 {
		t.Fatalf("%d failed", failed)
	}
	if got := read(t, filepath.Join(dir, "out")); got != data {
		t.Errorf("copied %d of %d bytes", len(got), len(data))
	}
}

// Under -r cp would recreate a fifo rather than read it. This tool does
// neither, named or met on the way, and carries on with the rest.
func TestRecursiveCopySkipsFifos(t *testing.T) {
	dir := t.TempDir()
	mkfifo(t, filepath.Join(dir, "in"))
	write(t, filepath.Join(dir, "tree", "file.txt"), "kept")
	mkfifo(t, filepath.Join(dir, "tree", "p"))
	if failed := run(t, dir, "-r", "in", "out"); failed != 0 || exists(filepath.Join(dir, "out")) {
		t.Errorf("-r with a fifo: %d failed, created %v", failed, exists(filepath.Join(dir, "out")))
	}
	if failed := run(t, dir, "-r", "tree", "copy"); failed != 0 {
		t.Fatalf("%d failed", failed)
	}
	if read(t, filepath.Join(dir, "copy", "file.txt")) != "kept" || exists(filepath.Join(dir, "copy", "p")) {
		t.Errorf("copied %v", tree(t, filepath.Join(dir, "copy")))
	}
}

// A fifo as the destination is written into, front to back, and stays a fifo
// under its own name: no holes seeked over, and no extension added for
// --compress, since out.gz would be a different file altogether.
func TestFifoDestinationIsWrittenInto(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	mkfifo(t, out)
	content := "head" + strings.Repeat("\x00", 1<<20) + "tail"
	write(t, filepath.Join(dir, "src"), content)

	got := drainFifo(t, out)
	if failed := run(t, dir, "--sparse=always", "src", "out"); failed != 0 {
		t.Fatalf("%d failed", failed)
	}
	if b := got(); string(b) != content {
		t.Errorf("the reader got %d of %d bytes", len(b), len(content))
	}

	got = drainFifo(t, out)
	if failed := run(t, dir, "--compress", "src", "out"); failed != 0 {
		t.Fatalf("--compress: %d failed", failed)
	}
	r, err := gzip.NewReader(bytes.NewReader(got()))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	if b, err := io.ReadAll(r); err != nil || string(b) != content {
		t.Errorf("expanded to %d bytes: %v", len(b), err)
	}
	if exists(out + ".gz") {
		t.Error("--compress wrote out.gz beside the fifo")
	}
	if info, err := os.Stat(out); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("the destination is no longer a fifo: %v", err)
	}
}

// A fifo compressed on the way is stored under the extension, like a file.
func TestFifoSourceCompresses(t *testing.T) {
	dir := t.TempDir()
	mkfifo(t, filepath.Join(dir, "in"))
	feedFifo(filepath.Join(dir, "in"), "squeeze me")
	if failed := run(t, dir, "--compress", "in", "out"); failed != 0 {
		t.Fatalf("%d failed", failed)
	}
	f, err := os.Open(filepath.Join(dir, "out.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(r); err != nil || string(b) != "squeeze me" {
		t.Errorf("expanded to %q: %v", b, err)
	}
}
