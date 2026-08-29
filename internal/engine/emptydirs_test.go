package engine

import (
	"sort"
	"testing"

	"github.com/JohanLindvall/azcp/internal/uri"
)

// feed replays a walk — ancestors before contents, as WalkAll guarantees —
// into a tracker and returns the paths that would get marker blobs.
func feed(t *testing.T, entries ...string) []string {
	t.Helper()
	base, err := uri.Parse("azure://acct/container", uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	e := newEmptyDirs()
	for _, entry := range entries {
		kind := entry[:2]
		u := base.Join(entry[2:])
		switch kind {
		case "d:":
			e.dir(u)
		case "f:":
			e.file(u)
		default:
			t.Fatalf("bad entry %q", entry)
		}
	}
	var out []string
	for _, u := range e.leaves() {
		out = append(out, u.Key)
	}
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Only the deepest empty directories need markers: a directory over another
// directory's marker, or over a blob, already exists as a prefix.
func TestEmptyDirsMarksOnlyLeaves(t *testing.T) {
	cases := []struct {
		name    string
		entries []string
		want    []string
	}{
		{"single empty directory",
			[]string{"d:a"}, []string{"a"}},
		{"chain of empty directories keeps only the leaf",
			[]string{"d:a", "d:a/b", "d:a/b/c"}, []string{"a/b/c"}},
		{"a blob clears the whole chain above it",
			[]string{"d:a", "d:a/b", "f:a/b/f.txt"}, nil},
		{"a blob beside an empty sibling clears only its own chain",
			[]string{"d:a", "d:a/b", "d:a/c", "f:a/b/f.txt"}, []string{"a/c"}},
		{"two empty branches each keep their leaf",
			[]string{"d:a", "d:a/b", "d:a/c"}, []string{"a/b", "a/c"}},
		{"a deeper branch after a shallow blob",
			[]string{"d:a", "f:a/f.txt", "d:a/b", "d:a/b/c"}, []string{"a/b/c"}},
		{"a top-level blob leaves nothing",
			[]string{"f:top.txt"}, nil},
	}
	for _, c := range cases {
		if got := feed(t, c.entries...); !equal(got, c.want) {
			t.Errorf("%s: markers for %v, want %v", c.name, got, c.want)
		}
	}
}

// A local destination needs no markers; the nil tracker accepts everything
// and yields nothing.
func TestEmptyDirsNilTracker(t *testing.T) {
	var e *emptyDirs
	base, err := uri.Parse("azure://acct/container", uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	e.dir(base.Join("a"))
	e.file(base.Join("a/f"))
	if got := e.leaves(); got != nil {
		t.Errorf("nil tracker produced leaves: %v", got)
	}
}
