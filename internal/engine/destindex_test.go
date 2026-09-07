package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/azure"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// newEngine builds an engine for the options given, without running anything.
func newEngine(t *testing.T, argv ...string) *Engine {
	t.Helper()
	opt, err := cli.Parse(argv)
	if err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	e, err := New(Config{
		Options:  opt,
		Log:      slog.New(slog.DiscardHandler),
		Progress: progress.New(progress.Config{Mode: progress.ModeNever}),
		Stdin:    strings.NewReader(""),
	})
	if err != nil {
		t.Fatalf("new engine %v: %v", argv, err)
	}
	return e
}

// The index answers from a directory listing what a stat used to answer, and
// the two are not allowed to disagree: every option that reaches the overwrite
// rules has to decide the same way whether or not the index could speak.
//
// The stat is the specification here, being what cp's behaviour was checked
// against; the index is only ever a cheaper route to the same answer.
func TestIndexAgreesWithStat(t *testing.T) {
	flagsets := [][]string{
		{"-n"},
		{"--update=none"},
		{"--update=none-fail"},
		{"-u"},
		{"-n", "-u"},
		{"--backup=simple"},
	}
	for _, argv := range flagsets {
		for _, present := range []bool{false, true} {
			for _, partial := range []bool{false, true} {
				for _, remote := range []bool{false, true} {
					name := fmt.Sprintf("%s/present=%v/partial=%v/remote=%v",
						strings.Join(argv, " "), present, partial, remote)
					t.Run(name, func(t *testing.T) {
						assertSameDecision(t, argv, present, partial, remote)
					})
				}
			}
		}
	}
}

