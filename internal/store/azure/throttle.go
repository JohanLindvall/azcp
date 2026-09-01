package azure

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// A bulk copy will take everything the link has, which is exactly what makes
// operations teams ban such tools from shared networks. Throttling lives in the
// HTTP transport rather than in the transfer code so that it covers every byte
// of content in both directions without each path having to remember to ask.
//
// What it does not cover is the scan. Listing a large container is megabytes of
// XML, and pacing that means nothing moves at all until the listing has
// trickled through — a limit meant to leave room on a shared link instead
// spends its whole budget on finding out what to copy. The bulk is the point.
//
// A server-side copy cannot be throttled by anything here: the bytes move
// between storage servers and never reach this process. The engine says so
// rather than appearing to work.

// limiter is a token bucket measured in bytes.
type limiter struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	burst  float64
	tokens float64
	last   time.Time
}

func newLimiter(bytesPerSec int64) *limiter {
	if bytesPerSec <= 0 {
		return nil
	}
	// A tenth of a second of traffic is enough burst to keep reads efficient
	// without letting the average drift; the floor keeps small limits usable.
	burst := max(float64(bytesPerSec)/10, 64<<10)
	return &limiter{
		rate:   float64(bytesPerSec),
		burst:  burst,
		tokens: burst,
		last:   time.Now(),
	}
}

// take blocks until n bytes may be transferred, and reports how many it
// actually granted, which is never more than the burst size.
func (l *limiter) take(ctx context.Context, n int) (int, error) {
	if l == nil || n <= 0 {
		return n, nil
	}
	if float64(n) > l.burst {
		n = int(l.burst)
	}
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens = min(l.tokens+now.Sub(l.last).Seconds()*l.rate, l.burst)
		l.last = now
		if l.tokens >= float64(n) {
			l.tokens -= float64(n)
			l.mu.Unlock()
			return n, nil
		}
		deficit := float64(n) - l.tokens
		l.mu.Unlock()

		wait := max(time.Duration(deficit/l.rate*float64(time.Second)), time.Millisecond)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return 0, ctx.Err()
		case <-t.C:
		}
	}
}

// giveBack returns tokens taken for bytes that never arrived.
func (l *limiter) giveBack(n int) {
	if l == nil || n <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tokens = min(l.tokens+float64(n), l.burst)
}

// throttledReader paces a stream through the bucket.
type throttledReader struct {
	r   io.ReadCloser
	lim *limiter
	ctx context.Context
}

func (t *throttledReader) Read(p []byte) (int, error) {
	allowed, err := t.lim.take(t.ctx, len(p))
	if err != nil {
		return 0, err
	}
	n, rerr := t.r.Read(p[:allowed])
	// Pay for the bytes, not for the buffer they were asked for in. A read
	// from a socket returns whatever has arrived — often a single segment
	// against a buffer thousands of times its size — and charging for the
	// buffer throttles the stream to a fraction of the rate. A local emulator
	// never shows this, because it fills every buffer it is handed.
	if n < allowed {
		t.lim.giveBack(allowed - n)
	}
	return n, rerr
}

func (t *throttledReader) Close() error { return t.r.Close() }

// throttledBody keeps the Seeker the SDK needs to rewind a request for a retry.
type throttledBody struct {
	throttledReader
	seeker io.Seeker
}

func (t *throttledBody) Seek(offset int64, whence int) (int64, error) {
	return t.seeker.Seek(offset, whence)
}

// throttledTransport paces every request body out and every response body in.
type throttledTransport struct {
	base http.RoundTripper
	lim  *limiter
}

func (t *throttledTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.lim != nil && req.Body != nil && req.Body != http.NoBody {
		tr := throttledReader{r: req.Body, lim: t.lim, ctx: req.Context()}
		if sk, ok := req.Body.(io.Seeker); ok {
			// The pipeline rewinds a body to retry it, so the wrapper has to
			// stay seekable or every retry would fail.
			req.Body = &throttledBody{throttledReader: tr, seeker: sk}
		} else {
			req.Body = &tr
		}
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || t.lim == nil || resp.Body == nil || isControlOp(req) {
		return resp, err
	}
	resp.Body = &throttledReader{r: resp.Body, lim: t.lim, ctx: req.Context()}
	return resp, nil
}

// isControlOp reports whether a request asks about names rather than contents.
// Listings, properties, metadata and block lists are all addressed with a
// "comp" parameter and none of them carries bulk data, so their answers are not
// paced.
//
// This is asked of responses only. Staging a block is PUT ...&comp=block, whose
// body is exactly the bulk the limit exists for.
func isControlOp(req *http.Request) bool {
	return req.URL.Query().Has("comp")
}
