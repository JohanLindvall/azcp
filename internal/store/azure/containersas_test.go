package azure

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"github.com/JohanLindvall/azcp/internal/retryx"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// scopedSAS is the blob service as a SAS scoped to one container (sr=c) sees
// it. The reference for a service SAS is blunt about what one is not for:
// "Containers, queues, and tables can't be created, deleted, or listed.
// Container metadata and properties can't be read or written." Everything
// inside the container is served — every blob operation, and List Blobs — and
// anything about the container itself, or the account above it, is a 403
// whether the container is there or not.
type scopedSAS struct {
	blobs *fakeBlobs
	// list, when set, is what a listing is answered with instead of being
	// served, which is how this becomes a token for some other container.
	list *answer

	mu     sync.Mutex
	denied map[string]int
	// limits is the maxresults each listing asked for, in the order they came.
	limits []string
}

// answer is what the fake says in place of serving a request.
type answer struct {
	status int
	code   string
}

// The two refusals are the emulator's, which enforces the same rule the
// service does: one code for what such a token may not do, another for a token
// that is not this container's at all. That they differ is what lets a test
// tell which refusal it was handed.
var (
	notPermitted = &answer{http.StatusForbidden, "AuthorizationPermissionMismatch"}
	noAccess     = &answer{http.StatusForbidden, "AuthorizationFailure"}
	beingDeleted = &answer{http.StatusConflict, "ContainerBeingDeleted"}
)

func (f *scopedSAS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	switch {
	case len(parts) < 2 || parts[1] == "":
		f.deny(w, "account", notPermitted)
	case q.Get("restype") == "container" && q.Get("comp") != "list":
		f.deny(w, r.Method+" container", notPermitted)
	case q.Get("restype") == "container" && f.list != nil:
		f.deny(w, "list", f.list)
	default:
		if q.Get("comp") == "list" {
			f.mu.Lock()
			f.limits = append(f.limits, q.Get("maxresults"))
			f.mu.Unlock()
		}
		f.blobs.ServeHTTP(w, r)
	}
}

func (f *scopedSAS) deny(w http.ResponseWriter, kind string, a *answer) {
	f.mu.Lock()
	if f.denied == nil {
		f.denied = map[string]int{}
	}
	f.denied[kind]++
	f.mu.Unlock()
	refuse(w, a.status, a.code)
}

// refusals counts the requests of one kind that were turned away.
func (f *scopedSAS) refusals(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.denied[kind]
}

// listLimits is the maxresults of every listing that was served.
func (f *scopedSAS) listLimits() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.limits)
}

// refused names every kind of request that was turned away.
func (f *scopedSAS) refused() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.denied))
}

// The refusal arrives under more than one code — the emulator alone has two,
// and an account SAS of the wrong resource type a third — and none of them
// says whether the container is there. The status is what is read.
func TestForbiddenGoesByTheStatus(t *testing.T) {
	for _, code := range []string{"AuthorizationFailure", "AuthorizationPermissionMismatch",
		"AuthorizationResourceTypeMismatch", ""} {
		err := error(&azcore.ResponseError{StatusCode: http.StatusForbidden, ErrorCode: code})
		if !isForbidden(err) || !isForbidden(fmt.Errorf("check container: %w", err)) {
			t.Errorf("a 403 %s was not recognised", code)
		}
		if isNotFound(err) {
			t.Errorf("a 403 %s was taken for something that is not there", code)
		}
	}
	for _, err := range []error{
		nil,
		errors.New("connection reset"),
		rejected(http.StatusUnauthorized, "InvalidAuthenticationInfo"),
		rejected(http.StatusNotFound, "ContainerNotFound"),
		rejected(http.StatusConflict, "ContainerBeingDeleted"),
	} {
		if isForbidden(err) {
			t.Errorf("isForbidden(%v) = true", err)
		}
	}
}

