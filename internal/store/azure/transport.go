package azure

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/JohanLindvall/azcp/internal/retryx"
)

// The SDK's default client keeps ten idle connections per host. That is a
// sensible figure for a control-plane client making occasional calls, and quite
// wrong for a copier: with `--jobs × --part-concurrency` requests in flight
// against a single storage account, every connection past the tenth is closed
// the moment it goes idle and has to be built again for the next request — a
// TCP handshake and a TLS negotiation apiece.
//
// On a local emulator that costs nothing and hides the problem entirely. On a
// real account, at twenty to fifty milliseconds away, re-establishing a
// connection costs more than transferring a small blob over it.

// A connection that stops delivering is not the same as one that breaks. TCP
// keeps a silent socket alive indefinitely — keepalives answer, queues stay
// empty, nothing errors — so a response that stops mid-body leaves the reader
// waiting for ever. One wedged listing stops a whole scan, since the walk is
// ordered; one wedged range read leaves a file at 95% and a run that never
// ends. There is no per-attempt timeout to fall back on either, deliberately:
// a large block may legitimately take minutes.
//
// What is never legitimate is delivering nothing at all. stallTimeout bounds
// that and nothing else, and expiring counts as transient, so the SDK reissues
// the request on a fresh connection and the run carries on.
const stallTimeout = 60 * time.Second

// errStalled is what a stalled attempt fails with. It is marked retryable so
// the pipeline treats it as the blip it is.
var errStalled = fmt.Errorf("%w: nothing received for %s", retryx.ErrRetryable, stallTimeout)

// maxPooledConns bounds the pool so an extravagant --jobs cannot leave the
// process holding thousands of sockets.
const maxPooledConns = 512

// minPooledConns keeps a useful pool even for a small, serial copy.
const minPooledConns = 32

// newHTTPClient builds a client whose connection pool matches how many requests
// this run can have outstanding at once.
func newHTTPClient(peakRequests int, bytesPerSec int64) *http.Client {
	idle := min(max(peakRequests, minPooledConns), maxPooledConns)

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// Storage is served over HTTP/1.1 in practice, and a pool of separate
		// connections moves bulk data better than one multiplexed stream.
		ForceAttemptHTTP2: false,
		// Left on, Go advertises gzip and silently expands any response that
		// comes back encoded. For a copier that is corruption: a blob stored
		// with Content-Encoding: gzip would arrive expanded, under its .gz
		// name, and shorter than the length the service reported for it. The
		// bytes that are stored are the bytes that must land.
		DisableCompression:  true,
		MaxIdleConns:        idle * 2, // a run may touch two accounts
		MaxIdleConnsPerHost: idle,
		MaxConnsPerHost:     0, // unbounded: the engine already limits itself
		IdleConnTimeout:     90 * time.Second,
		// A request that is written and then answered with silence is the same
		// failure as one that stops mid-body, and this is the only bound Go
		// offers for it.
		ResponseHeaderTimeout: stallTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	// The stall guard sits under the throttle, so that waiting for the bucket
	// is never mistaken for a connection that has stopped delivering.
	var rt http.RoundTripper = &stallTransport{base: transport, timeout: stallTimeout}
	if lim := newLimiter(bytesPerSec); lim != nil {
		rt = &throttledTransport{base: rt, lim: lim}
	}
	return &http.Client{Transport: rt}
}

// stallTransport abandons a response that stops arriving.
type stallTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

func (t *stallTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A cause rather than a plain cancellation: what comes back through the
	// SDK is what the context was cancelled with, and "context canceled" is
	// how a run being interrupted reports itself. A stall is not that.
	ctx, cancel := context.WithCancelCause(req.Context())
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil || resp.Body == nil {
		cancel(errStalled)
		return resp, err
	}
	resp.Body = &stallGuard{body: resp.Body, cancel: cancel, timeout: t.timeout}
	return resp, nil
}

// stallGuard fails a read that delivers nothing for too long. The clock is per
// read, not per response, so a transfer that keeps arriving may take as long as
// it likes.
type stallGuard struct {
	body    io.ReadCloser
	cancel  context.CancelCauseFunc
	timeout time.Duration
	timer   *time.Timer
}

func (g *stallGuard) Read(p []byte) (int, error) {
	if g.timer == nil {
		g.timer = time.AfterFunc(g.timeout, func() { g.cancel(errStalled) })
	} else {
		g.timer.Reset(g.timeout)
	}
	n, err := g.body.Read(p)
	g.timer.Stop()
	return n, err
}

func (g *stallGuard) Close() error {
	if g.timer != nil {
		g.timer.Stop()
	}
	err := g.body.Close()
	// Releases whatever the request still holds; the body is done with either
	// way, so this can only tidy up.
	g.cancel(errStalled)
	return err
}
