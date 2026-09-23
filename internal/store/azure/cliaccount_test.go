package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

const (
	homeTenant  = "11111111-1111-1111-1111-111111111111"
	otherTenant = "44444444-4444-4444-4444-444444444444"
)

// cliProfile writes an Azure CLI profile into a fresh AZURE_CONFIG_DIR the way
// the CLI writes one, byte-order mark included.
func cliProfile(t *testing.T, subscriptions ...map[string]any) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AZURE_CONFIG_DIR", dir)
	b, err := json.Marshal(map[string]any{"installationId": "test", "subscriptions": subscriptions})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "azureProfile.json"), append([]byte("\xef\xbb\xbf"), b...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func subscription(id, tenant, user string, current bool) map[string]any {
	return map[string]any{"id": id, "tenantId": tenant, "isDefault": current, "state": "Enabled",
		"user": map[string]any{"name": user, "type": "user"}}
}

// One subscription names each account, and the CLI's current account is left
// out: a request for the tenant has already asked as that one.
func TestCLISubscriptionsIn(t *testing.T) {
	cliProfile(t,
		subscription("home", homeTenant, "me@home.example", true),
		subscription("home-as-guest", otherTenant, "Me@Home.example", false),
		subscription("other-1", otherTenant, "me@other.example", false),
		subscription("other-2", otherTenant, "me@other.example", false),
		subscription("ops", strings.ToUpper(otherTenant), "ops@other.example", false),
	)
	if got, want := cliSubscriptionsIn(otherTenant), []string{"other-1", "ops"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cliSubscriptionsIn(other) = %q, want %q", got, want)
	}
	if got := cliSubscriptionsIn(homeTenant); got != nil {
		t.Errorf("cliSubscriptionsIn(home) = %q, want nothing: the current account is the only one there", got)
	}
	if got := cliSubscriptionsIn("22222222-2222-2222-2222-222222222222"); got != nil {
		t.Errorf("cliSubscriptionsIn(unknown) = %q, want nothing", got)
	}
}

// No profile, or one that cannot be read, is no account rather than a failure.
func TestCLISubscriptionsInWithoutAUsableProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AZURE_CONFIG_DIR", dir)
	if got := cliSubscriptionsIn(otherTenant); got != nil {
		t.Errorf("with no profile: %q, want nothing", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "azureProfile.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := cliSubscriptionsIn(otherTenant); got != nil {
		t.Errorf("with a profile that is not JSON: %q, want nothing", got)
	}
}

// memberOnlyAt is an identity that is a member of one tenant and of no other:
// an `az login` account asked for a token somewhere else, which the CLI refuses
// with AADSTS50020.
type memberOnlyAt struct {
	tenant string
	asked  atomic.Int32
}

func (m *memberOnlyAt) GetToken(_ context.Context, o policy.TokenRequestOptions) (azcore.AccessToken, error) {
	m.asked.Add(1)
	if o.TenantID != "" && o.TenantID != m.tenant {
		return azcore.AccessToken{}, errors.New("AADSTS50020: user account does not exist in tenant")
	}
	return azcore.AccessToken{Token: "home", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// fakeCLI stands in for the Azure CLI answering as the account a subscription
// belongs to, and records how it was asked.
type fakeCLI struct {
	mu      sync.Mutex
	asked   []string // the subscription each request named
	tenants []string // the tenant each request named
	refuse  bool
}

func (f *fakeCLI) newCred(subscription string) (azcore.TokenCredential, error) {
	return cliAs{f, subscription}, nil
}

func (f *fakeCLI) requests() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...), append([]string(nil), f.tenants...)
}

type cliAs struct {
	f            *fakeCLI
	subscription string
}

func (c cliAs) GetToken(_ context.Context, o policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.asked = append(c.f.asked, c.subscription)
	c.f.tenants = append(c.f.tenants, o.TenantID)
	if c.f.refuse {
		return azcore.AccessToken{}, errors.New("AADSTS700082: the refresh token has expired")
	}
	return azcore.AccessToken{Token: "cli:" + c.subscription, ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// An `az login` that holds an account in the tenant the storage account named
// is all that is needed: that account is asked, by one of its subscriptions,
// and nobody is asked to sign in.
func TestTheCLIsAccountInTheNamedTenantAnswersForIt(t *testing.T) {
	var signIns atomic.Int32
	s := newTestStore(t, func() { signIns.Add(1) })
	cliProfile(t,
		subscription("home", homeTenant, "me@home.example", true),
		subscription("other", otherTenant, "me@other.example", false))
	cli := &fakeCLI{}
	s.creds.cliFn = cli.newCred
	current := &memberOnlyAt{tenant: homeTenant}
	s.creds.mu.Lock()
	s.creds.resolved, s.creds.cred = true, current
	s.creds.mu.Unlock()

	// The service names the tenant; the retried operation asks for a token in
	// it, as the storage pipeline does.
	var token string
	var attempts int
	err := s.withSignIn(context.Background(), func() error {
		attempts++
		if attempts == 1 {
			return challenged(http.StatusUnauthorized, "InvalidAuthenticationInfo", otherTenant)
		}
		cred, _, err := s.creds.Resolve(context.Background())
		if err != nil {
			return err
		}
		tk, err := cred.GetToken(context.Background(), tokenRequest())
		if err != nil {
			return fmt.Errorf("list containers: %w", err)
		}
		token = tk.Token
		return nil
	})
	if err != nil {
		t.Fatalf("the operation failed although the CLI holds an account in the tenant: %v", err)
	}
	if token != "cli:other" {
		t.Errorf("token = %q, want the one the CLI's account in the tenant was given", token)
	}
	if got := signIns.Load(); got != 0 {
		t.Errorf("interactive sign-ins = %d, want 0", got)
	}
	asked, tenants := cli.requests()
	if !reflect.DeepEqual(asked, []string{"other"}) {
		t.Errorf("the CLI was asked as %q, want the account in the tenant, by its subscription", asked)
	}
	if !reflect.DeepEqual(tenants, []string{""}) {
		t.Errorf("the CLI was asked for tenant %q, want none: the subscription names it, and the CLI "+
			"refuses to be told both", tenants)
	}
	s.tenant.mu.Lock()
	refusal := s.tenant.refusal
	s.tenant.mu.Unlock()
	if refusal != nil {
		t.Errorf("a refusal was recorded although a token was had: %v", refusal)
	}

	// Asked again, the account that answered is asked first, and the identity
	// already refused in the tenant is not asked there again.
	before := current.asked.Load()
	cred, _, _ := s.creds.Resolve(context.Background())
	if _, err := cred.GetToken(context.Background(), tokenRequest()); err != nil {
		t.Fatal(err)
	}
	if got := current.asked.Load(); got != before {
		t.Errorf("the refused identity was asked %d more time(s)", got-before)
	}
}

// When the CLI's accounts cannot answer either, the refusal is what it was, and
// the sign-in that follows it still happens.
func TestWhenTheCLIsAccountsAreRefusedTooASignInFollows(t *testing.T) {
	var signIns atomic.Int32
	s := newTestStore(t, func() { signIns.Add(1) })
	cliProfile(t,
		subscription("home", homeTenant, "me@home.example", true),
		subscription("other", otherTenant, "me@other.example", false))
	cli := &fakeCLI{refuse: true}
	s.creds.cliFn = cli.newCred
	s.creds.mu.Lock()
	s.creds.resolved, s.creds.cred = true, &memberOnlyAt{tenant: homeTenant}
	s.creds.mu.Unlock()

	var attempts int
	err := s.withSignIn(context.Background(), func() error {
		attempts++
		if attempts == 1 {
			return challenged(http.StatusUnauthorized, "InvalidAuthenticationInfo", otherTenant)
		}
		cred, _, err := s.creds.Resolve(context.Background())
		if err != nil {
			return err
		}
		if _, err := cred.GetToken(context.Background(), tokenRequest()); err != nil {
			return fmt.Errorf("list containers: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("the operation failed after the sign-in: %v", err)
	}
	if got := signIns.Load(); got != 1 {
		t.Errorf("interactive sign-ins = %d, want 1", got)
	}
	if asked, _ := cli.requests(); !reflect.DeepEqual(asked, []string{"other"}) {
		t.Errorf("the CLI was asked as %q, want the account in the tenant, once", asked)
	}
}

// Asking for a way of signing in by name asks to sign in as somebody, not to be
// found: the CLI's accounts are not offered in its place.
func TestASignInAskedForByNameIsNotAnsweredByTheCLI(t *testing.T) {
	cliProfile(t,
		subscription("home", homeTenant, "me@home.example", true),
		subscription("other", otherTenant, "me@other.example", false))
	for _, mode := range []AuthMode{AuthDevice, AuthBrowser, AuthAnonymous} {
		c := &Credentials{Mode: mode, Log: slog.New(slog.DiscardHandler)}
		if got := c.cliAccountsIn(otherTenant); got != nil {
			t.Errorf("--auth=%s: the CLI's accounts were offered", mode)
		}
	}
	for _, mode := range []AuthMode{AuthAuto, AuthIdentity} {
		c := &Credentials{Mode: mode, Log: slog.New(slog.DiscardHandler)}
		if got := c.cliAccountsIn(otherTenant); got == nil {
			t.Errorf("--auth=%s: the CLI's accounts were not offered", mode)
		}
	}
}

// The real Azure CLI credential, run against a stand-in `az`, because what
// reaches the CLI's command line is the contract all of this rests on: the CLI
// answers as the subscription's account only when it is given the subscription
// and not the tenant as well, which it refuses outright.
func TestTheCLIIsAskedBySubscriptionAndNotByTenantAsWell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in az is a shell script")
	}
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "az.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + calls + `'
case " $* " in
  *" --tenant "*" --subscription "*|*" --subscription "*" --tenant "*)
    echo "ERROR: Please specify only one of subscription and tenant, not both" >&2; exit 1 ;;
  *" --subscription "*)
    printf '{"accessToken": "from-az", "expires_on": %s}\n' "$(( $(date +%s) + 3600 ))" ;;
  *)
    echo "ERROR: AADSTS50020: user account does not exist in tenant" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "az"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cliProfile(t,
		subscription("home", homeTenant, "me@home.example", true),
		subscription("other-subscription", otherTenant, "me@other.example", false))

	c := &Credentials{Mode: AuthAuto, Log: slog.New(slog.DiscardHandler)}
	accounts := c.cliAccountsIn(otherTenant)
	if accounts == nil {
		t.Fatal("no account of the CLI's was found in the tenant")
	}
	opts := tokenRequest()
	opts.TenantID = otherTenant
	tk, err := accounts.GetToken(context.Background(), opts)
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if tk.Token != "from-az" {
		t.Errorf("token = %q, want the one az printed", tk.Token)
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(b))
	if !strings.Contains(got, "--subscription other-subscription") || strings.Contains(got, "--tenant") {
		t.Errorf("az was run as %q, want --subscription and no --tenant", got)
	}
}
