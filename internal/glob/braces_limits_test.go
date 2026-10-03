package glob

import (
	"fmt"
	"math"
	"reflect"
	"testing"
)

func TestBraceExpansionLimitsAreErrors(t *testing.T) {
	for _, pattern := range []string{"{1..9000}", "{1..100}{1..100}"} {
		if results, err := ExpandBraces(pattern); err == nil || len(results) != 0 {
			t.Fatalf("partial expansion of %s: %d results, %v", pattern, len(results), err)
		}
	}
}

func TestBraceSequenceCannotWrap(t *testing.T) {
	for _, tc := range []struct{ lo, hi int }{{math.MaxInt - 1, math.MaxInt}, {math.MinInt + 1, math.MinInt}} {
		pattern := fmt.Sprintf("{%d..%d}", tc.lo, tc.hi)
		want := []string{fmt.Sprint(tc.lo), fmt.Sprint(tc.hi)}
		if got, err := ExpandBraces(pattern); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s = %v, %v", pattern, got, err)
		}
	}
	if got, err := ExpandBraces(fmt.Sprintf("{a..z..%d}", math.MinInt)); err != nil || !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("large character step: %v, %v", got, err)
	}
}
