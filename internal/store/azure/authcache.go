package azure

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// Signing in has to survive the process, or every command is another trip to
// the browser. Two things are kept:
//
//   - the tokens, in whatever secure store the platform provides (a keyring on
//     Linux, the keychain on macOS, DPAPI on Windows), handled by the SDK;
//   - an authentication record, which is not secret — an account id, a tenant,
//     a username — and only says which cached account to look for.
//
// Neither is usable alone, and where the platform offers no secure store both
// degrade to memory: the run still works, it just has to sign in again next
// time.

// recordPath is where the authentication record lives. A run pinned to a
// particular tenant keeps its own, since the accounts differ.
func (c *Credentials) recordPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	name := "auth-record.json"
	if c.TenantID != "" {
		name = "auth-record-" + safeFileName(c.TenantID) + ".json"
	}
	return filepath.Join(dir, "azcp", name), nil
}

// safeFileName keeps a tenant id from escaping the directory it names a file
// in. Dots are replaced along with everything else, so no run of them can form
// a parent reference; "contoso.onmicrosoft.com" stays perfectly readable as
// "contoso_onmicrosoft_com".
func safeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		default:
			return '_'
		}
	}, s)
}

func (c *Credentials) loadRecord() (azidentity.AuthenticationRecord, bool) {
	path, err := c.recordPath()
	if err != nil {
		return azidentity.AuthenticationRecord{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return azidentity.AuthenticationRecord{}, false
	}
	var rec azidentity.AuthenticationRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		// A record written by an incompatible version. Signing in again
		// replaces it, so this is not worth reporting as a problem.
		c.logger().Debug("ignoring an unreadable authentication record",
			"path", path, "error", err)
		return azidentity.AuthenticationRecord{}, false
	}
	return rec, true
}

func (c *Credentials) saveRecord(rec azidentity.AuthenticationRecord) {
	path, err := c.recordPath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		c.logger().Debug("cannot create the configuration directory",
			"path", filepath.Dir(path), "error", err)
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		c.logger().Debug("cannot save the authentication record",
			"path", path, "error", err)
		return
	}
	c.logger().Debug("saved the authentication record; "+
		"later runs will not need to sign in again",
		"path", path, "account", rec.Username)
}

// resume rebuilds a credential from a previous sign-in. It can never prompt:
// automatic authentication is disabled and the prompt refuses, so the worst
// case is that it reports there is nothing to resume.
func (c *Credentials) resume(ctx context.Context) (azcore.TokenCredential, bool) {
	rec, ok := c.loadRecord()
	if !ok {
		return nil, false
	}
	cred, err := azidentity.NewDeviceCodeCredential(&azidentity.DeviceCodeCredentialOptions{
		TenantID:             c.TenantID,
		AuthenticationRecord: rec,
		Cache:                c.tokenCache(),
		// Silent or nothing: if the cached tokens have expired this must
		// report failure rather than start a sign-in behind the caller's back.
		DisableAutomaticAuthentication: true,
		UserPrompt:                     refuseToPrompt,
	})
	if err != nil {
		return nil, false
	}
	if err := probeWithin(ctx, cred, discoveryTimeout); err != nil {
		var required *azidentity.AuthenticationRequiredError
		if errors.As(err, &required) {
			c.logger().Debug("the saved sign-in has expired", "account", rec.Username)
		} else {
			c.logger().Debug("could not resume the saved sign-in", "error", err)
		}
		return nil, false
	}
	c.logger().Debug("resumed a previous sign-in", "account", rec.Username)
	return cred, true
}

func refuseToPrompt(context.Context, azidentity.DeviceCodeMessage) error {
	return errors.New("a sign-in was needed but this credential is not allowed to ask")
}

// authenticator is the part of an interactive credential that performs the
// sign-in and reports which account did it.
type authenticator interface {
	azcore.TokenCredential
	Authenticate(context.Context, *policy.TokenRequestOptions) (azidentity.AuthenticationRecord, error)
}

// authenticate runs the interactive flow. It is the only place in this package
// that can make a browser open or a code appear, which is what makes "at most
// one prompt per run" checkable.
func (c *Credentials) authenticate(ctx context.Context, a authenticator) (azidentity.AuthenticationRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, interactiveTimeout)
	defer cancel()
	c.prompts++
	return a.Authenticate(ctx, &policy.TokenRequestOptions{Scopes: []string{storageScope}})
}

// Prompts reports how many interactive sign-ins this run has started. A
// correctly behaved run never exceeds one.
func (c *Credentials) Prompts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.prompts
}
