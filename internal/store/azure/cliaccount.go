package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// The Azure CLI can be signed in as several people at once. One account per
// tenant is the usual shape of it: a work account in its own directory, and
// another in a customer's or a second employer's. Asked for a token, the CLI
// answers as one of them, and `--tenant` does not choose which: it asks the
// CLI's *current* account for a token in that tenant, which a tenant the
// account is not a member of refuses (AADSTS50020), however many accounts the
// CLI holds that are members there. `--subscription` does choose. The CLI
// answers as the account the subscription belongs to, in the subscription's
// own tenant.
//
// So when a storage account names a tenant that the identity in hand cannot
// get a token for, the CLI's other accounts in that tenant are asked next,
// each by one of its subscriptions, before anybody is asked to sign in. They
// come from the CLI's profile, azureProfile.json, which lists every
// subscription the CLI can see, with the tenant it is in and the account it
// was seen as. Reading that file costs no process, where `az account list`
// would cost one to say the same thing.

// cliSubscriptionsIn returns one subscription for each account the Azure CLI's
// profile holds in tenant, other than the CLI's current account, which is the
// one that a request for the tenant already asked as. It returns nothing when
// there is no profile, when the profile cannot be read, or when it holds no
// other account there.
func cliSubscriptionsIn(tenant string) []string {
	path, ok := cliProfilePath()
	if !ok || tenant == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var profile struct {
		Subscriptions []struct {
			ID        string `json:"id"`
			TenantID  string `json:"tenantId"`
			IsDefault bool   `json:"isDefault"`
			User      struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"user"`
		} `json:"subscriptions"`
	}
	// The CLI writes its profile with a byte-order mark, which encoding/json
	// refuses to read past.
	if json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &profile) != nil {
		return nil
	}
	account := func(kind, name string) string { return strings.ToLower(kind + "/" + name) }
	seen := map[string]bool{}
	for _, s := range profile.Subscriptions {
		if s.IsDefault {
			seen[account(s.User.Type, s.User.Name)] = true
		}
	}
	var subscriptions []string
	for _, s := range profile.Subscriptions {
		who := account(s.User.Type, s.User.Name)
		if s.ID == "" || !strings.EqualFold(s.TenantID, tenant) || seen[who] {
			continue
		}
		seen[who] = true
		subscriptions = append(subscriptions, s.ID)
	}
	return subscriptions
}

// cliProfilePath is where the Azure CLI keeps its profile: in $AZURE_CONFIG_DIR
// when that is set, as the CLI itself honours it, and in .azure in the home
// directory otherwise.
func cliProfilePath() (string, bool) {
	dir := os.Getenv("AZURE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		dir = filepath.Join(home, ".azure")
	}
	return filepath.Join(dir, "azureProfile.json"), true
}

// cliAccounts asks the Azure CLI for a token as each of its accounts in a
// tenant in turn, naming each by one of its subscriptions, and from then on
// asks the one that answered first.
type cliAccounts struct {
	subscriptions []string
	newCred       func(subscription string) (azcore.TokenCredential, error)
	log           *slog.Logger

	// mu is held across the requests, which are one process each: concurrent
	// transfers that all need a token wait for one answer rather than each
	// starting the CLI for every account.
	mu       sync.Mutex
	answered azcore.TokenCredential
}

// hasAnswered reports whether one of the accounts has produced a token.
func (a *cliAccounts) hasAnswered() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.answered != nil
}

func (a *cliAccounts) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	// The subscription names the tenant as well as the account, and the CLI
	// refuses to be told both ("Please specify only one of subscription and
	// tenant, not both"), so the tenant is left for the subscription to say.
	opts.TenantID = ""
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.answered != nil {
		if tk, err := a.answered.GetToken(ctx, opts); err == nil {
			return tk, nil
		}
		a.answered = nil
	}
	var errs []error
	for _, subscription := range a.subscriptions {
		cred, err := a.newCred(subscription)
		if err == nil {
			var tk azcore.AccessToken
			if tk, err = cred.GetToken(ctx, opts); err == nil {
				a.answered = cred
				a.log.Info("using the Azure CLI's account in the tenant the storage account named",
					"subscription", subscription)
				return tk, nil
			}
		}
		errs = append(errs, err)
	}
	return azcore.AccessToken{}, errors.Join(errs...)
}

// cliAccountsIn returns the Azure CLI's other accounts in tenant, to be asked
// when the identity in hand cannot get a token there. It returns nil when the
// CLI holds none, and when the user asked for a particular way of signing in,
// because that asks to sign in as somebody rather than to be found.
func (c *Credentials) cliAccountsIn(tenant string) *cliAccounts {
	switch c.Mode {
	case AuthDevice, AuthBrowser, AuthAnonymous:
		return nil
	}
	subscriptions := cliSubscriptionsIn(tenant)
	if len(subscriptions) == 0 {
		return nil
	}
	newCred := c.cliFn
	if newCred == nil {
		newCred = func(subscription string) (azcore.TokenCredential, error) {
			return azidentity.NewAzureCLICredential(
				&azidentity.AzureCLICredentialOptions{Subscription: subscription})
		}
	}
	return &cliAccounts{subscriptions: subscriptions, newCred: newCred, log: c.logger()}
}
