// SPDX-License-Identifier: MIT

package azure

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/azcp/internal/store"
)

func TestRemoveBlobUsesTheSelectedRevisionAndNeverFallsBack(t *testing.T) {
	for _, status := range []int{http.StatusAccepted, http.StatusNotFound, http.StatusPreconditionFailed} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "DELETE" || r.URL.Path != "/account/c/name" || r.Header.Get("If-Match") != `"selected-etag"` {
					t.Errorf("unexpected request: %s %s If-Match=%q", r.Method, r.URL.Path, r.Header.Get("If-Match"))
				}
				if status == http.StatusAccepted {
					w.WriteHeader(status)
				} else if status == http.StatusNotFound {
					refuse(w, status, "BlobNotFound")
				} else {
					refuse(w, status, "ConditionNotMet")
				}
			}))
			defer srv.Close()
			s := newTestStore(t, func() { t.Error("unexpected sign-in") })
			s.creds.Mode = AuthAnonymous
			s.cfg.MaxRetries = -1
			n := &store.Node{URL: mustURL(t, srv.URL+"/account/c/name"), Kind: store.KindFile, ETag: `"selected-etag"`}
			err := s.RemoveBlob(t.Context(), n)
			if (err == nil) != (status == http.StatusAccepted) {
				t.Fatalf("status %d: %v", status, err)
			}
			if calls.Load() != 1 {
				t.Fatalf("made %d requests; deletion must not fall back to a marker", calls.Load())
			}
		})
	}
}
