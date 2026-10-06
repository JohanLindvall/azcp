// SPDX-License-Identifier: MIT

package humanize

import (
	"math"
	"testing"
	"time"
)

func TestBytes(t *testing.T) {
	cases := map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1.00 KiB", 1536: "1.50 KiB",
		10 * 1024: "10.0 KiB", 999 * 1024: "999 KiB", 1 << 20: "1.00 MiB",
		10_000_000: "9.54 MiB", 1 << 40: "1.00 TiB", -2048: "-2.00 KiB",
		math.MinInt64: "-8.00 EiB",
	}
	for in, want := range cases {
		if got := Bytes(in); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestMinimumDurationDoesNotWrap(t *testing.T) {
	if got, want := Duration(time.Duration(math.MinInt64)), Duration(time.Duration(math.MaxInt64)); got != want {
		t.Fatalf("minimum duration = %q, want %q", got, want)
	}
}

func TestCount(t *testing.T) {
	cases := map[int64]string{0: "0", 12: "12", 123: "123", 1234: "1,234",
		1000000: "1,000,000", -98765: "-98,765"}
	for in, want := range cases {
		if got := Count(in); got != want {
			t.Errorf("Count(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0: "0ms", time.Nanosecond: "<1ms", 500 * time.Microsecond: "<1ms",
		400 * time.Millisecond: "400ms", 4200 * time.Millisecond: "4.2s",
		42 * time.Second: "42s", 67 * time.Second: "1m07s",
		2*time.Hour + 4*time.Minute: "2h04m", 73 * time.Hour: "3d01h",
	}
	for in, want := range cases {
		if got := Duration(in); got != want {
			t.Errorf("Duration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestElide(t *testing.T) {
	if got := Elide("abcdefghij", 10); got != "abcdefghij" {
		t.Errorf("no-op elide = %q", got)
	}
	got := Elide("data/2024/logs/access.log.gz", 12)
	if Width(got) != 12 {
		t.Errorf("Elide width = %d (%q), want 12", Width(got), got)
	}
	if Width(Pad("ab", 6)) != 6 {
		t.Error("Pad did not pad to width")
	}
}

func TestUnicodeDisplayCells(t *testing.T) {
	for _, tt := range []struct {
		text  string
		width int
	}{
		{"日本語", 6},
		{"e\u0301", 1},
		{"🇸🇪", 2},
		{"👩🏽‍💻", 2},
		{"❤️", 2},
	} {
		if got := Width(tt.text); got != tt.width {
			t.Errorf("Width(%q) = %d, want %d", tt.text, got, tt.width)
		}
		if got := Truncate(tt.text+"suffix", tt.width); got != tt.text {
			t.Errorf("Truncate split a grapheme: %q, want %q", got, tt.text)
		}
		for width := 0; width <= 12; width++ {
			if got := Elide("prefix/"+tt.text+".txt", width); Width(got) > width {
				t.Errorf("Elide(%q, %d) overflowed: %q", tt.text, width, got)
			}
			if got := Pad(tt.text, width); Width(got) != width {
				t.Errorf("Pad(%q, %d) has %d cells: %q", tt.text, width, Width(got), got)
			}
		}
	}
	if got := Elide("long/path/👩🏽‍💻", 3); got != "…👩🏽‍💻" {
		t.Errorf("Elide split a joined emoji: %q", got)
	}
	if got := Truncate("日", 1); got != "" {
		t.Errorf("Truncate put a wide character in one cell: %q", got)
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"1024": 1024, "8MiB": 8 << 20, "8M": 8 << 20, "512k": 512 << 10,
		"1.5G": 1610612736, "2GB": 2 << 30, "0": 0,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "10x", "MiB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) should fail", bad)
		}
	}
}

func TestRate(t *testing.T) {
	cases := map[float64]string{
		0:               "—",
		-5:              "—",
		math.NaN():      "—",
		math.Inf(1):     "—",
		512:             "512 B/s",
		2 * 1024 * 1024: "2.00 MiB/s",
	}
	for in, want := range cases {
		if got := Rate(in); got != want {
			t.Errorf("Rate(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestPlural(t *testing.T) {
	for n, want := range map[int64]string{0: "files", 1: "file", 2: "files", -1: "files"} {
		if got := Plural(n, "file", "files"); got != want {
			t.Errorf("Plural(%d) = %q, want %q", n, got, want)
		}
	}
}
