package azure

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
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
		DisableCompression:    true,
		MaxIdleConns:          idle * 2, // a run may touch two accounts
		MaxIdleConnsPerHost:   idle,
		MaxConnsPerHost:       0, // unbounded: the engine already limits itself
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	if lim := newLimiter(bytesPerSec); lim != nil {
		return &http.Client{Transport: &throttledTransport{base: transport, lim: lim}}
	}
	return &http.Client{Transport: transport}
}
