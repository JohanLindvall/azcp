// SPDX-License-Identifier: MIT

package cli

import (
	"slices"
	"strings"
	"testing"
)

func TestUsageTUIIsExplicitAndLeavesFollowingPathsAsOperands(t *testing.T) {
	o := mustParse(t, "--tui=usage", "left", "azure://account/c")
	if !o.TUI || o.TUIMode != TUIUsage || o.TUIPaths != ([2]string{"left", "azure://account/c"}) {
		t.Fatalf("usage mode: %+v", o)
	}
	o = mustParse(t, "--tui", "usage")
	if o.TUIMode != TUICommander || o.TUIPaths[0] != "usage" {
		t.Fatal("optional TUI mode consumed a directory operand")
	}
	if _, err := Parse([]string{"--tui=invalid"}); err == nil {
		t.Fatal("accepted an invalid TUI mode")
	}
	o = mustParse(t, "--tui=single", "azure://account/c")
	if o.TUIMode != TUISingle || o.TUIPaths != ([2]string{"azure://account/c", "."}) {
		t.Fatalf("single mode: %+v", o)
	}
	if _, err := Parse([]string{"--tui=single", "a", "b"}); err == nil {
		t.Error("single mode accepted a second pane")
	}
}

func TestTUIIsExplicitAndHasIndependentOperands(t *testing.T) {
	for _, args := range [][]string{nil, {"tui"}} {
		if _, err := Parse(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	for _, args := range [][]string{{"tui", "dest"}, {"--", "--tui", "dest"}} {
		o := mustParse(t, args...)
		if o.TUI || o.Dest != "dest" || len(o.Sources) != 1 {
			t.Fatalf("ordinary cp changed for %v: %+v", args, o)
		}
	}
	for _, paths := range [][]string{nil, {"left"}, {"left", "azure://account/c"}} {
		o := mustParse(t, append([]string{"--tui"}, paths...)...)
		if !o.TUI || len(o.Sources) != 0 || o.Dest != "" {
			t.Fatalf("TUI parsed as copy: %+v", o)
		}
		want := [2]string{".", "."}
		copy(want[:], paths)
		if o.TUIPaths != want {
			t.Fatalf("paths %v, want %v", o.TUIPaths, want)
		}
	}
	if _, err := Parse([]string{"--tui", "a", "b", "c"}); err == nil {
		t.Fatal("accepted third pane")
	}
}

func TestTUIRejectsCopyPoliciesThatCouldLoseMoveSources(t *testing.T) {
	for _, flag := range []string{"-i", "-n", "-u", "-r", "-a", "--delete", "--compress", "--exclude=x", "--files-from=-", "--output=json", "--benchmark", "-t=x"} {
		if _, err := Parse([]string{"--tui", flag}); err == nil {
			t.Errorf("accepted %s", flag)
		}
	}
	o := mustParse(t, "--tui", "--auth=anonymous", "--jobs=2", "--resume", "--dry-run")
	if o.Jobs != 2 || !o.Resume || !o.DryRun {
		t.Fatalf("transfer options lost: %+v", o)
	}
}

func TestTUIJobsFollowEachCopyUnlessGiven(t *testing.T) {
	o := mustParse(t, "--tui")
	if o.JobsFor(false) != defaultJobs(false) || o.JobsFor(true) != defaultJobs(true) {
		t.Fatalf("defaults: local %d, network %d", o.JobsFor(false), o.JobsFor(true))
	}
	if defaultJobs(false) > 4 {
		t.Fatalf("a local copy defaults to %d jobs", defaultJobs(false))
	}
	o = mustParse(t, "--tui", "--jobs=12")
	if o.JobsFor(false) != 12 || o.JobsFor(true) != 12 {
		t.Fatal("--jobs was second-guessed")
	}
}

func TestRemoveTakesOnlyPathsAndNeedsYesToAct(t *testing.T) {
	o := mustParse(t, "--rm", "-r", "--select", "older:30d", "a", "azure://account/c/")
	if !o.Remove || o.Confirmed || !o.Recursive || o.Select != "older:30d" || len(o.Sources) != 2 || o.Dest != "" {
		t.Fatalf("remove: %+v", o)
	}
	if o = mustParse(t, "--rm", "--yes", "--no-progress", "a"); !o.Confirmed {
		t.Fatal("--yes did not confirm")
	}
	for _, args := range [][]string{
		{"--rm"}, {"--yes", "a", "b"}, {"--rm", "--yes", "--dry-run", "a"},
		{"--rm", "-i", "a"}, {"--rm", "-t", "d", "a"}, {"--rm", "--delete", "a"},
		{"--tui", "--select", "x"},
		// Accepted and then ignored would be doing less than asked.
		{"--rm", "--progress=always", "a"}, {"--rm", "--retry-delay=1s", "a"},
		{"--rm", "--retry-max-delay=1m", "a"},
		// An unset variable must not turn a selection into its directory.
		{"--rm", "-r", "--select", "", "a"}, {"--rm", "-r", "--select=  ", "a"},
		{"-r", "--select=", "src", "dst"},
		{"--rm", "--whole-containers", "a"}, {"-r", "--whole-containers", "--select", "x", "a", "b"},
	} {
		if _, err := Parse(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	if o = mustParse(t, "--rm", "-r", "--whole-containers", "--select", "has:*.mp4", "azure://a/"); !o.WholeContainers {
		t.Fatal("--whole-containers was not taken")
	}
	if o = mustParse(t, "-r", "--select", "*.mp4", "src", "dst"); o.Remove || o.Dest != "dst" {
		t.Fatalf("a selected copy changed shape: %+v", o)
	}
}

// getopt_long takes any unambiguous prefix of a long option, so an option of
// azcp's own that begins one of cp's changes what a cp command line means.
// --remove is how cp's own --remove-destination may be written.
func TestNoExtensionBeginsACpOption(t *testing.T) {
	cp := []string{"archive", "attributes-only", "backup", "copy-contents", "debug",
		"dereference", "force", "help", "interactive", "link", "no-clobber",
		"no-dereference", "no-preserve", "no-target-directory", "one-file-system",
		"parents", "preserve", "recursive", "reflink", "remove-destination", "sparse",
		"strip-trailing-slashes", "suffix", "symbolic-link", "target-directory",
		"update", "verbose", "version", "context"}
	for _, s := range specs {
		if s.Long == "" || slices.Contains(cp, s.Long) {
			continue
		}
		for _, name := range cp {
			if strings.HasPrefix(name, s.Long) {
				t.Errorf("--%s begins cp's --%s, so it changes what --%s already means", s.Long, name, s.Long)
			}
		}
	}
	if o := mustParse(t, "--remove", "a", "b"); !o.RemoveDestination || o.Remove || o.Dest != "b" {
		t.Fatalf("--remove is not cp's --remove-destination: %+v", o)
	}
}

func TestListTakesPathsAndLooksOnly(t *testing.T) {
	if o := mustParse(t, "--ls"); !o.List || len(o.Sources) != 1 || o.Sources[0] != "." {
		t.Fatalf("--ls without operands: %+v", o)
	}
	if o := mustParse(t, "--ls", "--totals", "--select", "type:dir", "a", "azure://account/"); !o.Totals || len(o.Sources) != 2 {
		t.Fatalf("--ls --totals: %+v", o)
	}
	for _, args := range [][]string{
		{"--totals", "a", "b"}, {"--ls", "--rm", "a"}, {"--ls", "-r", "a"},
		{"--ls", "--yes", "a"}, {"--ls", "--delete", "a"},
	} {
		if _, err := Parse(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
}
