package cli

import (
	"testing"
	"time"
)

func TestInvalidAgesAreRejected(t *testing.T) {
	for _, value := range []string{"NaNd", "Infd", "-Infd", "1000000d", "-1d", "-1h"} {
		if _, err := ParseTimeSpec(value, time.Now()); err == nil {
			t.Errorf("accepted invalid age %q", value)
		}
	}
}

func TestNegativeTransferDurationsAreRejected(t *testing.T) {
	for _, flag := range []string{"--timeout", "--retry-delay", "--retry-max-delay"} {
		if _, err := Parse([]string{flag + "=-1s", "src", "dst"}); err == nil {
			t.Errorf("accepted negative %s", flag)
		}
	}
}
