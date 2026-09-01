// Package glob implements bash-style pathname expansion with the globstar and
// extglob options enabled, over an abstract path space that uses "/" as the
// separator. It is used for both local paths and blob keys, so it deliberately
// knows nothing about the filesystem: Compile turns a pattern into segments and
// callers drive their own directory walk.
//
// Supported syntax:
//
//   - any run of characters within one path element
//     ?            any single character within one path element
//     **           (as a whole path element) zero or more path elements
//     [abc] [a-z]  character class, negated with [!abc] or [^abc]
//     [[:alpha:]]  POSIX character classes inside a bracket expression
//     ?(p|q)       zero or one occurrence of a pattern     (extglob)
//     *(p|q)       zero or more occurrences                (extglob)
//     +(p|q)       one or more occurrences                 (extglob)
//     @(p|q)       exactly one occurrence                  (extglob)
//     !(p|q)       anything except the given patterns      (extglob)
//     {a,b} {1..9} brace expansion, applied before matching
//     \x           a literal x
package glob

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SegKind classifies one path element of a compiled pattern.
type SegKind int

const (
	// SegLiteral is an element with no metacharacters; Text is its exact,
	// unescaped value.
	SegLiteral SegKind = iota
	// SegMatch is an element that must be matched character by character.
	SegMatch
	// SegDoubleStar is "**": it matches zero or more whole path elements.
	SegDoubleStar
)

// Segment is one path element of a compiled pattern.
type Segment struct {
	Kind SegKind
	Text string // original pattern text; unescaped when Kind is SegLiteral
	seq  *sequence
}

// Match reports whether a single path element matches this segment. Calling it
// on a SegDoubleStar segment is a programming error and always returns false;
// "**" spans elements and is handled by Pattern.Match.
func (s Segment) Match(name string) bool {
	switch s.Kind {
	case SegLiteral:
		return name == s.Text
	case SegMatch:
		return s.seq.match(name)
	default:
		return false
	}
}

// Pattern is a compiled path pattern.
type Pattern struct {
	// Abs records whether the pattern started at "/".
	Abs bool
	// TrailingSlash records whether the pattern ended in "/", which callers use
	// to restrict matches to directories.
	TrailingSlash bool
	// Segs are the path elements, in order.
	Segs []Segment
	src  string
}

// Compile parses a pattern. It returns an error only for syntax that cannot be
// interpreted at all; unmatched brackets and parens are treated as literals,
// exactly as a shell does.
func Compile(pattern string) (*Pattern, error) {
	p := &Pattern{src: pattern}
	s := pattern
	if strings.HasPrefix(s, "/") {
		p.Abs = true
	}
	if len(s) > 1 && strings.HasSuffix(s, "/") {
		p.TrailingSlash = true
	}
	for _, part := range SplitPath(s) {
		switch {
		case part == "**":
			p.Segs = append(p.Segs, Segment{Kind: SegDoubleStar, Text: part})
		case !segHasMeta(part):
			p.Segs = append(p.Segs, Segment{Kind: SegLiteral, Text: unescape(part)})
		default:
			seq, err := parseSequence(part)
			if err != nil {
				return nil, fmt.Errorf("bad pattern %q: %w", pattern, err)
			}
			p.Segs = append(p.Segs, Segment{Kind: SegMatch, Text: part, seq: seq})
		}
	}
	return p, nil
}

// MustCompile is Compile for patterns known good at build time.
func MustCompile(pattern string) *Pattern {
	p, err := Compile(pattern)
	if err != nil {
		panic(err)
	}
	return p
}

func (p *Pattern) String() string { return p.src }

// HasWildcard reports whether the pattern contains anything that must actually
// be matched, as opposed to a plain path spelled out in full.
func (p *Pattern) HasWildcard() bool {
	for _, s := range p.Segs {
		if s.Kind != SegLiteral {
			return true
		}
	}
	return false
}

// LiteralPrefix returns the leading run of literal elements joined with "/",
// and the index of the first element that still needs matching. It lets callers
// start their walk deep in the tree — or, for a blob store, issue a single
// prefixed listing — instead of at the root.
func (p *Pattern) LiteralPrefix() (prefix string, rest int) {
	var parts []string
	i := 0
	for ; i < len(p.Segs) && p.Segs[i].Kind == SegLiteral; i++ {
		parts = append(parts, p.Segs[i].Text)
	}
	prefix = strings.Join(parts, "/")
	if p.Abs {
		prefix = "/" + prefix
	}
	return prefix, i
}

