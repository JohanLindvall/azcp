package azure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

type stubCredential struct{}

func (stubCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "stub", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func rejected(status int, code string) error {
	return &azcore.ResponseError{
		StatusCode:  status,
		ErrorCode:   code,
		RawResponse: &http.Response{StatusCode: status, Header: http.Header{}},
	}
}

// newTestStore builds a store whose sign-in is stubbed and whose saved-record
// lookup cannot find anything, so the test never touches the real credential
// store or the user's configuration.
func newTestStore(t *testing.T, signIn func()) *Store {
	t.Helper()
	creds := &Credentials{
		Mode:        AuthAuto,
		Interactive: true,
		TenantID:    "azcp-test-" + t.Name(),
		Log:         slog.New(slog.DiscardHandler),
		signInFn: func(context.Context, AuthMode) (azcore.TokenCredential, string, error) {
			signIn()
			return stubCredential{}, "stub", nil
		},
	}
	return &Store{
		cfg:     Config{Auth: AuthAuto, Interactive: true},
		log:     slog.New(slog.DiscardHandler),
		creds:   creds,
		clients: map[string]*azblob.Client{},
	}
}

// A run must sign in at most once however many transfers are rejected at the
// same moment. Anything else means a browser window per file.
func TestSignsInOnceUnderConcurrentRejections(t *testing.T) {
	var signIns, attempts atomic.Int32
	s := newTestStore(t, func() { signIns.Add(1) })

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.withSignIn(context.Background(), func() error {
				attempts.Add(1)
				return rejected(http.StatusUnauthorized, "InvalidAuthenticationInfo")
			})
		}()
	}
	wg.Wait()

	if got := signIns.Load(); got != 1 {
		t.Errorf("interactive sign-ins = %d, want exactly 1", got)
	}
	// Every caller runs its operation once, and once more only if that attempt
	// predated the sign-in: one that already ran with the new credential has
	// nothing to gain by repeating itself.
	if got := attempts.Load(); got < 21 || got > 40 {
		t.Errorf("operation attempts = %d, want between 21 and 40", got)
	}
}

// A later rejection must not start a second sign-in either.
func TestSignsInOnceAcrossSequentialRejections(t *testing.T) {
	var signIns atomic.Int32
	s := newTestStore(t, func() { signIns.Add(1) })
	for range 5 {
		_ = s.withSignIn(context.Background(), func() error {
			return rejected(http.StatusUnauthorized, "InvalidAuthenticationInfo")
		})
	}
	if got := signIns.Load(); got != 1 {
		t.Errorf("interactive sign-ins = %d, want exactly 1", got)
	}
	if got := s.creds.Prompts(); got != 1 {
		t.Errorf("Prompts() = %d, want 1", got)
	}
}

