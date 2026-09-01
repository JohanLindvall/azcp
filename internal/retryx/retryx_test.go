package retryx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

func respErr(status int, code string, hdr http.Header) error {
	if hdr == nil {
		hdr = http.Header{}
	}
	return &azcore.ResponseError{
		StatusCode:  status,
		ErrorCode:   code,
		RawResponse: &http.Response{StatusCode: status, Header: hdr},
	}
}

func TestIsTransient(t *testing.T) {
	transient := []error{
		respErr(503, "ServerBusy", nil),
		respErr(429, "", nil),
		respErr(500, "InternalError", nil),
		respErr(408, "", nil),
		respErr(200, "OperationTimedOut", nil),
		io.ErrUnexpectedEOF,
		fmt.Errorf("read: %w", syscall.ECONNRESET),
		fmt.Errorf("wrapped: %w", syscall.EMFILE),
		&net.DNSError{Err: "server misbehaving", IsTemporary: true},
		&net.OpError{Op: "dial", Err: errors.New("tls: bad record")},
	}
	for _, err := range transient {
		if !IsTransient(err) {
			t.Errorf("IsTransient(%v) = false, want true", err)
		}
	}
	permanent := []error{
		nil,
		respErr(404, "BlobNotFound", nil),
		respErr(403, "AuthorizationPermissionMismatch", nil),
		respErr(401, "NoAuthenticationInformation", nil),
		respErr(409, "ContainerAlreadyExists", nil),
		context.Canceled,
		fmt.Errorf("ctx: %w", context.DeadlineExceeded),
		fmt.Errorf("disk: %w", syscall.ENOSPC),
		fmt.Errorf("perm: %w", syscall.EACCES),
		&net.DNSError{Err: "no such host", IsNotFound: true},
		errors.New("some parse failure"),
	}
	for _, err := range permanent {
		if IsTransient(err) {
			t.Errorf("IsTransient(%v) = true, want false", err)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	d, ok := RetryAfter(respErr(429, "", http.Header{"Retry-After": []string{"7"}}))
	if !ok || d != 7*time.Second {
		t.Errorf("RetryAfter seconds = %v, %v", d, ok)
	}
	if _, ok := RetryAfter(respErr(429, "", nil)); ok {
		t.Error("RetryAfter should be absent without the header")
	}
	if _, ok := RetryAfter(io.EOF); ok {
		t.Error("RetryAfter on non-HTTP error")
	}
}

func TestDoRetriesTransientThenSucceeds(t *testing.T) {
	calls := 0
	var notified int
	p := Policy{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: 4 * time.Millisecond}
	err := Do(context.Background(), p, func(int, time.Duration, error) { notified++ },
		func(context.Context) error {
			calls++
			if calls < 3 {
				return respErr(503, "ServerBusy", nil)
			}
			return nil
		})
	if err != nil || calls != 3 || notified != 2 {
		t.Fatalf("err=%v calls=%d notified=%d", err, calls, notified)
	}
}

func TestDoStopsOnPermanent(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Policy{MaxAttempts: 5, BaseDelay: time.Millisecond},
		nil, func(context.Context) error {
			calls++
			return respErr(404, "BlobNotFound", nil)
		})
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want one attempt", err, calls)
	}
}

func TestDoExhaustsAttempts(t *testing.T) {
	calls := 0
	p := Policy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}
	err := Do(context.Background(), p, nil, func(context.Context) error {
		calls++
		return respErr(503, "ServerBusy", nil)
	})
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	var re *azcore.ResponseError
	if !errors.As(err, &re) {
		t.Fatalf("final error lost its cause: %v", err)
	}
}

func TestDoHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Do(ctx, Policy{MaxAttempts: 10, BaseDelay: 50 * time.Millisecond}, nil,
		func(context.Context) error {
			calls++
			cancel()
			return respErr(503, "ServerBusy", nil)
		})
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestDelayBounds(t *testing.T) {
	p := Policy{MaxAttempts: 10, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}.normalise()
	for attempt := 1; attempt <= 10; attempt++ {
		for i := 0; i < 50; i++ {
			d := p.delay(attempt)
			if d <= 0 || d > p.MaxDelay {
				t.Fatalf("attempt %d: delay %v out of bounds", attempt, d)
			}
		}
	}
}

// Describe collapses an Azure failure to what identifies it; anything else
// keeps its own words, which for a filesystem error already name the path.
func TestDescribe(t *testing.T) {
	cases := map[string]error{
		"HTTP 503 ServerBusy": respErr(503, "ServerBusy", nil),
		"HTTP 500":            respErr(500, "", nil),
		"HTTP 429 X":          fmt.Errorf("after 3 attempts: %w", respErr(429, "X", nil)),
		"disk full":           errors.New("disk full"),
	}
	for want, err := range cases {
		if got := Describe(err); got != want {
			t.Errorf("Describe(%v) = %q, want %q", err, got, want)
		}
	}
}

// The status list is the Azure SDK's own, so substituting this predicate for
// the SDK's changes nothing about which requests are retried.
func TestRetryableStatus(t *testing.T) {
	cases := map[int]bool{
		408: true, 429: true, 500: true, 502: true, 503: true, 504: true,
		200: false, 400: false, 401: false, 403: false, 404: false, 409: false, 501: false,
	}
	for code, want := range cases {
		if got := RetryableStatus(code); got != want {
			t.Errorf("RetryableStatus(%d) = %v, want %v", code, got, want)
		}
	}
}
