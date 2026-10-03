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
func ExpandBraces(s string) ([]string, error) {
	out := make([]string, 0, 1)
	if err := expandInto(s, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return []string{s}, nil
	}
	return out, nil
}

func expandInto(s string, out *[]string) error {
	if len(*out) >= maxBraceResults {
		return fmt.Errorf("brace expansion exceeds %d results", maxBraceResults)
	}
	open, closeIdx, items, ok := findGroup(s)
	if !ok {
		*out = append(*out, s)
		return nil
	}
	prefix, suffix := s[:open], s[closeIdx+1:]
	for _, it := range items {
		if err := expandInto(prefix+it+suffix, out); err != nil {
			return err
		}
	}
	return nil
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
	step := uint(1)
	if len(parts) == 3 {
		n, err := strconv.Atoi(parts[2])
		if err != nil {
			return nil
		}
		if n == 0 {
			n = 1 // bash treats a zero increment as the default increment
		}
		step = uint(n)
		if n < 0 {
			step = uint(-(n + 1)) + 1
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
				return out // the caller reports the expansion limit
			}
			if lo <= hi {
				if step > uint(hi)-uint(v) {
					break
				}
				v = int(uint(v) + step)
			} else {
				if step > uint(v)-uint(hi) {
					break
				}
				v = int(uint(v) - step)
			}
		}
		return out
	}
	a, b := []rune(parts[0]), []rune(parts[1])
	letter := func(s string) bool { return len(s) == 1 && (s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z') }
	if !letter(parts[0]) || !letter(parts[1]) {
		return nil
	}
	var out []string
	for r := a[0]; (a[0] <= b[0] && r <= b[0]) || (a[0] > b[0] && r >= b[0]); {
		out = append(out, string(r))
		if len(out) > maxBraceResults {
			return out
		}
		if a[0] <= b[0] {
			if step > uint(b[0]-r) {
				break
			}
			r += rune(step)
		} else {
			if step > uint(r-b[0]) {
				break
			}
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
