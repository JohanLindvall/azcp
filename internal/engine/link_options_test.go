// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/uri"
)

func TestAttributesOnlyKeepsUnrelatedDestinationLinks(t *testing.T) {
	for _, preserve := range []string{"--preserve=links", "--preserve=all"} {
		t.Run(preserve, func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "src/a"), "first source")
			write(t, filepath.Join(d, "src/b"), "second source")
			write(t, filepath.Join(d, "dst/a"), "keep these bytes")
			if err := os.Link(filepath.Join(d, "dst/a"), filepath.Join(d, "dst/b")); err != nil {
				t.Skip(err)
			}
			before := inode(t, filepath.Join(d, "dst/a"))
			if n := run(t, d, "--attributes-only", preserve, "src/a", "src/b", "dst"); n != 0 {
				t.Fatal(n)
			}
			for _, name := range []string{"a", "b"} {
				path := filepath.Join(d, "dst", name)
				if inode(t, path) != before || read(t, path) != "keep these bytes" {
					t.Fatalf("attributes-only replaced %s or its contents", name)
				}
			}
		})
	}
}

func TestAttributesOnlyPreservesSourceLinks(t *testing.T) {
	for _, firstExists := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-first", true: "existing-first"}[firstExists], func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "src/a"), "source")
			if err := os.Link(filepath.Join(d, "src/a"), filepath.Join(d, "src/b")); err != nil {
				t.Skip(err)
			}
			want := ""
			if firstExists {
				want = "retained first"
				write(t, filepath.Join(d, "dst/a"), want)
			}
			write(t, filepath.Join(d, "dst/b"), "second destination")
			if n := run(t, d, "--attributes-only", "--preserve=links", "src/a", "src/b", "dst"); n != 0 {
				t.Fatal(n)
			}
			if inode(t, filepath.Join(d, "dst/a")) != inode(t, filepath.Join(d, "dst/b")) || read(t, filepath.Join(d, "dst/b")) != want {
				t.Fatal("attributes-only failed to link the second destination to the first")
			}
		})
	}
}

func TestPreservedLinksFollowTheDereferencePolicy(t *testing.T) {
	for _, flags := range [][]string{{"--preserve=links"}, {"--attributes-only", "--preserve=links"}, {"-au"}, {"-aH"}, {"-aL"}} {
		t.Run(flags[0], func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "src/a"), "source")
			if err := os.Link(filepath.Join(d, "src/a"), filepath.Join(d, "src/b")); err != nil {
				t.Skip(err)
			}
			write(t, filepath.Join(d, "dst/target"), "retained")
			if err := os.Symlink("target", filepath.Join(d, "dst/a")); err != nil {
				t.Skip(err)
			}
			old := time.Unix(946684800, 0)
			if err := os.Chtimes(filepath.Join(d, "src/a"), old, old); err != nil {
				t.Fatal(err)
			}
			if n := run(t, d, append(flags, "src/a", "src/b", "dst")...); n != 0 {
				t.Fatal(n)
			}
			want := "dst/target"
			if flags[0] == "-au" {
				want = "dst/a" // -a implies -P, including for the preserved link.
			}
			if inode(t, filepath.Join(d, "dst/b")) != inode(t, filepath.Join(d, want)) {
				t.Fatalf("the second destination should link to %s", want)
			}
			if link, err := os.Readlink(filepath.Join(d, "dst/a")); err != nil || link != "target" {
				t.Fatalf("first destination symlink changed: %q, %v", link, err)
			}
		})
	}
}

func TestPreservedLinksInATreeFollowTheWalkPolicy(t *testing.T) {
	for _, flag := range []string{"-a", "-aH", "-aL"} {
		t.Run(flag, func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "src/a"), "source")
			if err := os.Link(filepath.Join(d, "src/a"), filepath.Join(d, "src/b")); err != nil {
				t.Skip(err)
			}
			write(t, filepath.Join(d, "dst/target"), "original")
			for _, name := range []string{"a", "b"} {
				if err := os.Symlink("target", filepath.Join(d, "dst", name)); err != nil {
					t.Skip(err)
				}
			}
			if n := run(t, d, flag, "-T", "src", "dst"); n != 0 {
				t.Fatal(n)
			}
			want := "dst/a"
			if flag == "-aL" {
				want = "dst/target"
			}
			if inode(t, filepath.Join(d, "dst/b")) != inode(t, filepath.Join(d, want)) {
				t.Fatalf("nested link should use %s under %s", want, flag)
			}
		})
	}
}

func TestUpdateLinksSkippedDestinationsWithExplicitCopyModes(t *testing.T) {
	for _, mode := range []string{"-l", "-s", "--attributes-only"} {
		t.Run(mode, func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "src/a"), "source")
			if err := os.Link(filepath.Join(d, "src/a"), filepath.Join(d, "src/b")); err != nil {
				t.Skip(err)
			}
			old := time.Unix(946684800, 0)
			if err := os.Chtimes(filepath.Join(d, "src/a"), old, old); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(d, "dst/a"), "first destination")
			write(t, filepath.Join(d, "dst/b"), "second destination")
			if n := run(t, d, "-u", mode, "src/a", "src/b", "dst"); n != 0 {
				t.Fatal(n)
			}
			if inode(t, filepath.Join(d, "dst/a")) != inode(t, filepath.Join(d, "dst/b")) || read(t, filepath.Join(d, "dst/b")) != "first destination" {
				t.Fatal("-u did not link its retained destinations")
			}
		})
	}
}