func assertSameDecision(t *testing.T, argv []string, present, partial, remoteSrc bool) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.bin")
	if present {
		write(t, path, "what is already there")
	}
	if partial {
		write(t, path+azure.ResumeSuffix, "azcp-resume 1 etag 100 8388608\n0\n")
	}

	dst, err := uri.Parse(path, uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	srcArg := filepath.Join(dir, "source.bin")
	if remoteSrc {
		srcArg = "azure://acct/container/file.bin"
	}
	srcURL, err := uri.Parse(srcArg, uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Older than the destination, so -u has something to decide on.
	src := &store.Node{URL: srcURL, Size: 100, ModTime: time.Now().Add(-time.Hour)}

	e := newEngine(t, append(append([]string{}, argv...), "src", "dst")...)
	if !e.needsDestCheck() {
		t.Fatalf("%v does not check the destination; nothing to compare", argv)
	}
	ctx := context.Background()
	proceed, backup, err := e.weighDestination(ctx, src, dst)
	// Without this the comparison could pass by never taking the fast path.
	if used := e.destIdx.reads > 0; used != e.existenceDecides() {
		t.Fatalf("index consulted = %v, want %v for %v", used, e.existenceDecides(), argv)
	}

	// The same question with the index unable to answer, which is the route
	// every one of these options took before it existed.
	e.destIdx = nil
	wantProceed, wantBackup, wantErr := e.weighDestination(ctx, src, dst)

	if proceed != wantProceed {
		t.Errorf("proceed = %v with the index, %v with a stat", proceed, wantProceed)
	}
	if (backup == "") != (wantBackup == "") {
		t.Errorf("backup = %q with the index, %q with a stat", backup, wantBackup)
	}
	if fmt.Sprint(err) != fmt.Sprint(wantErr) {
		t.Errorf("error = %v with the index, %v with a stat", err, wantErr)
	}
}

// The point of the index is the request economy: one listing per destination
// directory, not a stat per file in it. The README's claim about a rerun rests
// on this, so it is pinned here rather than left to a profiler.
func TestRerunListsEachDirectoryOnce(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"one", "two"} {
		for i := range 20 {
			write(t, filepath.Join(dir, "src", sub, fmt.Sprintf("f%02d", i)), "x")
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "-r", "src", "dst")

	// The rerun: everything is already there, and -n has to establish that.
	e := newEngine(t, "-r", "-n", "src", "dst")
	t.Chdir(dir)
	if failed, err := e.Run(context.Background()); err != nil || failed != 0 {
		t.Fatalf("rerun: failed=%d err=%v", failed, err)
	}
	// dst/src/one and dst/src/two, the two directories holding files. Forty
	// questions, two listings.
	if e.destIdx.reads != 2 {
		t.Errorf("listed the destination %d times, want 2 — one per directory",
			e.destIdx.reads)
	}
	if _, _, skipped, _, _, _ := e.prog.Totals(); skipped != 40 {
		t.Errorf("skipped %d files, want the 40 already there", skipped)
	}
}

// Two sources holding the same relative path are what -n is for: the first one
// provides the file and the second leaves it alone. The index has to see the
// copy the scanner has just queued, or both would be written — and with
// --resume, which does not open the destination exclusively, both at once.
func TestOverlappingSourcesRespectNoClobber(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a", "shared.txt"), "from a")
	write(t, filepath.Join(dir, "b", "shared.txt"), "from b")
	if err := os.MkdirAll(filepath.Join(dir, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "-r", "-n", "--no-target-directory", "a", "dst")
	run(t, dir, "-r", "-n", "--no-target-directory", "b", "dst")
	if got := read(t, filepath.Join(dir, "dst", "shared.txt")); got != "from a" {
		t.Errorf("second source overwrote the first: %q", got)
	}
}

// A directory read once is remembered, and one read for a directory the copy
// has not created yet is not a failure: nothing is there, which is the answer.
func TestIndexReadsOnceAndToleratesMissing(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "here"), "x")
	idx := newDestIndex()

	present, err := uri.Parse(filepath.Join(dir, "here"), uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	absent, err := uri.Parse(filepath.Join(dir, "gone"), uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if exists, _, ok := idx.lookup(present); !ok || !exists {
		t.Errorf("lookup of an existing file: exists=%v ok=%v", exists, ok)
	}
	if exists, _, ok := idx.lookup(absent); !ok || exists {
		t.Errorf("lookup of a missing file: exists=%v ok=%v", exists, ok)
	}
	if idx.reads != 1 {
		t.Errorf("read the directory %d times for two lookups, want 1", idx.reads)
	}

	missingDir, err := uri.Parse(filepath.Join(dir, "not-yet", "f"), uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if exists, _, ok := idx.lookup(missingDir); !ok || exists {
		t.Errorf("lookup under a directory that is not there: exists=%v ok=%v", exists, ok)
	}
}

// Eviction bounds the memory, and a directory dropped and asked about again
// must answer the same way — from a fresh listing, not from nothing.
func TestIndexEvictsAndStaysCorrect(t *testing.T) {
	dir := t.TempDir()
	dirs := maxIndexDirs + 4
	for i := range dirs {
		write(t, filepath.Join(dir, fmt.Sprintf("d%02d", i), "f"), "x")
	}
	idx := newDestIndex()
	for i := range dirs {
		u, err := uri.Parse(filepath.Join(dir, fmt.Sprintf("d%02d", i), "f"), uri.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if exists, _, ok := idx.lookup(u); !ok || !exists {
			t.Fatalf("d%02d/f: exists=%v ok=%v", i, exists, ok)
		}
	}
	if len(idx.dirs) > maxIndexDirs {
		t.Errorf("holding %d directories, want at most %d", len(idx.dirs), maxIndexDirs)
	}
	if len(idx.dirs) != len(idx.lru) {
		t.Errorf("%d directories cached but %d in the eviction order",
			len(idx.dirs), len(idx.lru))
	}
	// The first one was evicted; asking again reads it afresh and still finds it.
	first, err := uri.Parse(filepath.Join(dir, "d00", "f"), uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	before := idx.reads
	if exists, _, ok := idx.lookup(first); !ok || !exists {
		t.Errorf("after eviction: exists=%v ok=%v", exists, ok)
	}
	if idx.reads != before+1 {
		t.Errorf("reads = %d, want one more than %d: the directory was evicted",
			idx.reads, before)
	}
}
