// SPDX-License-Identifier: MIT

package cli

import "testing"

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