// Match reports whether path matches the pattern in full.
func (p *Pattern) Match(path string) bool {
	return matchSegs(p.Segs, 0, SplitPath(path), 0)
}

// MatchFrom reports whether the trailing elements of path, starting at pattern
// element `from`, match. It is the entry point used by walkers that have
// already consumed the literal prefix.
func (p *Pattern) MatchFrom(from int, path string) bool {
	if from > len(p.Segs) {
		return false
	}
	return matchSegs(p.Segs, from, SplitPath(path), 0)
}

func matchSegs(segs []Segment, si int, parts []string, pi int) bool {
	for si < len(segs) {
		seg := segs[si]
		if seg.Kind == SegDoubleStar {
			// "**" absorbs zero or more elements; try every split. Trailing
			// "**" segments collapse, so this stays linear in practice.
			for k := pi; k <= len(parts); k++ {
				if matchSegs(segs, si+1, parts, k) {
					return true
				}
			}
			return false
		}
		if pi >= len(parts) || !seg.Match(parts[pi]) {
			return false
		}
		si++
		pi++
	}
	return pi == len(parts)
}

// SplitPath splits a "/"-separated path into elements, dropping empty elements
// and "." so that "./a//b" and "a/b" compare equal.
func SplitPath(s string) []string {
	if s == "" {
		return nil
	}
	raw := strings.Split(s, "/")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if p != "" && p != "." {
			out = append(out, p)
		}
	}
	return out
}

// HasMeta reports whether s contains unescaped pattern metacharacters,
// including braces. Callers use it to decide whether an argument is a pattern
// at all or just a path that happens to be spelled with odd characters.
func HasMeta(s string) bool { return hasMetaIn(s, true) }

func segHasMeta(s string) bool { return hasMetaIn(s, false) }

func hasMetaIn(s string, braces bool) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '*', '?', '[':
			return true
		case '+', '@', '!':
			if i+1 < len(s) && s[i+1] == '(' {
				return true
			}
		case '{':
			if braces {
				return true
			}
		}
	}
	return false
}

// unescape removes backslash escapes from a string that will not be matched.
func unescape(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Within-element matching
// ---------------------------------------------------------------------------

type node interface{ node() }

type litNode struct{ s string }
type anyNode struct{}
type starNode struct{}

type classRange struct{ lo, hi rune }

type classNode struct {
	neg    bool
	ranges []classRange
	names  []string
}

// extNode is an extglob group: op is one of ?*+@! and alts are the
// "|"-separated alternatives.
type extNode struct {
	op   byte
	alts []*sequence
}

func (litNode) node()   {}
func (anyNode) node()   {}
func (starNode) node()  {}
func (classNode) node() {}
func (extNode) node()   {}

type sequence struct {
	nodes []node
	// backtracks records that matching can reach the same (node, offset) pair
	// by more than one route — a second star, or an extglob group alongside
	// one — which is when memoising the search pays for itself. The common
	// "*.txt" has one route to everything and is matched without allocating.
	backtracks bool
}

// finish computes what the matcher needs to know about a parsed sequence.
func (s *sequence) finish() *sequence {
	branching := 0
	for _, n := range s.nodes {
		switch n.(type) {
		case starNode, extNode:
			branching++
		}
	}
	s.backtracks = branching > 1
	return s
}

func parseSequence(s string) (*sequence, error) {
	seq, pos, err := parseNodes(s, 0, false)
	if err != nil {
		return nil, err
	}
	if pos != len(s) {
		return nil, fmt.Errorf("trailing %q", s[pos:])
	}
	return seq, nil
}

// parseNodes reads nodes from s starting at pos. When inGroup is true it stops
// at a top-level "|" or ")" and returns the index of that delimiter.
func parseNodes(s string, pos int, inGroup bool) (*sequence, int, error) {
	seq := &sequence{}
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			seq.nodes = append(seq.nodes, litNode{lit.String()})
			lit.Reset()
		}
	}
	for pos < len(s) {
		c := s[pos]
		switch {
		case c == '\\':
			if pos+1 < len(s) {
				pos++
				lit.WriteByte(s[pos])
				pos++
			} else {
				lit.WriteByte('\\')
				pos++
			}
		case inGroup && (c == '|' || c == ')'):
			flush()
			return seq.finish(), pos, nil
		case c == '*' || c == '?':
			// '*' and '?' are ambiguous: alone they are wildcards, but
			// followed by '(' they open an extglob group.
			if pos+1 < len(s) && s[pos+1] == '(' {
				if ext, next := parseExt(s, pos); ext != nil {
					flush()
					seq.nodes = append(seq.nodes, *ext)
					pos = next
					continue
				}
			}
			flush()
			if c == '*' {
				seq.nodes = append(seq.nodes, starNode{})
			} else {
				seq.nodes = append(seq.nodes, anyNode{})
			}
			pos++
		case c == '[':
			if cls, next, ok := parseClass(s, pos); ok {
				flush()
				seq.nodes = append(seq.nodes, cls)
				pos = next
			} else {
				lit.WriteByte(c)
				pos++
			}
		case (c == '+' || c == '@' || c == '!') && pos+1 < len(s) && s[pos+1] == '(':
			ext, next := parseExt(s, pos)
			if ext == nil { // no closing paren: treat as literal text
				lit.WriteByte(c)
				pos++
				continue
			}
			flush()
			seq.nodes = append(seq.nodes, *ext)
			pos = next
		default:
			lit.WriteByte(c)
			pos++
		}
	}
	if inGroup {
		return nil, 0, errors.New("unterminated group")
	}
	flush()
	return seq.finish(), pos, nil
}

