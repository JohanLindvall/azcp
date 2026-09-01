package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

// Two headers given the same text are two headers. An earlier version built
// the set from a map keyed by value, which quietly merged them into one.
func TestHTTPHeadersKeepEveryHeader(t *testing.T) {
	o := TransferOptions{
		ContentEncoding: "en", ContentLanguage: "en",
		CacheControl: "en", ContentDisposition: "en",
	}
	h := o.httpHeaders("x.bin")
	if h == nil {
		t.Fatal("no headers were built")
	}
	for name, got := range map[string]*string{
		"encoding":      h.BlobContentEncoding,
		"language":      h.BlobContentLanguage,
		"cache-control": h.BlobCacheControl,
		"disposition":   h.BlobContentDisposition,
	} {
		if got == nil || *got != "en" {
			t.Errorf("%s header was lost", name)
		}
	}
}

func TestHTTPHeadersOnlyWhenThereIsSomethingToSay(t *testing.T) {
	if h := (TransferOptions{}).httpHeaders("noext"); h != nil {
		t.Errorf("headers for a bare name with nothing set: %+v", h)
	}
	h := (TransferOptions{}).httpHeaders("a.json")
	if h == nil || h.BlobContentType == nil || !strings.HasPrefix(*h.BlobContentType, "application/json") {
		t.Errorf("the type was not guessed from the extension: %+v", h)
	}
	h = (TransferOptions{ContentType: "text/x-custom"}).httpHeaders("a.json")
	if h == nil || h.BlobContentType == nil || *h.BlobContentType != "text/x-custom" {
		t.Errorf("an explicit type did not win over the extension: %+v", h)
	}
	h = (TransferOptions{}).httpHeadersWithMD5("noext", []byte{1, 2, 3})
	if h == nil || len(h.BlobContentMD5) != 3 {
		t.Errorf("a checksum alone did not produce headers: %+v", h)
	}
}

func TestGuessContentType(t *testing.T) {
	if got := guessContentType("data.no-such-extension"); got != "" {
		t.Errorf("unknown extension guessed %q", got)
	}
	// These may come from the system's table or from the fallback here; either
	// way a file that is plainly text must not default to octet-stream.
	for _, name := range []string{"app.log", "notes.md", "conf.yaml", "big.tgz", "x.zst"} {
		if got := guessContentType(name); got == "" {
			t.Errorf("%s: no content type guessed", name)
		}
	}
	if got := guessContentType("x.zst"); got != "application/zstd" {
		t.Errorf("x.zst guessed %q", got)
	}
}

func TestBlockSizeHonoursTheServiceLimits(t *testing.T) {
	var o TransferOptions
	if got := o.blockSize(0); got != defaultBlockSize {
		t.Errorf("default block size = %d", got)
	}
	o.BlockSize = 1
	if got := o.blockSize(0); got != minBlockSize {
		t.Errorf("a tiny block size was not raised to the minimum: %d", got)
	}
	// Fifty thousand blocks of the default do not cover a terabyte, so the
	// block has to grow however small it was asked to be.
	o.BlockSize = defaultBlockSize
	size := int64(1) << 40
	if bs := o.blockSize(size); (size+bs-1)/bs > maxBlocksPerBlob {
		t.Errorf("block size %d needs more than %d blocks for %d bytes", bs, maxBlocksPerBlob, size)
	}
	o.BlockSize = 2 * maxBlockSizeBytes
	if got := o.blockSize(0); got != maxBlockSizeBytes {
		t.Errorf("block size was not capped: %d", got)
	}
	if got := (TransferOptions{}).concurrency(); got != defaultConcurrency {
		t.Errorf("default concurrency = %d", got)
	}
}

func TestOptionsRenderForTheSDK(t *testing.T) {
	var none TransferOptions
	if none.metadata() != nil || none.accessConditions() != nil || none.tier() != nil || none.anyHeader() {
		t.Error("empty options produced something to send")
	}

	o := TransferOptions{
		Metadata:     map[string]string{"a": "1", "b": "2"},
		NoClobber:    true,
		AccessTier:   "Cool",
		CacheControl: "no-cache",
	}
	m := o.metadata()
	if len(m) != 2 || *m["a"] != "1" || *m["b"] != "2" {
		t.Errorf("metadata = %v", m)
	}
	if m["a"] == m["b"] {
		t.Error("two metadata values share one pointer")
	}
	ac := o.accessConditions()
	if ac == nil || ac.ModifiedAccessConditions == nil || *ac.ModifiedAccessConditions.IfNoneMatch != azcore.ETagAny {
		t.Errorf("-n did not become an If-None-Match: * condition: %+v", ac)
	}
	if tier := o.tier(); tier == nil || string(*tier) != "Cool" {
		t.Errorf("tier = %v", tier)
	}
	if !o.anyHeader() {
		t.Error("a cache-control header went unnoticed")
	}
}

func TestUnsupportedByEndpoint(t *testing.T) {
	yes := []error{
		&azcore.ResponseError{StatusCode: http.StatusNotImplemented},
		&azcore.ResponseError{StatusCode: http.StatusBadRequest, ErrorCode: "APINotImplemented"},
		&azcore.ResponseError{StatusCode: http.StatusBadRequest, ErrorCode: "UnsupportedHeader"},
	}
	for _, err := range yes {
		if !unsupportedByEndpoint(err) {
			t.Errorf("%v was not recognised as unsupported", err)
		}
	}
	no := []error{
		&azcore.ResponseError{StatusCode: http.StatusForbidden, ErrorCode: "AuthorizationFailure"},
		&azcore.ResponseError{StatusCode: http.StatusBadRequest, ErrorCode: "InvalidInput"},
		errors.New("connection reset"),
	}
	for _, err := range no {
		if unsupportedByEndpoint(err) {
			t.Errorf("%v was taken for an unsupported operation", err)
		}
	}
}

// A cancelled copy route is not a failed one: no other route is tried.
func TestIsCancellation(t *testing.T) {
	for _, err := range []error{context.Canceled, fmt.Errorf("wrapped: %w", context.DeadlineExceeded)} {
		if !isCancellation(err) {
			t.Errorf("%v was not recognised as a cancellation", err)
		}
	}
	if isCancellation(errors.New("context canceled")) {
		t.Error("an error that merely says so was taken for a cancellation")
	}
}