func TestPreserveLinksWithExplicitLinkModes(t *testing.T) {
	for _, mode := range []string{"-l", "-s"} {
		t.Run(mode, func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "src/a"), "source")
			if mode == "-s" {
				if err := os.Symlink("src/a", filepath.Join(d, "probe")); err != nil {
					t.Skip(err)
				}
			}
			if err := os.Link(filepath.Join(d, "src/a"), filepath.Join(d, "src/b")); err != nil {
				t.Skip(err)
			}
			if n := run(t, d, mode, "--preserve=links", "src/a", "src/b", "."); n != 0 {
				t.Fatal(n)
			}
			// cp follows the first destination when preserving a regular
			// source's links, including a symlink made by -s itself.
			if inode(t, filepath.Join(d, "b")) != inode(t, filepath.Join(d, "src/a")) {
				t.Fatal("the later name did not link to the first destination's file")
			}
		})
	}
}

func TestPreserveLinksRechecksTheDestinationAfterAnEarlierUnlink(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src"), "source")
	write(t, filepath.Join(d, "dst"), "destination")
	if err := os.Link(filepath.Join(d, "dst"), filepath.Join(d, "alias")); err != nil {
		t.Skip(err)
	}
	t.Chdir(d)
	e := newEngine(t, "--preserve=links", "src", "dst")
	src, err := e.local.Stat(t.Context(), &uri.URL{Scheme: uri.SchemeFile, Path: "src"}, false)
	if err != nil {
		t.Fatal(err)
	}
	planned := &task{src: src, dst: &uri.URL{Scheme: uri.SchemeFile, Path: "dst"}}
	if err := e.prepareLocalTask(planned); err != nil {
		t.Fatal(err)
	}
	if !planned.removeFirst || planned.destID == nil {
		t.Fatal("the initially linked destination was not scheduled for separation")
	}
	done := make(chan struct{})
	e.recordLocalAccess(&task{destID: planned.destID, done: done})
	if err := os.Remove("alias"); err != nil {
		t.Fatal(err)
	}
	close(done)
	if err := e.awaitLocalAccess(t.Context(), planned); err != nil {
		t.Fatal(err)
	}
	if planned.removeFirst {
		t.Fatal("the last destination name would be unnecessarily removed")
	}
}

func TestSymbolicLinkNoDereferenceReplacesExistingDestination(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "target"), "source")
	write(t, filepath.Join(d, "dst"), "old")
	if err := os.Symlink("target", filepath.Join(d, "src")); err != nil {
		t.Skip(err)
	}
	if n := run(t, d, "-sP", "src", "dst"); n != 0 {
		t.Fatal(n)
	}
	if link, err := os.Readlink(filepath.Join(d, "dst")); err != nil || link != "src" {
		t.Fatalf("destination = %q, %v", link, err)
	}
}

func TestFailedArchivePreparationSettlesLinkClaim(t *testing.T) {
	for _, flags := range []string{"-ab", "-abu"} {
		t.Run(flags, func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "src/a"), "source")
			if err := os.Link(filepath.Join(d, "src/a"), filepath.Join(d, "src/b")); err != nil {
				t.Skip(err)
			}
			write(t, filepath.Join(d, "dst/a"), "cannot back up")
			write(t, filepath.Join(d, "dst/a~/child"), "blocks the backup rename")
			old := time.Unix(946684800, 0)
			if err := os.Chtimes(filepath.Join(d, "dst/a"), old, old); err != nil {
				t.Fatal(err)
			}
			t.Chdir(d)
			e := newEngine(t, flags, "src/a", "src/b", "dst")
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if n, err := e.Run(ctx); n != 1 || err != nil || ctx.Err() != nil {
				t.Fatalf("backup failure left a waiting link: failures %d, err %v, context %v", n, err, ctx.Err())
			}
			if got := read(t, filepath.Join(d, "dst/b")); got != "source" {
				t.Fatalf("later source = %q", got)
			}
		})
	}
}

func TestFailedUnlinkSettlesLinkClaim(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/a"), "source")
	if err := os.Link(filepath.Join(d, "src/a"), filepath.Join(d, "src/b")); err != nil {
		t.Skip(err)
	}
	write(t, filepath.Join(d, "dst/child"), "protect")
	e := newEngine(t, "-a", filepath.Join(d, "src/a"), filepath.Join(d, "dst"))
	src, err := e.local.Stat(t.Context(), mustURL(t, filepath.Join(d, "src/a")), false)
	if err != nil {
		t.Fatal(err)
	}
	// The destination became a nonempty directory after the scanner had
	// decided to unlink it. The failure must release every waiting alias.
	copy := &task{src: src, dst: mustURL(t, filepath.Join(d, "dst")), removeFirst: true}
	e.planHardLink(copy)
	future := copy.hardLink
	if future == nil {
		t.Fatal("source did not claim its link group")
	}
	if err := e.transfer(t.Context(), copy, nil); err == nil {
		t.Fatal("removed a nonempty directory")
	}
	select {
	case <-future.done:
		if future.ok {
			t.Fatal("failed unlink advertised a completed copy")
		}
	default:
		t.Fatal("failed unlink left its link group waiting")
	}
	if got := read(t, filepath.Join(d, "dst/child")); got != "protect" {
		t.Fatalf("destination contents changed: %q", got)
	}
}
