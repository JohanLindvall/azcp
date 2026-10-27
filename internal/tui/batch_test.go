// SPDX-License-Identifier: MIT

package tui

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/cli"
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
