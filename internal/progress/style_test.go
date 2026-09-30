package progress

import (
	"strings"
	"testing"

	"github.com/JohanLindvall/azcp/internal/humanize"
)

func TestStripANSI(t *testing.T) {
	cases := map[string]string{
		"plain":                         "plain",
		"\x1b[36mcyan\x1b[0m":           "cyan",
		"\x1b[38;2;56;189;248m█\x1b[0m": "█",
		"a\x1b[1;33mb\x1b[0mc":          "abc",
		"\x1b[2K\x1b[Aleft":             "left",
		"\x1b[0m":                       "",
	}
	for in, want := range cases {
		if got := stripANSI(in); got != want {
			t.Errorf("stripANSI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateANSI(t *testing.T) {
	// Styled text must be measured by visible cells, not bytes.
	styled := "\x1b[36mabcdefghij\x1b[0m"
	got := truncateANSI(styled, 4)
	if s := stripANSI(got); s != "abcd" {
		t.Errorf("truncateANSI visible = %q, want %q", s, "abcd")
	}
	if got[len(got)-4:] != "\x1b[0m" {
		t.Errorf("truncateANSI did not close the style: %q", got)
	}
	if got := truncateANSI("plain text", 5); got != "plain" {
		t.Errorf("plain truncate = %q", got)
	}
	if got := truncateANSI(styled, 50); stripANSI(got) != "abcdefghij" {
		t.Errorf("no-op truncate changed the text: %q", got)
	}
	if got := truncateANSI("abc", 0); got != "" {
		t.Errorf("zero width = %q", got)
	}
}

func TestFilenameCannotBreakTheFrame(t *testing.T) {
	p := palette{levelTrue}
	for _, name := range []string{
		"dir/a\nb.txt", "dir/a\tb.txt", "dir/\x1b[2J.txt", `C:\dir\file.txt`,
		"directory/日本語.txt", "emoji/👩🏽‍💻.txt", "dir/e\u0301.txt", "dir/file\u202etxt.exe",
	} {
		got := p.filename(name, 30)
		plain := stripANSI(got)
		if strings.ContainsAny(plain, "\n\r\t\x1b\u202e") || strings.Contains(got, "\x1b[2J") {
			t.Errorf("filename changed terminal state: %q", got)
		}
		if n := humanize.Width(plain); n != 30 {
			t.Errorf("filename %q took %d cells, want 30", name, n)
		}
	}
}

func TestTruncateStyledUnicodeKeepsWholeGraphemes(t *testing.T) {
	for _, tt := range []struct {
		text  string
		width int
		want  string
	}{
		{"日本語", 3, "日"},
		{"👩🏽‍💻text", 2, "👩🏽‍💻"},
		{"e\u0301text", 1, "e\u0301"},
	} {
		got := truncateANSI("\x1b[36m"+tt.text+"\x1b[0m", tt.width)
		if plain := stripANSI(got); plain != tt.want {
			t.Errorf("truncated %q to %q, want %q", tt.text, plain, tt.want)
		}
		if !strings.HasSuffix(got, "\x1b[0m") {
			t.Errorf("truncation left an open style: %q", got)
		}
	}
}

// TestBarWidthIsStable guards the layout arithmetic: a bar of N cells must
// measure N cells however it is coloured.
func TestBarWidthIsStable(t *testing.T) {
	for _, lvl := range []colourLevel{levelNone, level16, level256, levelTrue} {
		p := palette{lvl}
		r := &Reporter{pal: p}
		for _, frac := range []float64{0, 0.01, 0.5, 0.999, 1} {
			for _, w := range []int{1, 8, 10, 40} {
				if got := len([]rune(stripANSI(r.plainBar(w, frac)))); got != w {
					t.Fatalf("level %d frac %v width %d: plainBar measured %d",
						lvl, frac, w, got)
				}
				if got := len([]rune(stripANSI(r.gradientBar(w, frac)))); got != w {
					t.Fatalf("level %d frac %v width %d: gradientBar measured %d",
						lvl, frac, w, got)
				}
			}
		}
	}
}
