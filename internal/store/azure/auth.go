package azure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/JohanLindvall/azcp/internal/logx"
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
	// AuthDevice forces a device-code sign-in, which works anywhere the code
	// can be read and typed elsewhere.
	AuthDevice AuthMode = "device"
	// AuthBrowser forces a browser sign-in.
	AuthBrowser AuthMode = "browser"
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
	case AuthBrowser:
		return AuthBrowser, nil
	case AuthAnonymous:
		return AuthAnonymous, nil
	}
	return "", fmt.Errorf("unknown auth mode %q "+
		"(want auto, identity, browser, device or anonymous)", s)
}

// Credentials discovers and caches the credential for the process. Discovery is
// deliberately silent when it succeeds: the point of transparent login is that
// a user with `az login` already done, a managed identity, or the standard
// AZURE_* environment variables never has to think about it.
//
// Discovery alone cannot tell whether a credential will be accepted — an
// `az login` session for the wrong tenant produces a perfectly good token that
// the storage account then refuses. Escalate exists for that case, and is
// driven by the service's answer rather than by guesswork here.
type Credentials struct {
	Mode        AuthMode
	Log         *slog.Logger
	Interactive bool // a terminal is attached, so a sign-in prompt can be answered
	TenantID    string

	mu        sync.Mutex
	resolved  bool
	cred      azcore.TokenCredential
	err       error
	kind      string
	escalated bool
	// prompts counts interactive sign-ins started, which must never exceed one.
	prompts int

	cacheOnce sync.Once
	cache     azidentity.Cache
	// persistent records that the cache above survives the process, so the
	// user can be told when a sign-in will have to be repeated next time.
	persistent bool

	// signInFn stands in for the interactive flow in tests, which cannot open
	// a browser. It is nil everywhere else.
	signInFn func(context.Context, AuthMode) (azcore.TokenCredential, string, error)
}

// kindResumed marks a credential rebuilt from a previous run's sign-in, which
// must not be offered as the answer to that same credential being rejected.
const kindResumed = "saved sign-in"

func (c *Credentials) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.New(slog.DiscardHandler)
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
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.resolved {
		c.cred, c.kind, c.err = c.resolve(ctx)
		c.resolved = true
	}
	return c.cred, c.kind, c.err
}

// Consulted reports whether credential discovery ran at all. It does not when
// a SAS token or an account key was supplied, since those bypass the identity
// chain entirely — which changes what a rejection means.
func (c *Credentials) Consulted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resolved
}

// IsAnonymous reports whether the resolved credential is no credential at all.
// A rejection then means sign-in was needed, rather than that the signed-in
// identity lacks a role.
func (c *Credentials) IsAnonymous() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resolved && c.cred == nil
}

// Escalate discards whatever was discovered and signs in interactively,
// replacing the cached credential. It is called when the storage service
// rejects what discovery found — the one thing the credential chain cannot
// work out for itself.
//
// A run prompts at most once, whether the sign-in succeeds or not: a person who
// declined once should not be asked again for every remaining file.
func (c *Credentials) Escalate(ctx context.Context) (azcore.TokenCredential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.escalated {
		return c.cred, c.err
	}
	c.escalated = true

	// A previous run may have signed in as somebody the account does accept —
	// worth trying before asking, but pointless if that is already what was
	// just rejected.
	if c.kind != kindResumed {
		if cred, ok := c.resume(ctx); ok {
			c.cred, c.kind, c.err, c.resolved = cred, kindResumed, nil, true
			return cred, nil
		}
	}
	if !c.Interactive {
		return nil, errors.New("no terminal is attached, so there is nobody to sign in")
	}
	cred, kind, err := c.signIn(ctx)
	if err != nil {
		return nil, err
	}
	c.cred, c.kind, c.err, c.resolved = cred, kind, nil, true
	c.logger().Info("signed in to Azure", "method", kind)
	return cred, nil
}

// signIn runs an interactive sign-in, preferring a browser where there is a
// desktop to show one on, and falling back to a device code, which also works
// over SSH and inside a container.
func (c *Credentials) signIn(ctx context.Context) (azcore.TokenCredential, string, error) {
	if c.signInFn != nil {
		c.prompts++
		return c.signInFn(ctx, c.Mode)
	}
	return c.signInAs(ctx, AuthAuto)
}

