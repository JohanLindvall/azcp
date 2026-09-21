package retryx

import (
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

func TestBackoffSaturatesWithoutOverflow(t *testing.T) {
	p := Policy{BaseDelay: 1 << 62, MaxDelay: time.Duration(math.MaxInt64)}
	for _, attempt := range []int{1, 2, 3, 100} {
		d := p.delay(attempt)
		if d < 0 || d > p.MaxDelay {
			t.Fatalf("attempt %d: delay = %v", attempt, d)
		}
	}
}

func TestRetryAfterSaturatesWithoutOverflow(t *testing.T) {
	err := &azcore.ResponseError{RawResponse: &http.Response{
		Header: http.Header{"Retry-After": []string{strconv.FormatInt(math.MaxInt64, 10)}},
	}}
	if d, ok := RetryAfter(err); !ok || d != time.Duration(math.MaxInt64) {
		t.Fatalf("delay = %v, ok = %v", d, ok)
	}
}
