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
// just as importantly, to say nothing when the answer is nothing. Naming
// --resume on its own is worse than saying nothing: it promises to carry on and
// then copies every finished file a second time.
func TestResumeHint(t *testing.T) {
	for _, tc := range []struct {
		name             string
		uploads, downs   int
		opt              cli.Options
		want, wantAbsent string
	}{
		{name: "nothing was in flight", want: ""},
		{name: "uploads, with no flags", uploads: 2,
			want: "--resume -n"},
		{name: "downloads, with no flags", downs: 3,
			want: "--resume -n"},
		{name: "both, with no flags", uploads: 1, downs: 1,
			want: "--resume -n"},
		{name: "--resume alone still needs -n", uploads: 2,
			opt:  cli.Options{Resume: true},
			want: "with -n as well"},
		{name: "--resume -n carries on as it is", downs: 3,
			opt:  cli.Options{Resume: true, NoClobber: true},
			want: "carries on from", wantAbsent: "-n as well"},
		{name: "--resume -u carries on as it is", downs: 3,
			opt:  cli.Options{Resume: true, Update: cli.UpdateOlder},
			want: "carries on from", wantAbsent: "-n as well"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := tc.opt
			got := resumeHint(interruptedRun(t, tc.uploads, tc.downs), &opt)
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
