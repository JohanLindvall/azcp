package progress

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// colourLevel is how much colour the terminal can show. Everything above
// levelNone degrades gracefully: the layout is identical, only the escapes
// differ.
type colourLevel int

const (
	levelNone colourLevel = iota
	level16
	level256
	levelTrue
)

type palette struct{ level colourLevel }

// detectPalette follows the conventions terminal programs are expected to
// honour: NO_COLOR wins over everything, TERM=dumb means no escapes at all, and
// COLORTERM advertises truecolor.
func detectPalette(isTTY bool) palette {
	if !isTTY {
		return palette{levelNone}
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return palette{levelNone}
	}
	term := os.Getenv("TERM")
	if term == "" || term == "dumb" {
		return palette{levelNone}
	}
	if os.Getenv("CLICOLOR") == "0" {
		return palette{levelNone}
	}
	switch strings.ToLower(os.Getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return palette{levelTrue}
	}
	if strings.Contains(term, "256color") {
		return palette{level256}
	}
	return palette{level16}
}

func (p palette) wrap(code, s string) string {
	if p.level == levelNone || s == "" {
		return s
	}
	return code + s + "\x1b[0m"
}

func (p palette) dim(s string) string    { return p.wrap("\x1b[2m", s) }
func (p palette) bold(s string) string   { return p.wrap("\x1b[1m", s) }
func (p palette) accent(s string) string { return p.wrap("\x1b[36m", s) }
func (p palette) warn(s string) string   { return p.wrap("\x1b[33m", s) }
func (p palette) bad(s string) string    { return p.wrap("\x1b[31m", s) }
func (p palette) good(s string) string   { return p.wrap("\x1b[32m", s) }
func (p palette) track(s string) string  { return p.wrap("\x1b[90m", s) }
func (p palette) sep(s string) string    { return s }

// gradient endpoints: a cool blue at the start of the bar warming to green as
// it fills, so the eye can read progress from colour alone.
var (
	gradFrom = [3]int{56, 189, 248}
	gradTo   = [3]int{74, 222, 128}
)

// gradient colours s according to its position t in [0,1] along the bar.
func (p palette) gradient(t float64, s string) string {
	switch p.level {
	case levelNone:
		return s
	case levelTrue:
		t = min(max(t, 0), 1)
		r := int(float64(gradFrom[0]) + t*float64(gradTo[0]-gradFrom[0]))
		g := int(float64(gradFrom[1]) + t*float64(gradTo[1]-gradFrom[1]))
		b := int(float64(gradFrom[2]) + t*float64(gradTo[2]-gradFrom[2]))
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[0m", r, g, b, s)
	case level256:
		// A hand-picked run through the 6x6x6 cube from cyan to green.
		ramp := [...]int{45, 44, 43, 42, 41, 47, 83, 84}
		i := int(min(max(t, 0), 1) * float64(len(ramp)-1))
		return fmt.Sprintf("\x1b[38;5;%dm%s\x1b[0m", ramp[i], s)
	default:
		return p.accent(s)
	}
}

// escState tracks position within an escape sequence. A CSI sequence is
// ESC '[' then parameter and intermediate bytes then one final byte in the
// range @ to ~. The '[' itself falls in that range, so it has to be consumed as
// part of the introducer rather than mistaken for the terminator.
type escState int

const (
	escNone escState = iota
	escSeenESC
	escInCSI
)

// step advances the state for one rune and reports whether that rune is part of
// an escape sequence (and so occupies no display cell).
func (e *escState) step(r rune) bool {
	switch *e {
	case escNone:
		if r == '\x1b' {
			*e = escSeenESC
			return true
		}
		return false
	case escSeenESC:
		if r == '[' {
			*e = escInCSI
		} else {
			// A two-character escape such as ESC c; it ends here.
			*e = escNone
		}
		return true
	default:
		if r >= '@' && r <= '~' {
			*e = escNone
		}
		return true
	}
}

// stripANSI removes escape sequences so display width can be measured.
func stripANSI(s string) string {
	if !strings.ContainsRune(s, '\x1b') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	var st escState
	for _, r := range s {
		if !st.step(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// truncateANSI shortens s to at most width visible cells, leaving escape
// sequences intact and closing any style it cut through.
func truncateANSI(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if !strings.ContainsRune(s, '\x1b') {
		if utf8.RuneCountInString(s) <= width {
			return s
		}
		r := []rune(s)
		return string(r[:width])
	}
	var b strings.Builder
	visible, styled := 0, false
	var st escState
	for _, r := range s {
		if st.step(r) {
			styled = true
			b.WriteRune(r)
			continue
		}
		if visible >= width {
			if styled {
				b.WriteString("\x1b[0m")
			}
			return b.String()
		}
		b.WriteRune(r)
		visible++
	}
	return b.String()
}
