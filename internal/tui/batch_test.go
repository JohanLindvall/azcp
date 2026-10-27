// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/logx"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

func batchTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "old", "f.log"), "old")
	write(t, filepath.Join(root, "new", "clip.mp4"), "new contents")
	write(t, filepath.Join(root, "top.txt"), "t")
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	past := time.Now().AddDate(-2, 0, 0)
	if err := os.Chtimes(filepath.Join(root, "old", "f.log"), past, past); err != nil {
		t.Fatal(err)
	}
	return root
}

func batchOptions(root, filter string, args ...string) *cli.Options {
	o := cli.Defaults()
	o.Select, o.Sources = filter, append([]string{root}, args...)
	return o
}

func TestSelectRewritesSourcesToWhatTheFilterPicks(t *testing.T) {
	root := batchTree(t)
	log := slog.New(slog.DiscardHandler)
	for _, tc := range []struct{ filter, want string }{
		{"type:dir", "empty new old"},
		{"has:*.mp4", "new"},
		{"size>5", "new"},
		{"*.txt", "top.txt"},
		{"type:dir -has:*.mp4", "empty old"},
		{"has=clip.mp4", "new"},
		{"has=clip", ""},
		{"has:clip has=clip.mp4", "new"},
		{"name=top.txt", "top.txt"},
		{"name=top", ""},
		// An empty directory has no age, so a batch does not take it for old.
		{"older:365d", "old"},
		{"newer:1d", "new top.txt"},
	} {
		o := batchOptions(root, tc.filter)
		if err := Select(t.Context(), o, log); err != nil {
			t.Fatalf("%q: %v", tc.filter, err)
		}
		var got []string
		for _, s := range o.Sources {
			got = append(got, filepath.Base(s))
		}
		slices.Sort(got)
		if want := strings.Fields(tc.want); !slices.Equal(got, want) {
			t.Errorf("%q selected %v, want %v", tc.filter, got, want)
		}
		if o.Glob != cli.GlobNever {
			t.Errorf("%q: selected names would be expanded again", tc.filter)
		}
	}
	if err := Select(t.Context(), batchOptions(filepath.Join(root, "top.txt"), "x"), log); err == nil {
		t.Error("selected from a file")
	}
}

func TestRemoveListsUnlessConfirmed(t *testing.T) {
	root := batchTree(t)
	log := slog.New(slog.DiscardHandler)
	o := batchOptions(root, "older:365d")
	o.Remove, o.Recursive = true, true
	if failed, err := Remove(t.Context(), o, log); err != nil || failed != 0 {
		t.Fatalf("dry run: %d, %v", failed, err)
	}
	read(t, filepath.Join(root, "old", "f.log"), "old")

	o.Confirmed = true
	o.DryRun = true
	if _, err := Remove(t.Context(), o, log); err != nil {
		t.Fatal(err)
	}
	read(t, filepath.Join(root, "old", "f.log"), "old")

	o.DryRun = false
	if failed, err := Remove(t.Context(), o, log); err != nil || failed != 0 {
		t.Fatalf("remove: %d, %v", failed, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "old")); !os.IsNotExist(err) {
		t.Fatalf("old survived: %v", err)
	}
	for _, kept := range []string{"new/clip.mp4", "top.txt", "empty"} {
		if _, err := os.Lstat(filepath.Join(root, kept)); err != nil {
			t.Errorf("%s: %v", kept, err)
		}
	}
}

func TestRemoveNeedsRecursionForADirectory(t *testing.T) {
	root := batchTree(t)
	log := slog.New(slog.DiscardHandler)
	o := cli.Defaults()
	o.Remove, o.Confirmed = true, true
	o.Sources = []string{filepath.Join(root, "new"), filepath.Join(root, "top.txt")}
	if failed, err := Remove(t.Context(), o, log); err != nil || failed != 1 {
		t.Fatalf("got %d failures, %v; want the directory refused", failed, err)
	}
	read(t, filepath.Join(root, "new", "clip.mp4"), "new contents")
	if _, err := os.Lstat(filepath.Join(root, "top.txt")); !os.IsNotExist(err) {
		t.Fatalf("the file beside it was not removed: %v", err)
	}
	o.Sources = []string{filepath.Join(root, "missing")}
	if _, err := Remove(t.Context(), o, log); err == nil {
		t.Fatal("removed something that is not there")
	}
}

