// Package retryx classifies transient failures and retries operations with
// bounded, jittered exponential backoff.
//
// The classification is deliberately conservative: an error is retried only
// when repeating the request could plausibly succeed. Authentication failures,
// missing blobs, permission problems and a full disk are all permanent, and
// retrying them only delays the report the user needs.
package retryx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

// Policy bounds a retry loop.
type Policy struct {
	// MaxAttempts counts the first try. 1 disables retrying.
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// Default is the policy used when the user does not override it.
var Default = Policy{MaxAttempts: 6, BaseDelay: 300 * time.Millisecond, MaxDelay: 30 * time.Second}

func (p Policy) normalise() Policy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = Default.MaxAttempts
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = Default.BaseDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = Default.MaxDelay
	}
	return p
}

// Notify is invoked before sleeping between attempts. attempt is the number of
// the attempt that just failed, starting at 1.
type Notify func(attempt int, delay time.Duration, err error)

// Do runs fn until it succeeds, returns a permanent error, or runs out of
// attempts. The error returned is the last one observed, wrapped with the
// attempt count when more than one attempt was made.
func Do(ctx context.Context, p Policy, notify Notify, fn func(ctx context.Context) error) error {
	p = p.normalise()
	var err error
	for attempt := 1; ; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if err != nil {
				return err
			}
			return ctxErr
		}
		err = fn(ctx)
		if err == nil {
			return nil
		}
		if attempt >= p.MaxAttempts || !IsTransient(err) {
			if attempt > 1 {
				return fmt.Errorf("after %d attempts: %w", attempt, err)
			}
			return err
		}
		delay := p.delay(attempt)
		if d, ok := RetryAfter(err); ok && d > delay {
			delay = min(d, p.MaxDelay*4) // honour the server, within reason
		}
		if notify != nil {
			notify(attempt, delay, err)
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}

// delay computes the backoff for the given attempt using equal jitter: half the
// exponential window plus a random amount up to the other half. Equal jitter
// spreads retries without collapsing to near-zero waits the way full jitter
// can.
func (p Policy) delay(attempt int) time.Duration {
	window := p.BaseDelay
	for i := 1; i < attempt && window < p.MaxDelay; i++ {
		window *= 2
	}
	window = min(window, p.MaxDelay)
	half := window / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// ErrRetryable marks an error as worth another attempt when the failure is
// domain knowledge this package does not have — a checksum that did not match,
// for instance, which says the bytes arrived wrong and may well arrive right
// the second time.
var ErrRetryable = errors.New("retryable")

// IsTransient reports whether err is worth retrying.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrRetryable) {
		return true
	}
	// A cancelled or expired context is the caller giving up, never a hint to
	// try again.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		return isRetryableStatus(respErr.StatusCode) || isRetryableCode(respErr.ErrorCode)
	}

	// A truncated response body: the connection dropped mid-transfer.
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// A name that does not exist is a typo in the account, not a blip.
		return !dnsErr.IsNotFound
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ECONNREFUSED,
			syscall.EPIPE, syscall.ETIMEDOUT, syscall.EHOSTUNREACH,
			syscall.ENETUNREACH, syscall.ENETRESET, syscall.ENETDOWN,
			syscall.EAGAIN, syscall.EINTR:
			return true
		case syscall.EMFILE, syscall.ENFILE:
			// Descriptor exhaustion is self-inflicted and transient: another
			// in-flight transfer will finish and free one up.
			return true
		}
		return false
	}

	// net.OpError that is not a timeout and carries no errno (for example a
	// broken TLS session) is still a connection-level fault worth one retry.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return false
}

// RetryableStatus reports whether an HTTP status justifies another attempt.
// It matches the Azure SDK's own default list, so callers can substitute their
// own retry predicate without changing behaviour.
func RetryableStatus(code int) bool { return isRetryableStatus(code) }

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, // 408
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// isRetryableCode covers Azure error codes that can appear with status codes we
// would otherwise treat as permanent.
func isRetryableCode(code string) bool {
	switch code {
	case "ServerBusy", "InternalError", "OperationTimedOut",
		"PendingCopyOperation", "SnapshotOperationRateExceeded",
		"ConcurrentSnapshotOperationInProgress", "RequestTimeout":
		return true
	}
	return false
}

// RetryAfter extracts a server-supplied delay from an HTTP error response.
func RetryAfter(err error) (time.Duration, bool) {
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) || respErr.RawResponse == nil {
		return 0, false
	}
	v := respErr.RawResponse.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, convErr := strconv.Atoi(v); convErr == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if t, convErr := http.ParseTime(v); convErr == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// Describe returns a short, log-friendly rendering of err. An Azure failure
// collapses to its status and error code, because the SDK's own message is a
// multi-line dump of the whole exchange; anything else keeps its own text,
// which for a filesystem error already names the path.
func Describe(err error) string {
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		if respErr.ErrorCode != "" {
			return fmt.Sprintf("HTTP %d %s", respErr.StatusCode, respErr.ErrorCode)
		}
		return fmt.Sprintf("HTTP %d", respErr.StatusCode)
	}
	return err.Error()
}