// Such a token cannot be told whether its container exists, but it can list
// it, and a listing that answers is a container that is there — an empty one
// included. One listing of one name, because this is asked of every source and
// destination and costs a round trip each time.
func TestStatOfAContainerFallsBackToAListing(t *testing.T) {
	f := newFakeBlobs()
	f.put("c", "state.json", []byte("{}"), nil)
	f.put("c", "logs/app.log", []byte("line"), nil)
	f.containers["empty"] = map[string]fakeBlob{}
	sas := &scopedSAS{blobs: f}
	s, at := fakeStore(t, sas, false)
	ctx := context.Background()

	for i, p := range []string{"c", "c/", "empty"} {
		n, err := s.Stat(ctx, at(p), false)
		if err != nil {
			t.Fatalf("Stat(%q) = %v; the container can be listed, so it is there", p, err)
		}
		if !n.IsDir() || n.URL.PathPart() != strings.TrimSuffix(p, "/") {
			t.Errorf("Stat(%q) = %+v, want the container as a directory", p, n)
		}
		// Only the properties carry it, and nothing reads a container's.
		if !n.ModTime.IsZero() {
			t.Errorf("Stat(%q) invented a modification time: %v", p, n.ModTime)
		}
		if asked, listed := sas.refusals("GET container"), f.count("list"); asked != i+1 || listed != i+1 {
			t.Errorf("after %d containers: %d questions and %d listings, want one of each apiece",
				i+1, asked, listed)
		}
	}
	if got := sas.listLimits(); !slices.Equal(got, []string{"1", "1", "1"}) {
		t.Errorf("the listings asked for %v names; one is enough to know", got)
	}
}

// Refused is not the same as absent, and the listing is what says which: a
// container that is not there is still reported as that, with the service's
// own word for it.
func TestStatOfAMissingContainerThroughTheListing(t *testing.T) {
	sas := &scopedSAS{blobs: newFakeBlobs()}
	s, at := fakeStore(t, sas, false)

	n, err := s.Stat(context.Background(), at("missing"), false)
	if !store.IsNotExist(err) || !strings.Contains(err.Error(), "ContainerNotFound") {
		t.Errorf("Stat of a missing container = %v, %v; want ContainerNotFound as not-exist", n, err)
	}
	if sas.refusals("GET container") != 1 || sas.blobs.count("list") != 1 {
		t.Errorf("cost %d questions and %d listings, want one of each",
			sas.refusals("GET container"), sas.blobs.count("list"))
	}
}

// A listing refused as well means no access at all, and what comes back has to
// be the first refusal exactly as the service gave it: the sign-in logic reads
// its status, and the message to the user names its code.
func TestStatKeepsTheFirstRefusalWhenTheListingIsRefusedToo(t *testing.T) {
	f := newFakeBlobs()
	f.put("c", "state.json", nil, nil)
	sas := &scopedSAS{blobs: f, list: noAccess}
	s, at := fakeStore(t, sas, false)
	ctx := context.Background()

	_, err := s.stat(ctx, at("c"))
	var re *azcore.ResponseError
	if !errors.As(err, &re) {
		t.Fatalf("stat = %v, want the service's refusal", err)
	}
	if re.StatusCode != http.StatusForbidden || re.ErrorCode != "AuthorizationPermissionMismatch" {
		t.Errorf("stat was refused with %d %s, want the 403 AuthorizationPermissionMismatch "+
			"the properties were refused with", re.StatusCode, re.ErrorCode)
	}
	if re.RawResponse == nil || re.RawResponse.Request.URL.Query().Get("comp") == "list" {
		t.Error("the refusal handed back is the listing's, not the first one")
	}
	if store.IsNotExist(err) {
		t.Error("a refusal was reported as a container that is not there")
	}
	if sas.refusals("list") != 1 {
		t.Errorf("the listing was tried %d times, want once", sas.refusals("list"))
	}

	// Through the front door it reads as it always did for a SAS or a key the
	// account turns away.
	_, err = s.Stat(ctx, at("c"), false)
	if err == nil || !strings.Contains(err.Error(), "credentials given for it") ||
		!strings.Contains(err.Error(), "HTTP 403 AuthorizationPermissionMismatch") {
		t.Errorf("Stat = %v, want the rejected-credentials message naming the first refusal", err)
	}
}

