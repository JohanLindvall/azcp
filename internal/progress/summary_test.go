package progress

import (
	"bytes"
	"strings"
	"testing"
)

// The closing line reads as a sentence, agrees in number, and says what did
// not go to plan.
func TestSummaryReadsNaturally(t *testing.T) {
	r, _ := newTestReporter(t)
	var out bytes.Buffer
	r.Summary(&out, false)
	if s := out.String(); !strings.Contains(s, "Copied 3 files") || !strings.Contains(s, "256 KiB") {
		t.Errorf("summary = %q", s)
	}

	r, _ = newTestReporter(t)
	r.doneFiles.Store(1)
	r.skippedFiles.Store(2)
	r.failedFiles.Store(1)
	r.retries.Store(1)
	out.Reset()
	r.Summary(&out, true)
	s := out.String()
	for _, want := range []string{"Would copy 1 file ", "2 skipped", "1 failed", "1 transient error retried"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary %q lacks %q", s, want)
		}
	}
	if strings.Contains(s, "/s") {
		t.Errorf("a dry run reported a rate: %q", s)
	}

	// No display, no summary: cp is silent on success and scripts read stderr.
	out.Reset()
	New(Config{Mode: ModeNever}).Summary(&out, false)
	if out.Len() != 0 {
		t.Errorf("a disabled reporter wrote %q", out.String())
	}
}
