package store

import (
	"context"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/JohanLindvall/azcp/internal/glob"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// memStore is a namespace held in a map, so the walker can be exercised
// without a filesystem and its requests counted.
type memStore struct {
	nodes map[string]*Node
	dirs  map[string]bool

	stats, readDirs, walks int
}

// newMemStore builds a store from paths; a trailing "/" marks a directory.
func newMemStore(t *testing.T, paths ...string) *memStore {
	t.Helper()
	m := &memStore{nodes: map[string]*Node{}, dirs: map[string]bool{}}
	for _, p := range paths {
		dir := strings.HasSuffix(p, "/")
		p = strings.TrimSuffix(p, "/")
		u, err := uri.Parse(p, uri.Options{})
		if err != nil {
			t.Fatal(err)
		}
		n := &Node{URL: u, Kind: KindFile, Mode: 0o644, Size: 1}
		if dir {
			n.Kind, n.Mode = KindDir, fs.ModeDir|0o755
			m.dirs[p] = true
		}
		m.nodes[p] = n
	}
	return m
}

// key normalises a location the way the walker spells them, so "." and "./a"
// find what they name.
func key(u *uri.URL) string { return strings.Join(glob.SplitPath(u.PathPart()), "/") }

func (m *memStore) Scheme() string { return "mem" }

func (m *memStore) Stat(_ context.Context, u *uri.URL, _ bool) (*Node, error) {
	m.stats++
	if n, ok := m.nodes[key(u)]; ok {
		return n, nil
	}
	return nil, fmt.Errorf("%s: %w", u.PathPart(), ErrNotExist)
}

func (m *memStore) ReadDir(_ context.Context, u *uri.URL) ([]*Node, error) {
	m.readDirs++
	dir := key(u)
	if dir != "" && !m.dirs[dir] {
		return nil, fmt.Errorf("%s: %w", dir, ErrNotExist)
	}
	var out []*Node
	for p, n := range m.nodes {
		parent := ""
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			parent = p[:i]
		}
		if parent == dir && p != dir {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, func(a, b *Node) int { return strings.Compare(a.Name(), b.Name()) })
	return out, nil
}

func (m *memStore) WalkAll(_ context.Context, u *uri.URL, _ func(*uri.URL, error) error, fn func(*Node) error) error {
	m.walks++
	base := key(u)
	for _, p := range slices.Sorted(func(yield func(string) bool) {
		for p := range m.nodes {
			if !yield(p) {
				return
			}
		}
	}) {
		if p != base && (base == "" || strings.HasPrefix(p, base+"/")) {
			if err := fn(m.nodes[p]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *memStore) MkdirAll(context.Context, *uri.URL, fs.FileMode) error { return nil }
func (m *memStore) Remove(context.Context, *uri.URL) error                { return nil }

func expand(t *testing.T, m *memStore, pattern string) []string {
	t.Helper()
	u, err := uri.Parse(pattern, uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := Expand(context.Background(), m, u, ExpandOptions{})
	if err != nil {
		t.Fatalf("Expand(%q): %v", pattern, err)
	}
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.URL.PathPart()
	}
	return out
}

func tree(t *testing.T) *memStore {
	t.Helper()
	return newMemStore(t, "logs/", "logs/a.log", "logs/b.log", "logs/c.txt",
		"logs/sub/", "logs/sub/d.log", "logs/sub/deeper/", "logs/sub/deeper/e.log", "top.log")
}

// A plain path is one Stat and nothing else, which is what keeps ordinary
// copies at one round trip per argument.
func TestExpandLiteralIsOneStat(t *testing.T) {
	m := tree(t)
	if got := expand(t, m, "logs/a.log"); !slices.Equal(got, []string{"logs/a.log"}) {
		t.Errorf("got %v", got)
	}
	if m.stats != 1 || m.readDirs != 0 || m.walks != 0 {
		t.Errorf("a literal path cost %d stats, %d listings, %d walks", m.stats, m.readDirs, m.walks)
	}
}

// One wildcard element is one listing of its directory, filtered by name.
func TestExpandSingleElementIsOneListing(t *testing.T) {
	m := tree(t)
	got := expand(t, m, "logs/*.log")
	if want := []string{"logs/a.log", "logs/b.log"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if m.readDirs != 1 || m.walks != 0 {
		t.Errorf("one element cost %d listings and %d walks", m.readDirs, m.walks)
	}
}

// "**" is one recursive listing however deep the tree, not one per prefix.
func TestExpandDoubleStarIsOneWalk(t *testing.T) {
	m := tree(t)
	got := expand(t, m, "logs/**/*.log")
	want := []string{"logs/a.log", "logs/b.log", "logs/sub/d.log", "logs/sub/deeper/e.log"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if m.walks != 1 || m.readDirs != 0 {
		t.Errorf("** cost %d walks and %d listings", m.walks, m.readDirs)
	}
}

func TestExpandTrailingSlashSelectsDirectories(t *testing.T) {
	m := tree(t)
	if got := expand(t, m, "logs/*/"); !slices.Equal(got, []string{"logs/sub"}) {
		t.Errorf("got %v", got)
	}
	if got := expand(t, m, "**/"); !slices.Equal(got, []string{"logs", "logs/sub", "logs/sub/deeper"}) {
		t.Errorf("got %v", got)
	}
}

// A pattern that matches nothing is an empty result, not a failure; whether
// that is an error is the caller's decision, as it is for a shell.
func TestExpandNoMatchIsEmpty(t *testing.T) {
	if got := expand(t, tree(t), "logs/*.zip"); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestExpandMissingLiteralIsNotExist(t *testing.T) {
	u, err := uri.Parse("nope/x.txt", uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Expand(context.Background(), tree(t), u, ExpandOptions{}); !IsNotExist(err) {
		t.Errorf("a missing literal path gave %v", err)
	}
}

func TestRelUnder(t *testing.T) {
	cases := []struct {
		base, child, want string
		ok                bool
	}{
		{"a/b", "a/b/c/d", "c/d", true},
		{"./a", "a/x", "x", true},
		{"a//b", "a/b/x", "x", true},
		{"a", "a", "", true},
		{"", "x/y", "x/y", true},
		{"a/b", "a/c", "", false},
		{"a/b/c", "a/b", "", false},
	}
	for _, c := range cases {
		got, ok := RelUnder(c.base, c.child)
		if got != c.want || ok != c.ok {
			t.Errorf("RelUnder(%q, %q) = %q, %v; want %q, %v", c.base, c.child, got, ok, c.want, c.ok)
		}
	}
}