// A successful operation must never prompt.
func TestSuccessNeverSignsIn(t *testing.T) {
	var signIns atomic.Int32
	s := newTestStore(t, func() { signIns.Add(1) })
	if err := s.withSignIn(context.Background(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := signIns.Load(); got != 0 {
		t.Errorf("interactive sign-ins = %d, want 0", got)
	}
}

func TestShouldSignIn(t *testing.T) {
	s := newTestStore(t, func() {})

	// 401 always warrants a sign-in.
	if !s.shouldSignIn(rejected(http.StatusUnauthorized, "InvalidAuthenticationInfo")) {
		t.Error("401 should prompt a sign-in")
	}
	// 403 with an identity in hand is a missing role; signing in again as the
	// same person changes nothing.
	s.creds.mu.Lock()
	s.creds.resolved, s.creds.cred = true, stubCredential{}
	s.creds.mu.Unlock()
	if s.shouldSignIn(rejected(http.StatusForbidden, "AuthorizationPermissionMismatch")) {
		t.Error("403 with an identity should not prompt")
	}
	// With no identity at all, 403 means credentials were needed after all.
	s.creds.mu.Lock()
	s.creds.cred = nil
	s.creds.mu.Unlock()
	if !s.shouldSignIn(rejected(http.StatusForbidden, "AuthorizationFailure")) {
		t.Error("403 while anonymous should prompt")
	}
	// Nothing else does.
	for _, err := range []error{
		nil,
		rejected(http.StatusNotFound, "BlobNotFound"),
		rejected(http.StatusServiceUnavailable, "ServerBusy"),
	} {
		if s.shouldSignIn(err) {
			t.Errorf("shouldSignIn(%v) = true, want false", err)
		}
	}
	// Never when nobody is there, whatever the failure.
	s.cfg.Interactive = false
	if s.shouldSignIn(rejected(http.StatusUnauthorized, "")) {
		t.Error("should not prompt with no terminal attached")
	}
}

func TestExplainAuth(t *testing.T) {
	s := newTestStore(t, func() {})

	// Discovery never ran: a SAS or account key was supplied and refused.
	got := s.explainAuth(rejected(http.StatusForbidden, "AuthenticationFailed")).Error()
	if !contains(got, "credentials given for it") {
		t.Errorf("static-credential message = %q", got)
	}

	s.creds.mu.Lock()
	s.creds.resolved = true // anonymous: resolved, but no credential
	s.creds.mu.Unlock()
	got = s.explainAuth(rejected(http.StatusUnauthorized, "NoAuthenticationInformation")).Error()
	if !contains(got, "not signed in") {
		t.Errorf("anonymous message = %q", got)
	}

	s.creds.mu.Lock()
	s.creds.cred = stubCredential{}
	s.creds.mu.Unlock()
	got = s.explainAuth(rejected(http.StatusForbidden, "AuthorizationPermissionMismatch")).Error()
	if !contains(got, "not allowed") {
		t.Errorf("permission message = %q", got)
	}
	got = s.explainAuth(rejected(http.StatusUnauthorized, "InvalidAuthenticationInfo")).Error()
	if !contains(got, "different tenant") {
		t.Errorf("tenant message = %q", got)
	}

	// Anything that is not an authentication problem passes through unchanged.
	orig := rejected(http.StatusNotFound, "BlobNotFound")
	if s.explainAuth(orig) != orig {
		t.Error("a non-auth error was rewritten")
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}

// isolateConfigDir points os.UserConfigDir at a temporary directory, so a test
// can never read or overwrite a real saved sign-in. Which variable does that
// differs per platform.
func isolateConfigDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("AppData", dir)
	case "darwin":
		t.Setenv("HOME", dir)
	default:
		t.Setenv("XDG_CONFIG_HOME", dir)
	}
	got, err := os.UserConfigDir()
	if err != nil || !strings.HasPrefix(got, dir) {
		t.Skipf("cannot redirect the config directory on this platform (got %q)", got)
	}
}

// The record is what lets a later run skip the browser, so it has to survive a
// round trip through the file it is kept in.
func TestAuthenticationRecordRoundTrip(t *testing.T) {
	isolateConfigDir(t)
	c := &Credentials{Log: slog.New(slog.DiscardHandler)}

	if _, ok := c.loadRecord(); ok {
		t.Fatal("found a record before one was written")
	}
	rec := azidentity.AuthenticationRecord{
		Authority:     "login.microsoftonline.com",
		ClientID:      "04b07795-8ddb-461a-bbee-02f9e1bf7b46",
		HomeAccountID: "abc.def",
		TenantID:      "tenant-1",
		Username:      "someone@example.com",
		Version:       "1.0",
	}
	c.saveRecord(rec)

	got, ok := c.loadRecord()
	if !ok {
		t.Fatal("the record did not come back")
	}
	if got != rec {
		t.Errorf("record = %+v, want %+v", got, rec)
	}

	// It is not secret, but it names an account, so it is not world-readable.
	path, err := c.recordPath()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no POSIX mode to check.
	if perm := fi.Mode().Perm(); runtime.GOOS != "windows" && perm != 0o600 {
		t.Errorf("record mode = %v, want 0600", perm)
	}

	// A run pinned to a tenant keeps its own record, since the account differs.
	pinned := &Credentials{Log: slog.New(slog.DiscardHandler), TenantID: "other-tenant"}
	if _, ok := pinned.loadRecord(); ok {
		t.Error("a tenant-pinned run reused another tenant's record")
	}
	p2, _ := pinned.recordPath()
	if p2 == path {
		t.Error("tenant-pinned runs share a record path")
	}
}

// A tenant id reaches a file name, so it must not be able to escape the
// directory it names a file in.
func TestRecordPathIgnoresHostileTenant(t *testing.T) {
	isolateConfigDir(t)
	c := &Credentials{Log: slog.New(slog.DiscardHandler), TenantID: "../../etc/passwd"}
	path, err := c.recordPath()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(path, "..") || filepath.Base(filepath.Dir(path)) != "azcp" {
		t.Errorf("recordPath escaped its directory: %q", path)
	}
}

// resume must never start a sign-in, whatever it finds.
func TestResumeCannotPrompt(t *testing.T) {
	isolateConfigDir(t)
	c := &Credentials{Log: slog.New(slog.DiscardHandler), TenantID: "azcp-test-resume"}
	if _, ok := c.resume(context.Background()); ok {
		t.Error("resume succeeded with nothing saved")
	}
	if got := c.Prompts(); got != 0 {
		t.Errorf("resume started %d sign-ins, want 0", got)
	}
}

// challenged builds the rejection a storage account sends when it will not
// accept tokens from the tenant the caller authenticated to. The tenant it
// does accept is in the header, which is the whole point of it.
func challenged(status int, code, tenant string) error {
	err := rejected(status, code).(*azcore.ResponseError)
	err.RawResponse.Header.Set("WWW-Authenticate",
		"Bearer authorization_uri=https://login.microsoftonline.com/"+tenant+
			"/oauth2/authorize resource_id=https://storage.azure.com")
	return err
}

// tenantRecorder remembers which tenant a token was asked for.
type tenantRecorder struct {
	mu   sync.Mutex
	last string
}

func (r *tenantRecorder) GetToken(_ context.Context, o policy.TokenRequestOptions) (azcore.AccessToken, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = o.TenantID
	return azcore.AccessToken{Token: "stub", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func (r *tenantRecorder) tenant() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

func TestChallengeTenant(t *testing.T) {
	const tenant = "79cb5f1f-2be7-43cf-833f-431d16b23ef8"
	for _, tc := range []struct {
		name, header, want string
	}{
		{"as sent", "Bearer authorization_uri=https://login.microsoftonline.com/" + tenant +
			"/oauth2/authorize resource_id=https://storage.azure.com", tenant},
		{"quoted", `Bearer authorization_uri="https://login.microsoftonline.com/` + tenant +
			`/oauth2/authorize", resource_id="https://storage.azure.com"`, tenant},
		{"sovereign cloud", "Bearer authorization_uri=https://login.microsoftonline.us/" +
			tenant + "/oauth2/authorize resource_id=https://storage.azure.com", tenant},
		// These name no particular directory, so there is nothing to follow.
		{"common", "Bearer authorization_uri=https://login.microsoftonline.com/common/oauth2/authorize", ""},
		{"organizations", "Bearer authorization_uri=https://login.microsoftonline.com/organizations/oauth2/authorize", ""},
		{"no tenant", "Bearer authorization_uri=https://login.microsoftonline.com/", ""},
		{"no header", "", ""},
		{"nothing to parse", "Bearer realm=\"\"", ""},
		// A tenant id reaches a token request and a file name.
		{"hostile", "Bearer authorization_uri=https://login.microsoftonline.com/..%2f..%2fetc/oauth2/authorize", ""},
		{"relative", "Bearer authorization_uri=https://login.microsoftonline.com/../oauth2/authorize", ""},
		{"not a tenant", "Bearer authorization_uri=https://login.microsoftonline.com/who%20knows/oauth2/authorize", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := rejected(http.StatusUnauthorized, "InvalidAuthenticationInfo").(*azcore.ResponseError)
			if tc.header != "" {
				err.RawResponse.Header.Set("WWW-Authenticate", tc.header)
			}
			if got := challengeTenant(err); got != tc.want {
				t.Errorf("challengeTenant() = %q, want %q", got, tc.want)
			}
		})
	}
	// Anything that is not a rejection by the service names nothing.
	if got := challengeTenant(errors.New("connection refused")); got != "" {
		t.Errorf("challengeTenant(non-response error) = %q", got)
	}
}

// The account itself knows which directory it trusts, and says so. Following
// that costs one retry and no interaction at all, which is the difference
// between a copy that works and a browser window.
func TestFollowsTheTenantTheAccountNames(t *testing.T) {
	const tenant = "79cb5f1f-2be7-43cf-833f-431d16b23ef8"
	var signIns atomic.Int32
	s := newTestStore(t, func() { signIns.Add(1) })
	rec := &tenantRecorder{}
	s.creds.mu.Lock()
	s.creds.resolved, s.creds.cred = true, rec
	s.creds.mu.Unlock()

	var attempts int
	err := s.withSignIn(context.Background(), func() error {
		attempts++
		if attempts == 1 {
			return challenged(http.StatusUnauthorized, "InvalidAuthenticationInfo", tenant)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("the operation failed after the tenant was named: %v", err)
	}
	if attempts != 2 {
		t.Errorf("operation attempts = %d, want 2", attempts)
	}
	if got := signIns.Load(); got != 0 {
		t.Errorf("interactive sign-ins = %d, want 0: the account said which tenant "+
			"it trusts, which needs nobody's attention", got)
	}
	if got := s.knownTenant(); got != tenant {
		t.Errorf("knownTenant() = %q, want %q", got, tenant)
	}

	// The identity already found is now asked for tokens in that tenant.
	cred, _, cerr := s.creds.Resolve(context.Background())
	if cerr != nil {
		t.Fatal(cerr)
	}
	if _, terr := cred.GetToken(context.Background(), policy.TokenRequestOptions{}); terr != nil {
		t.Fatal(terr)
	}
	if got := rec.tenant(); got != tenant {
		t.Errorf("token requested for tenant %q, want %q", got, tenant)
	}
	// A tenant the caller asks for explicitly still wins over it.
	if _, terr := cred.GetToken(context.Background(),
		policy.TokenRequestOptions{TenantID: "other"}); terr != nil {
		t.Fatal(terr)
	}
	if got := rec.tenant(); got != "other" {
		t.Errorf("token requested for tenant %q, want %q", got, "other")
	}
}

// --tenant is an instruction, not a suggestion: a service that names another
// one does not get to overrule it.
func TestPinnedTenantIsNotOverruled(t *testing.T) {
	var signIns atomic.Int32
	s := newTestStore(t, func() { signIns.Add(1) })
	s.cfg.TenantID = "11111111-1111-1111-1111-111111111111"

	_ = s.withSignIn(context.Background(), func() error {
		return challenged(http.StatusUnauthorized, "InvalidAuthenticationInfo",
			"79cb5f1f-2be7-43cf-833f-431d16b23ef8")
	})
	if got := s.knownTenant(); got != "" {
		t.Errorf("followed the service to tenant %q despite --tenant", got)
	}
	if got := signIns.Load(); got != 1 {
		t.Errorf("interactive sign-ins = %d, want 1", got)
	}
}

// Resuming a sign-in saved by an earlier run asks nothing of anybody, so being
// refused with one in hand must still leave the run able to ask. Spending the
// prompt on it is how "signing in…" comes to be followed straight away by
// another rejection, with no browser in between.
func TestSavedSignInDoesNotSpendThePrompt(t *testing.T) {
	var signIns, resumes atomic.Int32
	s := newTestStore(t, func() { signIns.Add(1) })
	s.creds.resumeFn = func(context.Context) (azcore.TokenCredential, bool) {
		resumes.Add(1)
		return stubCredential{}, true
	}

	var attempts int
	err := s.withSignIn(context.Background(), func() error {
		attempts++
		return rejected(http.StatusUnauthorized, "InvalidAuthenticationInfo")
	})
	if err == nil {
		t.Fatal("a rejection that never let up was reported as success")
	}
	if got := resumes.Load(); got != 1 {
		t.Errorf("saved sign-ins tried = %d, want 1", got)
	}
	if got := signIns.Load(); got != 1 {
		t.Errorf("interactive sign-ins = %d, want 1", got)
	}
	if got := s.creds.Prompts(); got != 1 {
		t.Errorf("Prompts() = %d, want 1", got)
	}
	// The operation, the retry with the saved sign-in, and the retry after the
	// interactive one.
	if attempts != 3 {
		t.Errorf("operation attempts = %d, want 3", attempts)
	}
}

// Naming the tenant is the difference between a message the user can act on
// and one that tells them to guess an id.
func TestExplainAuthNamesTheTenant(t *testing.T) {
	const tenant = "79cb5f1f-2be7-43cf-833f-431d16b23ef8"
	s := newTestStore(t, func() {})
	s.creds.mu.Lock()
	s.creds.resolved, s.creds.cred = true, stubCredential{}
	s.creds.mu.Unlock()
	s.tenant.known = tenant

	got := s.explainAuth(rejected(http.StatusUnauthorized, "InvalidAuthenticationInfo")).Error()
	if !contains(got, tenant) {
		t.Errorf("message = %q, want the tenant named", got)
	}

	// A credential that cannot produce a token for that tenant at all is the
	// same problem wearing a different error — and one the identity chain
	// reports with a type this package cannot name, so it is recognised by the
	// refusal recorded when the token was asked for rather than by its type.
	refusal := errors.New("AADSTS500213: the resource tenant's cross-tenant access policy")
	s.noteTenantRefusal(refusal)
	failed := fmt.Errorf("list containers on acct: %w", refusal)
	if !s.shouldSignIn(failed) {
		t.Error("a token that cannot be had for the named tenant should prompt")
	}
	if got := s.explainAuth(failed).Error(); !contains(got, tenant) {
		t.Errorf("message = %q, want the tenant named", got)
	}
	// An unrelated failure is not that, whatever else has gone wrong.
	other := errors.New("no space left on device")
	if s.shouldSignIn(other) {
		t.Error("an unrelated failure should not prompt")
	}
	if s.explainAuth(other) != other {
		t.Error("an unrelated failure was rewritten")
	}
}
