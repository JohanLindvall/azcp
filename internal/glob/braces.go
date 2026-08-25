package glob

import (
	"fmt"
	"strconv"
	"strings"
)

// maxBraceResults caps brace expansion so a pattern like {1..1000000} cannot
// exhaust memory before the walk even starts.
const maxBraceResults = 8192

// ExpandBraces performs bash-style brace expansion, handling nesting,
// comma lists and {a..b[..step]} sequences. A string with no expandable group
// is returned unchanged as the sole result.
func ExpandBraces(s string) []string {
	out := make([]string, 0, 1)
	expandInto(s, &out)
	if len(out) == 0 {
		return []string{s}
	}
	return out
}

func expandInto(s string, out *[]string) {
	if len(*out) >= maxBraceResults {
		return
	}
	open, closeIdx, items, ok := findGroup(s)
	if !ok {
		*out = append(*out, s)
		return
	}
	prefix, suffix := s[:open], s[closeIdx+1:]
	for _, it := range items {
		expandInto(prefix+it+suffix, out)
	}
}

// findGroup locates the leftmost brace group that is a real expansion — one
// with a top-level comma, or a valid sequence. Braces that are neither (such as
// a lone "{}" or shell parameter syntax) are skipped over as ordinary text.
func findGroup(s string) (open, closeIdx int, items []string, ok bool) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '[':
			if j, isClass := skipBracket(s, i); isClass {
				i = j - 1
			}
		case '{':
			if o, c, its, valid := scanGroup(s, i); valid {
				return o, c, its, true
			}
		}
	}
	return 0, 0, nil, false
}

func scanGroup(s string, open int) (int, int, []string, bool) {
	depth := 1
	last := open + 1
	var parts []string
	sawComma := false
	for i := open + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '[':
			if j, isClass := skipBracket(s, i); isClass {
				i = j - 1
			}
		case '{':
			depth++
		case ',':
			if depth == 1 {
				parts = append(parts, s[last:i])
				last = i + 1
				sawComma = true
			}
		case '}':
			depth--
			if depth == 0 {
				parts = append(parts, s[last:i])
				if sawComma {
					return open, i, parts, true
				}
				if seq := expandSequence(s[open+1 : i]); seq != nil {
					return open, i, seq, true
				}
				return 0, 0, nil, false
			}
		}
	}
	return 0, 0, nil, false
}

// skipBracket reports the index just past a bracket expression starting at i,
// so brace scanning does not trip over a "{" inside "[...]".
func skipBracket(s string, i int) (int, bool) {
	p := i + 1
	if p < len(s) && (s[p] == '!' || s[p] == '^') {
		p++
	}
	if p < len(s) && s[p] == ']' {
		p++
	}
	for p < len(s) {
		switch s[p] {
		case '\\':
			p += 2
		case ']':
			return p + 1, true
		default:
			p++
		}
	}
	return i, false
}

// expandSequence expands {a..b} and {a..b..step} over integers or single
// characters. It returns nil when body is not a sequence.
func expandSequence(body string) []string {
	parts := strings.Split(body, "..")
	if len(parts) != 2 && len(parts) != 3 {
		return nil
	}
	step := 1
	if len(parts) == 3 {
		n, err := strconv.Atoi(parts[2])
		if err != nil || n == 0 {
			return nil
		}
		step = n
		if step < 0 {
			step = -step
		}
	}
	if lo, errA := strconv.Atoi(parts[0]); errA == nil {
		hi, errB := strconv.Atoi(parts[1])
		if errB != nil {
			return nil
		}
		width := 0
		if isZeroPadded(parts[0]) || isZeroPadded(parts[1]) {
			width = max(len(parts[0]), len(parts[1]))
		}
		var out []string
		for v := lo; (lo <= hi && v <= hi) || (lo > hi && v >= hi); {
			out = append(out, formatSeqInt(v, width))
			if len(out) > maxBraceResults {
				return nil
			}
			if lo <= hi {
				v += step
			} else {
				v -= step
			}
		}
		return out
	}
	a, b := []rune(parts[0]), []rune(parts[1])
	if len(a) != 1 || len(b) != 1 {
		return nil
	}
	var out []string
	for r := a[0]; (a[0] <= b[0] && r <= b[0]) || (a[0] > b[0] && r >= b[0]); {
		out = append(out, string(r))
		if len(out) > maxBraceResults {
			return nil
		}
		if a[0] <= b[0] {
			r += rune(step)
		} else {
			r -= rune(step)
		}
	}
	return out
}

func isZeroPadded(s string) bool {
	s = strings.TrimPrefix(s, "-")
	return len(s) > 1 && s[0] == '0'
}

func formatSeqInt(v, width int) string {
	if width == 0 {
		return strconv.Itoa(v)
	}
	return fmt.Sprintf("%0*d", width, v)
}