// output collects what a run writes for the terminal, a line at a time.
func output(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	logx.SetTerminalSink(func(s string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, strings.Split(strings.TrimSuffix(s, "\n"), "\n")...)
	})
	t.Cleanup(func() { logx.SetTerminalSink(nil) })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(lines)
	}
}

func TestRemoveRefusesDotOperandsAndTheEmptyName(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a", "b", "c", "f"), "keep")
	// Inside the temporary tree, so a regression removes nothing else.
	t.Chdir(filepath.Join(root, "a", "b"))
	log := slog.New(slog.DiscardHandler)
	o := cli.Defaults()
	o.Remove, o.Recursive, o.Confirmed = true, true, true
	o.Sources = []string{".", "..", "c/.", "c/..", "c/./", "./"}
	if runtime.GOOS == "windows" {
		o.Sources = append(o.Sources, filepath.VolumeName(root))
	}
	lines := output(t)
	failed, err := Remove(t.Context(), o, log)
	if err != nil || failed != len(o.Sources) {
		t.Fatalf("got %d failures, %v; want every operand refused", failed, err)
	}
	read(t, filepath.Join(root, "a", "b", "c", "f"), "keep")
	if got := lines(); !slices.Contains(got, "azcp: refusing to remove '.' or '..' directory: skipping '..'") {
		t.Errorf("refusal not reported as rm reports it: %q", got)
	}
	o.Sources = []string{""}
	if _, err := Remove(t.Context(), o, log); err == nil || !strings.Contains(err.Error(), "cannot remove '': No such file or directory") {
		t.Fatalf("the empty name: %v", err)
	}
	o.Select, o.Sources = "*", []string{""}
	if _, err := Remove(t.Context(), o, log); err == nil {
		t.Fatal("selected from the empty name")
	}
	read(t, filepath.Join(root, "a", "b", "c", "f"), "keep")
}

// hooked lets a test change the tree at the moments a race would.
type hooked struct {
	store.Store
	beforeStat, afterWalk func(path string)
}

func (s *hooked) Stat(ctx context.Context, u *uri.URL, follow bool) (*store.Node, error) {
	if s.beforeStat != nil {
		s.beforeStat(u.Path)
	}
	return s.Store.Stat(ctx, u, follow)
}

func (s *hooked) WalkAll(ctx context.Context, u *uri.URL, onError func(*uri.URL, error) error, fn func(*store.Node) error) error {
	err := s.Store.WalkAll(ctx, u, onError, fn)
	if s.afterWalk != nil {
		s.afterWalk(u.Path)
	}
	return err
}

func TestRemoveWeighsTheFilterOnWhatItRemoves(t *testing.T) {
	root := t.TempDir()
	past := time.Now().AddDate(-2, 0, 0)
	for _, f := range []string{"busy/stale.log", "idle/stale.log", "old.log"} {
		write(t, filepath.Join(root, f), "stale")
		if err := os.Chtimes(filepath.Join(root, f), past, past); err != nil {
			t.Fatal(err)
		}
	}
	b := testBackend(t)
	o := batchOptions(root, "older:30d")
	o.Remove, o.Recursive, o.Confirmed = true, true, true
	b.opt = *o
	b.local = &hooked{Store: b.local,
		// After the walk that measured busy, before the one listing it for removal.
		afterWalk: func(path string) {
			if path == filepath.ToSlash(root) {
				write(t, filepath.Join(root, "busy", "fresh.txt"), "written meanwhile")
			}
		},
		// After the listing that judged it, before the stat it is removed by.
		beforeStat: func(path string) {
			if strings.HasSuffix(path, "/old.log") {
				write(t, filepath.Join(root, "old.log"), "rewritten meanwhile")
			}
		},
	}
	lines := output(t)
	if failed, err := b.removeBatch(t.Context(), o); err != nil || failed != 0 {
		t.Fatalf("remove: %d, %v", failed, err)
	}
	read(t, filepath.Join(root, "busy", "fresh.txt"), "written meanwhile")
	read(t, filepath.Join(root, "old.log"), "rewritten meanwhile")
	if _, err := os.Lstat(filepath.Join(root, "idle")); !os.IsNotExist(err) {
		t.Fatalf("the directory nobody touched survived: %v", err)
	}
	if got := strings.Join(lines(), "\n"); !strings.Contains(got, "kept '"+filepath.Join(root, "busy")+"'") {
		t.Errorf("kept directory not reported: %s", got)
	}
}