// parseExt parses an extglob group starting at s[pos] (the operator). It
// returns a nil node when there is no closing paren, so the caller can fall
// back to treating the operator as a literal character.
func parseExt(s string, pos int) (*extNode, int) {
	op := s[pos]
	p := pos + 2 // skip operator and "("
	ext := &extNode{op: op}
	for {
		alt, next, err := parseNodes(s, p, true)
		if err != nil {
			return nil, 0 // an unterminated group is literal text
		}
		ext.alts = append(ext.alts, alt)
		if next >= len(s) {
			return nil, 0
		}
		if s[next] == ')' {
			return ext, next + 1
		}
		p = next + 1 // skip "|"
	}
}

func parseClass(s string, pos int) (classNode, int, bool) {
	p := pos + 1
	var cls classNode
	if p < len(s) && (s[p] == '!' || s[p] == '^') {
		cls.neg = true
		p++
	}
	first := true
	for p < len(s) {
		if s[p] == ']' && !first {
			return cls, p + 1, true
		}
		first = false
		// POSIX class: [:name:]
		if strings.HasPrefix(s[p:], "[:") {
			if end := strings.Index(s[p+2:], ":]"); end >= 0 {
				cls.names = append(cls.names, s[p+2:p+2+end])
				p += 2 + end + 2
				continue
			}
		}
		if s[p] == '\\' && p+1 < len(s) {
			p++
		}
		lo, w := utf8.DecodeRuneInString(s[p:])
		p += w
		hi := lo
		if p+1 < len(s) && s[p] == '-' && s[p+1] != ']' {
			p++
			if s[p] == '\\' && p+1 < len(s) {
				p++
			}
			r, w2 := utf8.DecodeRuneInString(s[p:])
			hi = r
			p += w2
		}
		cls.ranges = append(cls.ranges, classRange{lo, hi})
	}
	return classNode{}, 0, false // no closing bracket
}

func (c classNode) matches(r rune) bool {
	in := false
	for _, rg := range c.ranges {
		if r >= rg.lo && r <= rg.hi {
			in = true
			break
		}
	}
	if !in {
		for _, n := range c.names {
			if matchPosixClass(n, r) {
				in = true
				break
			}
		}
	}
	if c.neg {
		return !in
	}
	return in
}

func (s *sequence) match(str string) bool {
	var memo map[[2]int]bool
	if s.backtracks {
		memo = make(map[[2]int]bool)
	}
	return matchNodes(s.nodes, 0, str, 0, memo)
}

