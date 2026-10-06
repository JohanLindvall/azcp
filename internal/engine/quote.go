// SPDX-License-Identifier: MIT

package engine

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// quote uses cp's shell quoting for names. Controls must be visible text:
// filenames can contain newlines, terminal commands and bidirectional marks.
func quote(s string) string {
	var out strings.Builder
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !unicode.IsControl(r) && !unicode.Is(unicode.Bidi_Control, r) && !(r == utf8.RuneError && size == 1) {
			i += size
			continue
		}
		if start < i {
			out.WriteString(quotePlain(s[start:i]))
		}
		out.WriteString("$'")
		for _, c := range []byte(s[i : i+size]) {
			switch c {
			case '\a':
				out.WriteString(`\a`)
			case '\b':
				out.WriteString(`\b`)
			case '\f':
				out.WriteString(`\f`)
			case '\n':
				out.WriteString(`\n`)
			case '\r':
				out.WriteString(`\r`)
			case '\t':
				out.WriteString(`\t`)
			case '\v':
				out.WriteString(`\v`)
			default:
				fmt.Fprintf(&out, `\%03o`, c)
			}
		}
		out.WriteByte('\'')
		i += size
		start = i
	}
	if start == 0 {
		return quotePlain(s)
	}
	if start < len(s) {
		out.WriteString(quotePlain(s[start:]))
	}
	return out.String()
}

func quotePlain(s string) string {
	if strings.ContainsRune(s, '\'') && !strings.ContainsAny(s, "\"$`\\") {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
