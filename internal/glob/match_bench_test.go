package glob

import "testing"

// Only a pattern that can reach the same position by more than one route needs
// the memo; the common single-wildcard patterns are matched without allocating.
func TestBacktracksOnlyWhenNeeded(t *testing.T) {
	cases := map[string]bool{
		"*.txt":    false,
		"a?c":      false,
		"[abc]x*":  false,
		"@(a|b)":   false,
		"!(*.gz)":  false,
		"*a*":      true,
		"@(a|b)*":  true,
		"*(x)+(y)": true,
	}
	for pat, want := range cases {
		p := MustCompile(pat)
		if got := p.Segs[0].seq.backtracks; got != want {
			t.Errorf("%q: backtracks = %v, want %v", pat, got, want)
		}
	}
}

// The memo-free path must agree with the memoised one on everything.
func TestMatchAgreesWithAndWithoutMemo(t *testing.T) {
	cases := []struct {
		pat, s string
	}{
		{"*.txt", "a.txt"}, {"*.txt", "a.txt.bak"}, {"a?c", "abc"}, {"a?c", "abbc"},
		{"[a-c]x*", "bxyz"}, {"[a-c]x*", "dx"}, {"@(a|b)", "a"}, {"@(a|b)", "ab"},
		{"!(*.gz)", "file.txt"}, {"!(*.gz)", "file.gz"}, {"a*b", "ab"}, {"a*b", "a"},
	}
	for _, c := range cases {
		p := MustCompile(c.pat)
		seq := p.Segs[0].seq
		plain := seq.match(c.s)
		memoised := (&sequence{nodes: seq.nodes, backtracks: true}).match(c.s)
		if plain != memoised {
			t.Errorf("%q against %q: %v without memo, %v with", c.pat, c.s, plain, memoised)
		}
	}
}

func BenchmarkMatch(b *testing.B) {
	const subject = "part-00017-of-00100.parquet"
	cases := []struct{ name, pat string }{
		{"single-star", "*.parquet"},
		{"two-stars", "*-of-*.parquet"},
		{"extglob", "!(*.tmp|*.bak)"},
	}
	for _, c := range cases {
		p := MustCompile(c.pat)
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if !p.Match(subject) {
					b.Fatalf("%q did not match %q", c.pat, subject)
				}
			}
		})
	}
	// The same single-star pattern forced through the memo, for comparison.
	forced := &sequence{nodes: MustCompile("*.parquet").Segs[0].seq.nodes, backtracks: true}
	b.Run("single-star-memoised", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if !forced.match(subject) {
				b.Fatal("no match")
			}
		}
	})
}