func TestRemoveTakesNestedAndRepeatedOperandsOnce(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "d", "sub", "x"), "x")
	write(t, filepath.Join(root, "d", "y"), "y")
	write(t, filepath.Join(root, "f"), "f")
	o := cli.Defaults()
	o.Remove, o.Recursive, o.Confirmed = true, true, true
	o.Sources = []string{filepath.Join(root, "d", "sub"), filepath.Join(root, "d"), filepath.Join(root, "f"), filepath.Join(root, "f")}
	if failed, err := Remove(t.Context(), o, slog.New(slog.DiscardHandler)); err != nil || failed != 0 {
		t.Fatalf("got %d failures, %v", failed, err)
	}
	for _, gone := range []string{"d", "f"} {
		if _, err := os.Lstat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived: %v", gone, err)
		}
	}
}

func TestRemoveNamesWhatItCouldNotRemove(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory its owner cannot write to")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "d", "locked")
	write(t, filepath.Join(locked, "x"), "x")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	t.Chdir(root)
	o := cli.Defaults()
	o.Remove, o.Recursive, o.Confirmed = true, true, true
	o.Sources = []string{"d"}
	lines := output(t)
	if failed, err := Remove(t.Context(), o, slog.New(slog.DiscardHandler)); err != nil || failed != 1 {
		t.Fatalf("got %d failures, %v", failed, err)
	}
	if got := lines(); !slices.Contains(got, "azcp: cannot remove 'd/locked/x': Permission denied") {
		t.Errorf("the failure does not name its file as rm would: %q", got)
	}
}

func TestRemoveInJSONReportsFailuresAndASummary(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "f"), "f")
	write(t, filepath.Join(root, "d", "x"), "x")
	o := cli.Defaults()
	o.Remove, o.Confirmed, o.Output = true, true, cli.OutputJSON
	o.Sources = []string{filepath.Join(root, "d"), filepath.Join(root, "f")}
	lines := output(t)
	if failed, err := Remove(t.Context(), o, slog.New(slog.DiscardHandler)); err != nil || failed != 1 {
		t.Fatalf("got %d failures, %v", failed, err)
	}
	var events []map[string]any
	for _, line := range lines() {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		events = append(events, event)
	}
	if len(events) != 3 || events[0]["event"] != "error" || events[0]["path"] != filepath.Join(root, "d") ||
		events[1]["event"] != "removed" || events[2]["event"] != "summary" ||
		events[2]["removed"] != 1.0 || events[2]["failed"] != 1.0 {
		t.Fatalf("events: %v", events)
	}
}

func TestListShowsEntriesAndTheirTotals(t *testing.T) {
	root := batchTree(t)
	o := cli.Defaults()
	o.List, o.Totals, o.Sources = true, true, []string{root}
	lines := output(t)
	if failed, err := List(t.Context(), o, slog.New(slog.DiscardHandler)); err != nil || failed != 0 {
		t.Fatalf("list: %d, %v", failed, err)
	}
	got := strings.Join(lines(), "\n")
	for _, want := range []string{"NEWEST", "new/", "old/", "top.txt", "total 16 B in 3 files"} {
		if !strings.Contains(got, want) {
			t.Errorf("listing lacks %q:\n%s", want, got)
		}
	}
	o.Sources = []string{filepath.Join(root, "missing")}
	if failed, err := List(t.Context(), o, slog.New(slog.DiscardHandler)); err != nil || failed != 1 {
		t.Fatalf("a missing operand: %d, %v", failed, err)
	}
}