// signInAs runs a sign-in by a particular method, or picks one when mode is
// AuthAuto.
func (c *Credentials) signInAs(ctx context.Context, mode AuthMode) (azcore.TokenCredential, string, error) {
	// Opening the cache here rather than at the first use tells us, before
	// anyone is troubled, whether this sign-in will have to be repeated.
	c.tokenCache()
	if !c.persistent {
		c.logger().Warn("this system has no secure store for tokens, " +
			"so this sign-in will have to be repeated next time")
	}

	if mode == AuthBrowser || (mode != AuthDevice && browserAvailable()) {
		cred, err := azidentity.NewInteractiveBrowserCredential(
			&azidentity.InteractiveBrowserCredentialOptions{
				TenantID: c.TenantID,
				Cache:    c.tokenCache(),
			})
		if err == nil {
			// A browser opening by itself is alarming without a word of
			// explanation, so this is announced rather than logged.
			logx.Errf("azcp: opening a browser to sign in to Azure…\n")
			if rec, aerr := c.authenticate(ctx, cred); aerr == nil {
				c.saveRecord(rec)
				return cred, "browser", nil
			} else if mode == AuthBrowser {
				return nil, "", aerr
			} else {
				c.logger().Warn("browser sign-in did not complete, asking for a device code instead",
					"error", aerr)
			}
		} else if mode == AuthBrowser {
			return nil, "", err
		} else {
			c.logger().Debug("browser sign-in unavailable", "error", err)
		}
	}
	cred, err := c.deviceCode()
	if err != nil {
		return nil, "", err
	}
	rec, aerr := c.authenticate(ctx, cred)
	if aerr != nil {
		return nil, "", aerr
	}
	c.saveRecord(rec)
	return cred, "device code", nil
}

// browserAvailable reports whether a browser sign-in has any chance of being
// seen. On a Unix desktop the display-server variables are the reliable
// signal; over SSH or in a container they are absent and only a device code
// can work.
func browserAvailable() bool {
	if v := os.Getenv("AZCP_NO_BROWSER"); v != "" && v != "0" {
		return false
	}
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
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
	case AuthDevice, AuthBrowser:
		c.escalated = true // already as interactive as it gets
		cred, kind, err := c.signInAs(ctx, c.Mode)
		if err != nil {
			return nil, "", fmt.Errorf("%s sign-in failed: %w", c.Mode, err)
		}
		return cred, kind, nil
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

	// A sign-in from an earlier run, if the tokens are still good.
	if cred, ok := c.resume(ctx); ok {
		return cred, kindResumed, nil
	}

	// Nothing ambient and nothing saved. If someone is watching, offer to sign
	// in now rather than letting the first request fail.
	if c.Interactive {
		log.Info("no ambient Azure credential found, signing in")
		c.escalated = true
		if cred, kind, serr := c.signIn(ctx); serr == nil {
			log.Info("signed in to Azure", "method", kind)
			return cred, kind, nil
		} else {
			log.Warn("sign-in did not succeed, continuing anonymously", "error", serr)
		}
	}

	log.Warn("no Azure credential found; continuing anonymously, "+
		"which only works for containers that allow public read access",
		"hint", "run `az login`, or pass a SAS token in the URL")
	return nil, "anonymous", nil
}

func (c *Credentials) deviceCode() (*azidentity.DeviceCodeCredential, error) {
	return azidentity.NewDeviceCodeCredential(&azidentity.DeviceCodeCredentialOptions{
		TenantID: c.TenantID,
		Cache:    c.tokenCache(),
		UserPrompt: func(_ context.Context, m azidentity.DeviceCodeMessage) error {
			// A prompt, not a log record: it has to appear whatever the log
			// level, and it has to stand the progress display down first or it
			// will be drawn over.
			logx.WithTerminal(func() {
				fmt.Fprintf(os.Stderr, "\n%s\n\n", m.Message)
			})
			return nil
		},
	})
}

// Timeouts for the token request that validates a credential. Discovery should
// be quick; a person completing a sign-in needs far longer.
const (
	discoveryTimeout   = 90 * time.Second
	interactiveTimeout = 10 * time.Minute
)

// probe forces a token request so that an unusable credential is reported now,
// with a clear message, instead of surfacing as a puzzling 401 mid-transfer.
func probe(ctx context.Context, cred azcore.TokenCredential) error {
	return probeWithin(ctx, cred, discoveryTimeout)
}

func probeWithin(ctx context.Context, cred azcore.TokenCredential, d time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, d)
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
