package azure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// storageScope is the OAuth scope for the Blob service.
const storageScope = "https://storage.azure.com/.default"

// AuthMode selects how credentials are found. The default, AuthAuto, tries
// everything in turn so that a correctly configured environment needs no flags
// at all.
type AuthMode string

const (
	// AuthAuto walks the whole chain: SAS in the URL, connection string,
	// account key, the ambient Azure identity, an interactive device-code
	// sign-in, and finally anonymous access.
	AuthAuto AuthMode = "auto"
	// AuthIdentity restricts the chain to DefaultAzureCredential.
	AuthIdentity AuthMode = "identity"
	// AuthDevice forces an interactive device-code sign-in.
	AuthDevice AuthMode = "device"
	// AuthAnonymous makes unauthenticated requests, for public containers.
	AuthAnonymous AuthMode = "anonymous"
)

// ParseAuthMode validates a --auth value.
func ParseAuthMode(s string) (AuthMode, error) {
	switch AuthMode(strings.ToLower(strings.TrimSpace(s))) {
	case "", AuthAuto:
		return AuthAuto, nil
	case AuthIdentity:
		return AuthIdentity, nil
	case AuthDevice:
		return AuthDevice, nil
	case AuthAnonymous:
		return AuthAnonymous, nil
	}
	return "", fmt.Errorf("unknown auth mode %q (want auto, identity, device or anonymous)", s)
}

// Credentials discovers and caches the credential for the process. Discovery is
// deliberately silent when it succeeds: the point of transparent login is that
// a user with `az login` already done, a managed identity, or the standard
// AZURE_* environment variables never has to think about it.
type Credentials struct {
	Mode        AuthMode
	Log         *slog.Logger
	Interactive bool // a terminal is attached, so a device-code prompt can work
	TenantID    string

	once sync.Once
	cred azcore.TokenCredential
	err  error
	kind string
}

// Token returns a bearer token for the Blob service, used to authorise
// server-side copies where the service itself fetches the source.
func (c *Credentials) Token(ctx context.Context) (string, error) {
	cred, _, err := c.Resolve(ctx)
	if err != nil {
		return "", err
	}
	if cred == nil {
		return "", errors.New("no token credential available")
	}
	tk, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{storageScope}})
	if err != nil {
		return "", err
	}
	return tk.Token, nil
}

// Resolve returns the token credential to use, along with a short description
// of where it came from. A nil credential with a nil error means anonymous
// access: no identity was found, and the caller should proceed unauthenticated.
func (c *Credentials) Resolve(ctx context.Context) (azcore.TokenCredential, string, error) {
	c.once.Do(func() { c.cred, c.kind, c.err = c.resolve(ctx) })
	return c.cred, c.kind, c.err
}

func (c *Credentials) resolve(ctx context.Context) (azcore.TokenCredential, string, error) {
	log := c.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	switch c.Mode {
	case AuthAnonymous:
		log.Debug("using anonymous access", "reason", "requested with --auth=anonymous")
		return nil, "anonymous", nil
	case AuthDevice:
		cred, err := c.deviceCode()
		if err != nil {
			return nil, "", err
		}
		if err := probe(ctx, cred); err != nil {
			return nil, "", fmt.Errorf("device-code sign-in failed: %w", err)
		}
		return cred, "device code", nil
	}

	// The ambient identity: environment variables, workload identity, managed
	// identity, the Azure CLI, and the Azure Developer CLI, in that order.
	def, err := azidentity.NewDefaultAzureCredential(&azidentity.DefaultAzureCredentialOptions{
		TenantID: c.TenantID,
	})
	if err == nil {
		if perr := probe(ctx, def); perr == nil {
			log.Debug("authenticated with the ambient Azure identity")
			return def, "default azure credential", nil
		} else {
			log.Debug("ambient Azure identity unavailable", "error", perr)
			err = perr
		}
	}

	if c.Mode == AuthIdentity {
		return nil, "", fmt.Errorf("no Azure identity available: %w\n"+
			"hint: run `az login`, set AZURE_CLIENT_ID/AZURE_TENANT_ID/AZURE_CLIENT_SECRET, "+
			"or pass a SAS token in the URL", err)
	}

	// Nothing ambient. If someone is watching, offer to sign in; the token is
	// cached in memory for the life of the process.
	if c.Interactive {
		log.Info("no ambient Azure credential found, starting interactive sign-in")
		if cred, derr := c.deviceCode(); derr == nil {
			if perr := probe(ctx, cred); perr == nil {
				return cred, "device code", nil
			} else {
				log.Warn("interactive sign-in failed, continuing anonymously", "error", perr)
			}
		} else {
			log.Warn("could not start interactive sign-in", "error", derr)
		}
	}

	log.Warn("no Azure credential found; continuing anonymously, "+
		"which only works for containers that allow public read access",
		"hint", "run `az login`, or pass a SAS token in the URL")
	return nil, "anonymous", nil
}

func (c *Credentials) deviceCode() (azcore.TokenCredential, error) {
	return azidentity.NewDeviceCodeCredential(&azidentity.DeviceCodeCredentialOptions{
		TenantID: c.TenantID,
		UserPrompt: func(_ context.Context, m azidentity.DeviceCodeMessage) error {
			// Written straight to stderr: this is a prompt, not a log record,
			// and it must be visible whatever the log level.
			fmt.Fprintf(os.Stderr, "\n%s\n\n", m.Message)
			return nil
		},
	})
}

// probe forces a token request so that an unusable credential is reported now,
// with a clear message, instead of surfacing as a puzzling 401 mid-transfer.
func probe(ctx context.Context, cred azcore.TokenCredential) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	_, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{storageScope}})
	return err
}

// staticCredentials describes credential material found outside the identity
// chain: a SAS token or a shared key. These bypass Credentials entirely.
type staticCredentials struct {
	sas              string
	accountKey       string
	connectionString string
}

// lookupStatic collects credential material from the environment for a given
// account. Values on the URL itself take precedence and are handled by the
// caller.
func lookupStatic(account string) staticCredentials {
	var s staticCredentials
	s.connectionString = os.Getenv("AZURE_STORAGE_CONNECTION_STRING")
	s.sas = strings.TrimPrefix(firstEnv("AZURE_STORAGE_SAS_TOKEN", "AZURE_STORAGE_SAS"), "?")
	// A key only applies to the account it was issued for. When
	// AZURE_STORAGE_ACCOUNT names a different account, ignore the key rather
	// than sending a signature that cannot verify.
	named := os.Getenv("AZURE_STORAGE_ACCOUNT")
	if named == "" || named == account {
		s.accountKey = firstEnv("AZURE_STORAGE_KEY", "AZURE_STORAGE_ACCOUNT_KEY")
	}
	return s
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}
