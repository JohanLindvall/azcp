// Package humanize formats byte counts, rates, durations and identifiers for
// display in a terminal. Everything here is display-only: no value produced by
// this package should ever be parsed back.
package humanize

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/rivo/uniseg"
)

var binaryUnits = [...]string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}

// Bytes renders n as an IEC size with three significant digits, e.g. "9.53 MiB".
func Bytes(n int64) string {
	if n > -1024 && n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	sign := ""
	if n < 0 {
		sign = "-"
	}
	val := math.Abs(float64(n))
	i := 0
	for val >= 1024 && i < len(binaryUnits)-1 {
		val /= 1024
		i++
	}
	return sign + trimNum(val) + " " + binaryUnits[i]
}

// Rate renders a transfer rate, e.g. "112 MiB/s". A non-positive or
// non-finite rate renders as an em dash so unknown speeds do not print "0 B/s".
func Rate(bytesPerSec float64) string {
	if bytesPerSec <= 0 || math.IsNaN(bytesPerSec) || math.IsInf(bytesPerSec, 0) {
		return "—"
	}
	val := bytesPerSec
	i := 0
	for val >= 1024 && i < len(binaryUnits)-1 {
		val /= 1024
		i++
	}
	return trimNum(val) + " " + binaryUnits[i] + "/s"
}

// trimNum keeps roughly three significant digits without trailing ".0".
func trimNum(v float64) string {
	switch {
	case v >= 100:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case v >= 10:
		return strconv.FormatFloat(v, 'f', 1, 64)
	default:
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
}

// Duration renders a coarse, fixed-vocabulary duration: "4.2s", "1m07s",
// "2h04m", "3d01h". Sub-second values render as milliseconds.
func Duration(d time.Duration) string {
	if d < 0 {
		// Saturating the unrepresentable magnitude loses one nanosecond,
		// well below the precision of this display.
		d = -(d + 1)
		if d < time.Duration(math.MaxInt64) {
			d++
		}
	}
	switch {
	case d > 0 && d < time.Millisecond:
		return "<1ms"
	case d < time.Second:
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	case d < 10*time.Second:
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
	case d < time.Minute:
		return strconv.FormatInt(int64(d.Seconds()), 10) + "s"
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// Count renders an integer with thousands separators.
func Count(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var groups []string
	for len(s) > 3 {
		groups = append([]string{s[len(s)-3:]}, groups...)
		s = s[:len(s)-3]
	}
	groups = append([]string{s}, groups...)
	out := strings.Join(groups, ",")
	if neg {
		out = "-" + out
	}
	return out
}

// Plural picks the form of a noun that agrees with n: "1 file", "2 files".
func Plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Elide shortens s to at most width display cells, cutting out the middle and
// leaving a horizontal ellipsis. The tail is favoured because the interesting
// part of a path (the file name) lives there.
func Elide(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if Width(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	// Reserve one cell for the ellipsis, then split the remainder so the tail
	// gets the larger half.
	keep := width - 1
	head := Truncate(s, keep/3)
	tailWidth := keep - Width(head)
	type cluster struct{ start, width int }
	var clusters []cluster
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		start, _ := g.Positions()
		clusters = append(clusters, cluster{start, g.Width()})
	}
	tail := len(s)
	for i := len(clusters) - 1; i >= 0; i-- {
		c := clusters[i]
		if c.width > tailWidth {
			break
		}
		tail, tailWidth = c.start, tailWidth-c.width
	}
	return head + "…" + s[tail:]
}

// Truncate takes the longest prefix that fits in width display cells without
// splitting a grapheme, such as an accented letter or a joined emoji.
func Truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	end, used := 0, 0
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		used += g.Width()
		if used > width {
			break
		}
		_, end = g.Positions()
	}
	return s[:end]
}

// Pad right-pads s with spaces to exactly width display cells, truncating with
// Elide when it is too long.
func Pad(s string, width int) string {
	s = Elide(s, width)
	if n := Width(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// Width counts terminal cells, including wide characters and combining marks.
func Width(s string) int { return uniseg.StringWidth(s) }

// ParseSize parses a human-written byte size such as "8MiB", "512k", "1.5G" or
// a bare byte count. It accepts both IEC (KiB) and short (K, KB) suffixes; all
// are interpreted as powers of 1024, matching how every other transfer tool
// treats block sizes.
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, errors.New("empty size")
	}
	i := 0
	for i < len(t) && (t[i] >= '0' && t[i] <= '9' || t[i] == '.') {
		i++
	}
	num, suffix := t[:i], strings.ToLower(strings.TrimSpace(t[i:]))
	if num == "" {
		return 0, fmt.Errorf("invalid size %q: no leading number", s)
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	suffix = strings.TrimSuffix(strings.TrimSuffix(suffix, "b"), "i")
	var mult float64 = 1
	switch suffix {
	case "":
	case "k":
		mult = 1 << 10
	case "m":
		mult = 1 << 20
	case "g":
		mult = 1 << 30
	case "t":
		mult = 1 << 40
	case "p":
		mult = 1 << 50
	default:
		return 0, fmt.Errorf("invalid size %q: unknown unit %q", s, suffix)
	}
	res := v * mult
	if res < 0 || res > float64(1<<62) {
		return 0, fmt.Errorf("size %q out of range", s)
	}
	return int64(res), nil
}