// matchNodes matches nodes[i:] against s[p:]. memo, when the sequence needs
// one, remembers the outcome for every (i, p) already tried.
func matchNodes(nodes []node, i int, s string, p int, memo map[[2]int]bool) bool {
	if i == len(nodes) {
		return p == len(s)
	}
	key := [2]int{i, p}
	if v, ok := memo[key]; ok {
		return v
	}
	res := false
	switch n := nodes[i].(type) {
	case litNode:
		if strings.HasPrefix(s[p:], n.s) {
			res = matchNodes(nodes, i+1, s, p+len(n.s), memo)
		}
	case anyNode:
		if p < len(s) {
			_, w := utf8.DecodeRuneInString(s[p:])
			res = matchNodes(nodes, i+1, s, p+w, memo)
		}
	case starNode:
		for e := p; ; {
			if matchNodes(nodes, i+1, s, e, memo) {
				res = true
				break
			}
			if e >= len(s) {
				break
			}
			_, w := utf8.DecodeRuneInString(s[e:])
			e += w
		}
	case classNode:
		if p < len(s) {
			r, w := utf8.DecodeRuneInString(s[p:])
			if n.matches(r) {
				res = matchNodes(nodes, i+1, s, p+w, memo)
			}
		}
	case extNode:
		for _, e := range n.ends(s, p) {
			if matchNodes(nodes, i+1, s, e, memo) {
				res = true
				break
			}
		}
	}
	if memo != nil {
		memo[key] = res
	}
	return res
}

// ends returns every position the extglob group can consume up to, starting
// from p.
func (n extNode) ends(s string, p int) []int {
	// one returns the positions reachable by matching exactly one alternative.
	one := func(from int) []int {
		var out []int
		for e := from; e <= len(s); e++ {
			if e < len(s) && !utf8.RuneStart(s[e]) {
				continue
			}
			for _, alt := range n.alts {
				if alt.match(s[from:e]) {
					out = append(out, e)
					break
				}
			}
		}
		return out
	}
	closure := func(seeds []int) []int {
		seen := map[int]bool{}
		queue := append([]int(nil), seeds...)
		for _, s0 := range seeds {
			seen[s0] = true
		}
		for len(queue) > 0 {
			c := queue[0]
			queue = queue[1:]
			for _, e := range one(c) {
				// e == c means an alternative matched the empty string;
				// repeating it can never make progress.
				if e > c && !seen[e] {
					seen[e] = true
					queue = append(queue, e)
				}
			}
		}
		out := make([]int, 0, len(seen))
		for k := range seen {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}

	switch n.op {
	case '@':
		return one(p)
	case '?':
		return append([]int{p}, one(p)...)
	case '*':
		return closure([]int{p})
	case '+':
		seeds := one(p)
		if len(seeds) == 0 {
			return nil
		}
		return closure(seeds)
	case '!':
		var out []int
		for e := p; e <= len(s); e++ {
			if e < len(s) && !utf8.RuneStart(s[e]) {
				continue
			}
			excluded := false
			for _, alt := range n.alts {
				if alt.match(s[p:e]) {
					excluded = true
					break
				}
			}
			if !excluded {
				out = append(out, e)
			}
		}
		return out
	}
	return nil
}

// matchPosixClass implements the [:name:] classes. Unknown names match nothing,
// which mirrors how a shell treats a bracket expression it cannot interpret.
func matchPosixClass(name string, r rune) bool {
	switch name {
	case "alpha":
		return unicode.IsLetter(r)
	case "digit":
		return unicode.IsDigit(r)
	case "alnum":
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	case "upper":
		return unicode.IsUpper(r)
	case "lower":
		return unicode.IsLower(r)
	case "space":
		return unicode.IsSpace(r)
	case "blank":
		return r == ' ' || r == '\t'
	case "punct":
		return unicode.IsPunct(r) || unicode.IsSymbol(r)
	case "print":
		return unicode.IsPrint(r)
	case "graph":
		return unicode.IsGraphic(r) && !unicode.IsSpace(r)
	case "cntrl":
		return unicode.IsControl(r)
	case "xdigit":
		return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
	case "word":
		return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
	case "ascii":
		return r <= 0x7f
	default:
		return false
	}
}
