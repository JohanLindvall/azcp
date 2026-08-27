package azure

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/retryx"
)

// stalledServer answers with headers and a little body, then delivers nothing
// more while keeping the connection open — which is what a wedged response
// looks like from here, and what TCP will never report as broken.
func stalledServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 64))
		w.(http.Flusher).Flush()
		<-release
	}))
	// Cleanups run last-registered-first, so the handler is let go before the
	// server is closed — Close waits for it, and waiting for something that is
	// waiting for Close is how a test hangs instead of failing.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv
}

// A response that stops arriving has to fail, and fail in a way the pipeline
// will try again — and not in a way that looks like the run being cancelled,
// which is how an interrupted transfer reports itself.
func TestStalledResponseFailsAndIsRetryable(t *testing.T) {
	srv := stalledServer(t)
	c := &http.Client{Transport: &stallTransport{
		base:           http.DefaultTransport,
		timeout:        150 * time.Millisecond,
		controlTimeout: 150 * time.Millisecond,
	}}

	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	start := time.Now()
	_, err = io.ReadAll(resp.Body)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a response that stopped arriving was read as if it were complete")
	}
	if elapsed > 2*time.Second {
		t.Errorf("the read took %v to give up, want about 150ms", elapsed)
	}
	if !errors.Is(err, retryx.ErrRetryable) {
		t.Errorf("error %v is not marked retryable, so nothing will try again", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("error %v looks like the run being cancelled, which is how an "+
			"interrupted transfer reports itself", err)
	}
}

// The bound is on delivering nothing, not on taking a long time: a large block
// arriving slowly must not be cut off, which is why there is no per-attempt
// timeout in the first place.
func TestSlowButMovingResponseIsLeftAlone(t *testing.T) {
	const chunks = 12
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1200")
		w.WriteHeader(http.StatusOK)
		for range chunks {
			_, _ = w.Write(make([]byte, 100))
			w.(http.Flusher).Flush()
			time.Sleep(40 * time.Millisecond) // well inside the stall timeout
		}
	}))
	defer srv.Close()

	c := &http.Client{Transport: &stallTransport{
		base:           http.DefaultTransport,
		timeout:        150 * time.Millisecond,
		controlTimeout: 150 * time.Millisecond,
	}}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Total time is far past the stall timeout; no single gap is.
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("a response that kept arriving was cut off: %v", err)
	}
	if len(b) != 1200 {
		t.Errorf("read %d bytes, want 1200", len(b))
	}
}
