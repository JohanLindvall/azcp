package glob

import (
	"reflect"
	"strings"
	"testing"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		// plain wildcards
		{"*.txt", "a.txt", true},
		{"*.txt", "a.log", false},
		{"*.txt", "sub/a.txt", false},
		{"a/*/c", "a/b/c", true},
		{"a/*/c", "a/b/x/c", false},
		{"?.txt", "a.txt", true},
		{"?.txt", "ab.txt", false},
		{"a*b*c", "axxbyyc", true},

		// globstar
		{"**", "a", true},
		{"**", "a/b/c", true},
		{"a/**/c", "a/c", true},
		{"a/**/c", "a/b/c", true},
		{"a/**/c", "a/b/x/y/c", true},
		{"a/**/c", "a/b/c/d", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "internal/glob/glob.go", true},
		{"**/*.go", "internal/glob/glob.txt", false},
		{"data/**", "data", true},
		{"data/**", "data/2024/x.log", true},
		{"data/**/logs/**/*.gz", "data/a/logs/b/c.gz", true},
		{"data/**/logs/**/*.gz", "data/logs/c.gz", true},
		// a bare "*" must not cross a separator even next to globstar
		{"a/*/**", "a/b/c/d", true},
		{"a/*/**", "a/b", true},

		// character classes
		{"[abc].txt", "b.txt", true},
		{"[abc].txt", "d.txt", false},
		{"[!abc].txt", "d.txt", true},
		{"[^abc].txt", "a.txt", false},
		{"[a-z][0-9]", "q7", true},
		{"[a-z][0-9]", "77", false},
		{"[[:digit:]][[:alpha:]]", "1a", true},
		{"[[:digit:]][[:alpha:]]", "a1", false},
		{"[]].txt", "].txt", true},
		{"[a-]", "-", true},

		// extglob
		{"@(foo|bar).txt", "foo.txt", true},
		{"@(foo|bar).txt", "baz.txt", false},
		{"?(foo)bar", "bar", true},
		{"?(foo)bar", "foobar", true},
		{"?(foo)bar", "foofoobar", false},
		{"*(ab)c", "c", true},
		{"*(ab)c", "ababc", true},
		{"*(ab)c", "abac", false},
		{"+(ab)c", "c", false},
		{"+(ab)c", "abc", true},
		{"!(foo).txt", "bar.txt", true},
		{"!(foo).txt", "foo.txt", false},
		{"!(*.tmp)", "keep.log", true},
		{"!(*.tmp)", "junk.tmp", false},
		{"@(*.tar|*.tar.gz)", "x.tar.gz", true},
		{"+([0-9]).log", "2024.log", true},
		{"+([0-9]).log", "20x4.log", false},
		{"**/@(bin|sbin)/*", "usr/local/bin/ls", true},
		{"**/@(bin|sbin)/*", "usr/local/lib/ls", false},

		// escaping
		{`a\*b`, "a*b", true},
		{`a\*b`, "axb", false},
		{`\[x\]`, "[x]", true},

		// path normalisation
		{"a/b", "./a/b", true},
		{"a//b", "a/b", true},
	}
	for _, c := range cases {
		p, err := Compile(c.pat)
		if err != nil {
			t.Errorf("Compile(%q): %v", c.pat, err)
			continue
		}
		if got := p.Match(c.path); got != c.want {
			t.Errorf("Compile(%q).Match(%q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestLiteralPrefix(t *testing.T) {
	cases := []struct {
		pat, prefix string
		rest        int
	}{
		{"a/b/c", "a/b/c", 3},
		{"a/b/*.txt", "a/b", 2},
		{"a/**/c", "a", 1},
		{"*.txt", "", 0},
		{"/var/log/*.gz", "/var/log", 2},
	}
	for _, c := range cases {
		p := MustCompile(c.pat)
		gp, gr := p.LiteralPrefix()
		if gp != c.prefix || gr != c.rest {
			t.Errorf("LiteralPrefix(%q) = %q,%d; want %q,%d", c.pat, gp, gr, c.prefix, c.rest)
		}
	}
}

func TestHasWildcard(t *testing.T) {
	if MustCompile("a/b/c").HasWildcard() {
		t.Error("plain path reported as wildcard")
	}
	for _, p := range []string{"a/*", "**/x", "a/[ab]", "@(a|b)"} {
		if !MustCompile(p).HasWildcard() {
			t.Errorf("%q not reported as wildcard", p)
		}
	}
}

func TestHasMeta(t *testing.T) {
	yes := []string{"*.txt", "a?", "a[bc]", "{a,b}", "!(x)", "+(x)", "@(x)"}
	no := []string{"plain.txt", `a\*b`, "a-b_c.d", "dir/sub/file"}
	for _, s := range yes {
		if !HasMeta(s) {
			t.Errorf("HasMeta(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if HasMeta(s) {
			t.Errorf("HasMeta(%q) = true, want false", s)
		}
	}
}

func TestExpandBraces(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a", []string{"a"}},
		{"{a,b}", []string{"a", "b"}},
		{"x{a,b}y", []string{"xay", "xby"}},
		{"{a,b}{1,2}", []string{"a1", "a2", "b1", "b2"}},
		{"a{b,{c,d}}e", []string{"abe", "ace", "ade"}},
		{"{1..4}", []string{"1", "2", "3", "4"}},
		{"{01..04}", []string{"01", "02", "03", "04"}},
		{"{1..7..3}", []string{"1", "4", "7"}},
		{"{5..1}", []string{"5", "4", "3", "2", "1"}},
		{"{a..e}", []string{"a", "b", "c", "d", "e"}},
		{"{}", []string{"{}"}},
		{"{a}", []string{"{a}"}},
		{"a{b", []string{"a{b"}},
		{"logs/{app,web}/*.log", []string{"logs/app/*.log", "logs/web/*.log"}},
	}
	for _, c := range cases {
		if got := ExpandBraces(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ExpandBraces(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestPathological guards the memoised matcher against catastrophic
// backtracking: without memoisation this input takes exponential time.
func TestPathological(t *testing.T) {
	p := MustCompile(strings.Repeat("*a", 24) + "b")
	if p.Match(strings.Repeat("a", 200)) {
		t.Error("unexpected match")
	}
}
