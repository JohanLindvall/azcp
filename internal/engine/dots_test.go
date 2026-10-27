// SPDX-License-Identifier: MIT

package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// SRC/. is the directory's contents, and cp copies SRC/.. into the target too,
// rather than into the target's parent.
func TestDotOperandsCopyIntoTheTargetItself(t *testing.T) {
	for _, tc := range []struct{ cwd, operand string }{
		{"", "src/."},
		{"", "src/./"},
		{"", "src/sub/.."},
		{"src", "."},
		{"src/sub", ".."},
	} {
		t.Run(tc.operand, func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "src", "a.txt"), "a")
			write(t, filepath.Join(d, "src", "sub", "b.txt"), "b")
			target := filepath.Join(d, "box", "target")
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			if n := run(t, filepath.Join(d, tc.cwd), "-r", tc.operand, target+"/"); n != 0 {
				t.Fatalf("%d failed", n)
			}
			if read(t, filepath.Join(target, "a.txt")) != "a" || read(t, filepath.Join(target, "sub", "b.txt")) != "b" {
				t.Fatal("the contents did not land in the target")
			}
			entries, err := os.ReadDir(filepath.Join(d, "box"))
			if err != nil || len(entries) != 1 {
				t.Fatalf("something landed beside the target: %v, %v", entries, err)
			}
		})
	}
}

// A blob name keeps whatever it is given, so the dots of a local operand must
// be resolved before one is made from it: azcp -r . azure://acct/c/ once
// wrote ./a.txt.
func TestDotsInALocalOperandNeverBecomeBlobNames(t *testing.T) {
	dest, err := uri.Parse("azure://acct/c/pre/", uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		src     string
		parents bool
		want    string
	}{
		{"t", false, "c/pre/t"},
		{"t/.", false, "c/pre"},
		{"t/./", false, "c/pre"},
		{"t/logs/..", false, "c/pre"},
		{".", false, "c/pre"},
		{"./t/x.txt", true, "c/pre/t/x.txt"},
		{"t/./x.txt", true, "c/pre/t/x.txt"},
		{"t/.", true, "c/pre/t"},
		{".", true, "c/pre"},
		// A blob's own name is exact, dots included.
		{"azure://acct/c/dir/.", false, "c/pre/."},
	} {
		src, err := uri.Parse(tc.src, uri.Options{})
		if err != nil {
			t.Fatal(err)
		}
		// A dry run plans without creating the parents --parents implies.
		e := &Engine{opt: &cli.Options{Parents: tc.parents, DryRun: true}}
		target, err := e.targetFor(t.Context(), dest, true, &store.Node{URL: src, Kind: store.KindDir})
		if err != nil || target.PathPart() != tc.want {
			t.Errorf("parents=%v %q lands at %v, %v; want %q", tc.parents, tc.src, target, err, tc.want)
		}
	}
	for _, climbs := range []string{"../x.txt", "t/../x.txt"} {
		src, _ := uri.Parse(climbs, uri.Options{})
		e := &Engine{opt: &cli.Options{Parents: true, DryRun: true}}
		if target, err := e.targetFor(t.Context(), dest, true, &store.Node{URL: src}); err == nil {
			t.Errorf("--parents %q was given the blob name %q", climbs, target.PathPart())
		}
	}
}