// A listing that fails for a reason of its own is that failure, not the refusal
// before it. To such a token the refusal is routine, and "check the SAS token"
// said of a container that is being deleted sends the user to the wrong place.
func TestStatReportsAListingThatFailsForItsOwnReasons(t *testing.T) {
	f := newFakeBlobs()
	f.put("c", "state.json", nil, nil)
	s, at := fakeStore(t, &scopedSAS{blobs: f, list: beingDeleted}, false)

	_, err := s.Stat(context.Background(), at("c"), false)
	if got := retryx.Describe(err); err == nil || got != "HTTP 409 ContainerBeingDeleted" {
		t.Errorf("Stat = %v, want the listing's own HTTP 409 ContainerBeingDeleted", err)
	}
}

// identityStore is a store holding cred as its identity — nobody at all where
// cred is nil — pointed at h. The endpoint speaks TLS because the SDK will not
// send a bearer token to one that does not, and sign-ins are counted rather
// than performed.
func identityStore(t *testing.T, h http.Handler, cred azcore.TokenCredential,
	signIns *atomic.Int32) (*Store, *uri.URL) {

	t.Helper()
	// A SAS or a key in the developer's environment would be used in place of
	// the identity under test.
	for _, name := range []string{
		"AZURE_STORAGE_CONNECTION_STRING", "AZURE_STORAGE_SAS_TOKEN", "AZURE_STORAGE_SAS",
		"AZURE_STORAGE_KEY", "AZURE_STORAGE_ACCOUNT_KEY",
	} {
		t.Setenv(name, "")
	}
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)

	s := newTestStore(t, func() { signIns.Add(1) })
	s.creds.resumeFn = func(context.Context) (azcore.TokenCredential, bool) { return nil, false }
	s.creds.mu.Lock()
	s.creds.resolved, s.creds.cred = true, cred
	s.creds.mu.Unlock()
	s.clientOnce.Do(func() { s.http = srv.Client() })

	u, err := uri.Parse(srv.URL+"/devstoreaccount1/c", uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return s, u
}

// With the first refusal intact, a credential that has no access is dealt with
// exactly as before the listing was ever tried: a missing role is explained
// and not answered with a sign-in, and nobody being signed in is answered with
// the run's one sign-in.
func TestARefusedContainerStillReachesTheSignInLogic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cred    azcore.TokenCredential
		signIns int32
	}{
		{"an identity without the role", stubCredential{}, 0},
		{"nobody signed in", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeBlobs()
			f.put("c", "state.json", nil, nil)
			var signIns atomic.Int32
			s, u := identityStore(t, &scopedSAS{blobs: f, list: noAccess}, tc.cred, &signIns)

			_, err := s.Stat(context.Background(), u, false)
			if err == nil || !strings.Contains(err.Error(), "not allowed to do this") ||
				!strings.Contains(err.Error(), "HTTP 403 AuthorizationPermissionMismatch") {
				t.Errorf("Stat = %v, want the missing-role message naming the first refusal", err)
			}
			if got := signIns.Load(); got != tc.signIns {
				t.Errorf("interactive sign-ins = %d, want %d", got, tc.signIns)
			}
		})
	}
}

// A token that may write every blob in a container may still not ask about the
// container, so the refusal is no reason to stop — nor to ask again for the
// next file, nor to try creating what such a token cannot create. The write is
// what reports a container that is not there.
func TestMkdirAllLeavesARefusedContainerToTheWrite(t *testing.T) {
	for _, create := range []bool{false, true} {
		name := "as it is"
		if create {
			name = "with --create-container"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeBlobs()
			f.put("c", "seed", nil, nil)
			sas := &scopedSAS{blobs: f}
			s, at := fakeStore(t, sas, create)
			ctx := context.Background()

			for _, p := range []string{"c/dir", "c/other", "c"} {
				if err := s.MkdirAll(ctx, at(p), 0); err != nil {
					t.Errorf("MkdirAll(%q) = %v; a refusal says nothing about the container", p, err)
				}
			}
			if got := sas.refusals("GET container"); got != 1 {
				t.Errorf("three uses of one container cost %d questions; want 1", got)
			}
			if got := sas.refusals("PUT container"); got != 0 {
				t.Errorf("tried %d times to create a container this token cannot create", got)
			}

			src := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(src, []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := s.Upload(ctx, src, at("c/state.json"), TransferOptions{}); err != nil {
				t.Fatalf("the write the check was for: %v", err)
			}
			if b, ok := f.blob("c", "state.json"); !ok || string(b.data) != "{}" {
				t.Errorf("uploaded %q, %v", b.data, ok)
			}

			if err := s.MkdirAll(ctx, at("missing/dir"), 0); err != nil {
				t.Errorf("MkdirAll in a missing container = %v; this token cannot know", err)
			}
			err := s.Upload(ctx, src, at("missing/state.json"), TransferOptions{})
			if got := retryx.Describe(err); err == nil || got != "HTTP 404 ContainerNotFound" {
				t.Errorf("a write into a missing container gave %v, want HTTP 404 ContainerNotFound", err)
			}
			if got := sas.refusals("PUT container"); got != 0 {
				t.Errorf("tried %d times to create the missing container", got)
			}
		})
	}
}

