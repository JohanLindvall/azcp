// SPDX-License-Identifier: MIT

package engine

import (
	"io"
	"strings"
	"testing"
)

// A pipe gives up its contents once. A second attempt at a stream upload gets
// an error saying so, not whatever happens to be left as if it were the whole.
func TestPipeIsReadOnce(t *testing.T) {
	open := readOnce(io.NopCloser(strings.NewReader("all of it")), "/dev/stdin")
	if b, err := io.ReadAll(open()); err != nil || string(b) != "all of it" {
		t.Fatalf("first read: %q, %v", b, err)
	}
	_, err := io.ReadAll(open())
	if err == nil || !strings.Contains(err.Error(), "cannot read '/dev/stdin' again") {
		t.Errorf("second read: %v", err)
	}
}
