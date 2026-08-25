package glob

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
)

// TestMatchesBash compares this package against the shell it imitates. bash is
// run with globstar and extglob enabled — the options whose behaviour this
// package reproduces — over a fixed tree, and its expansion of each pattern is
// compared with ours.
//
// The test skips where bash is unavailable, so it does not turn into a
// portability problem; where bash is present it is the strongest statement
// available that the matcher is right.
func TestMatchesBash(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The bash on PATH may be Git Bash or WSL, and WSL sees a different
		// filesystem entirely, so the comparison would not mean anything.
		t.Skip("not meaningful on Windows")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	if !bashSupportsOptions(t, bash) {
		t.Skip("bash is too old for globstar and extglob")
	}

	root := buildTree(t)
	names := listTree(t, root)

	patterns := []string{
		"*.txt", "*.log", "a/*", "a/*/*", "?.txt", "r[12].txt",
		"[bf]*.txt", "[!f]*.txt", "[[:digit:]]*.log",
		"**", "**/*.txt", "a/**", "a/**/*.txt", "a/**/c/*",
		"data/**", "data/**/*.gz", "data/**/logs/*.gz", "**/logs/**",
		"usr/**/bin/*", "**/*.tar.gz",
		"**/@(bin|sbin)/*", "!(foo).txt", "!(*.tmp)", "+([0-9]).log",
		"@(foo|bar).txt", "*(a|b)*.txt", "?(*.gz|*.md)",
		"usr/local/*(bin|sbin)",
		"data/{2023,2024}/logs/*.gz", "{a,data}/*", "r{1,2}.txt",
	}

	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			want := bashExpand(t, bash, root, pattern)
			got := ourExpand(t, names, pattern)
			if !slices.Equal(got, want) {
				t.Errorf("pattern %q\n  bash: %v\n  ours: %v", pattern, want, got)
			}
		})
	}
}

func bashSupportsOptions(t *testing.T, bash string) bool {
	t.Helper()
	return exec.Command(bash, "-c", "shopt -s globstar extglob").Run() == nil
}

// bashExpand asks bash to expand the pattern inside root. nullglob makes an
// unmatched pattern expand to nothing rather than to itself, and dotglob makes
// the comparison independent of hidden-file rules.
func bashExpand(t *testing.T, bash, root, pattern string) []string {
	t.Helper()
	cmd := exec.Command(bash, "-O", "globstar", "-O", "extglob", "-O", "nullglob",
		"-O", "dotglob", "-c", "printf '%s\\n' "+pattern)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bash failed on %q: %v", pattern, err)
	}
	return normalise(strings.Split(string(out), "\n"))
}

// ourExpand matches the pattern against every path in the tree, which is what
// the walker does once it has listed a directory.
func ourExpand(t *testing.T, names []string, pattern string) []string {
	t.Helper()
	var out []string
	for _, raw := range ExpandBraces(pattern) {
		p, err := Compile(raw)
		if err != nil {
			t.Fatalf("Compile(%q): %v", raw, err)
		}
		for _, n := range names {
			if p.Match(n) {
				out = append(out, n)
			}
		}
	}
	return normalise(out)
}

// normalise sorts, deduplicates and drops empties so the two lists compare on
// content rather than on the order each side happens to produce.
func normalise(in []string) []string {
	out := in[:0:0]
	for _, s := range in {
		// bash writes a trailing slash for a directory matched through "**/".
		if s = strings.TrimSuffix(strings.TrimSpace(s), "/"); s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func buildTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dirs := []string{
		"a/b/c", "a/b2", "data/2024/logs", "data/2023/logs",
		"usr/local/bin", "usr/local/sbin", "usr/local/lib", "empty",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := []string{
		"a/f.txt", "a/b/g.txt", "a/b/c/h.txt", "a/b2/i.log",
		"data/2024/logs/x.gz", "data/2024/logs/y.tar.gz",
		"data/2023/logs/z.gz", "data/note.md",
		"usr/local/bin/ls", "usr/local/sbin/fsck", "usr/local/lib/libc.so",
		"keep.log", "junk.tmp", "foo.txt", "bar.txt", "baz.log",
		"2024.log", "r1.txt", "r2.txt",
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(root, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func listTree(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	err := filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if rel != "." {
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}