// Refused is one answer among several, and the others mean what they did. A
// container that is not there is an error, or created when that was asked
// for, and an answer that is neither yes nor no still stops the copy.
func TestMkdirAllStillTellsMissingFromRefused(t *testing.T) {
	ctx := context.Background()

	f := newFakeBlobs()
	s, at := fakeStore(t, f, false)
	err := s.MkdirAll(ctx, at("new/dir"), 0)
	if err == nil || !strings.Contains(err.Error(), "does not exist") ||
		!strings.Contains(err.Error(), "--create-container") {
		t.Errorf("a missing container gave %v; want it named as missing, with the hint", err)
	}
	if f.count("PUT container") != 0 {
		t.Error("a container was created without being asked for")
	}

	s, at = fakeStore(t, f, true)
	if err := s.MkdirAll(ctx, at("new/dir"), 0); err != nil {
		t.Errorf("--create-container: %v", err)
	}
	if f.count("PUT container") != 1 {
		t.Errorf("--create-container made the container %d times", f.count("PUT container"))
	}

	// fakeAccount answers everything about this container with a 409.
	s, at = fakeStore(t, &fakeAccount{failOn: "busy"}, true)
	err = s.MkdirAll(ctx, at("busy/dir"), 0)
	if err == nil || !strings.Contains(err.Error(), "check container") ||
		retryx.Describe(err) != "HTTP 409 ContainerBeingDeleted" {
		t.Errorf("a container that could not be checked gave %v; want that reported", err)
	}
}

// Everything else a copy does stays inside the container, where such a token
// is welcome. A question about the container or the account slipped into any
// of it would go unnoticed with every other credential, and be a 403 with this
// one.
func TestNamingStaysInsideTheContainer(t *testing.T) {
	f := newFakeBlobs()
	f.put("c", "state.json", []byte("{}"), nil)
	f.put("c", "logs/app.log", []byte("line"), nil)
	sas := &scopedSAS{blobs: f}
	s, at := fakeStore(t, sas, false)
	ctx := context.Background()

	if _, err := s.Stat(ctx, at("c/state.json"), false); err != nil {
		t.Errorf("Stat of a blob: %v", err)
	}
	if n, err := s.Stat(ctx, at("c/logs"), false); err != nil || !n.IsDir() {
		t.Errorf("Stat of a prefix = %v, %v", n, err)
	}
	if _, err := s.Stat(ctx, at("c/nope"), false); !store.IsNotExist(err) {
		t.Errorf("Stat of nothing = %v", err)
	}
	top, err := s.ReadDir(ctx, at("c"))
	if err != nil || len(top) != 2 {
		t.Errorf("ReadDir = %v, %v", names(top), err)
	}
	var walked []string
	err = s.WalkAll(ctx, at("c"),
		func(_ *uri.URL, e error) error { return e },
		func(n *store.Node) error { walked = append(walked, n.URL.Key); return nil })
	if err != nil || len(walked) != 3 {
		t.Errorf("WalkAll = %v, %v; want the two blobs and the directory between them", walked, err)
	}
	if err := s.MkdirMarker(ctx, at("c/empty")); err != nil {
		t.Errorf("MkdirMarker: %v", err)
	}
	if err := s.Remove(ctx, at("c/logs/app.log")); err != nil {
		t.Errorf("Remove: %v", err)
	}

	if got := sas.refused(); len(got) != 0 {
		t.Errorf("requests went outside the container: %v", got)
	}
}
