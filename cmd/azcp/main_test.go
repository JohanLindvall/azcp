package main

import (
	"strings"
	"testing"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/progress"
)

// interruptedRun stages a reporter that has had transfers cut off part-way.
func interruptedRun(t *testing.T, uploads, downloads int) *progress.Reporter {
	t.Helper()
	p := progress.New(progress.Config{Mode: progress.ModeNever})
	stop := func(n int, dir progress.Direction) {
		for range n {
			tk := p.Begin("some/file", 1000, dir)
			tk.Set(500) // half of it arrived
			tk.Interrupted()
		}
	}
	stop(uploads, progress.DirUpload)
	stop(downloads, progress.DirDownload)
	return p
}

// Stopping a transfer is the moment to say what can be picked up again — and,
// just as importantly, to say nothing when the answer is nothing.
func TestResumeHint(t *testing.T) {
	for _, tc := range []struct {
		name             string
		uploads, downs   int
		resume           bool
		want, wantAbsent string
	}{
		{name: "nothing was in flight", want: ""},
		{name: "uploads, without --resume", uploads: 2, want: "add --resume"},
		{name: "uploads, with --resume", uploads: 2, resume: true,
			want: "run again", wantAbsent: "add --resume"},
		{name: "downloads, without --resume", downs: 3, want: "with --resume"},
		{name: "downloads, with --resume", downs: 3, resume: true, want: "run again"},
		{name: "both, without --resume", uploads: 1, downs: 1, want: "with --resume"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := &cli.Options{Resume: tc.resume}
			got := resumeHint(interruptedRun(t, tc.uploads, tc.downs), opt)
			if tc.want == "" {
				if got != "" {
					t.Errorf("hint = %q, want nothing to be said", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("hint = %q, want it to mention %q", got, tc.want)
			}
			if tc.wantAbsent != "" && strings.Contains(got, tc.wantAbsent) {
				t.Errorf("hint = %q, should not mention %q", got, tc.wantAbsent)
			}
			// However it is worded, it has to say how much is at stake.
			if n := tc.uploads + tc.downs; !strings.Contains(got, itoa(n)) {
				t.Errorf("hint = %q, want the count %d in it", got, n)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
