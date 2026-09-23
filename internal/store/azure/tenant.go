package azure

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// A credential names an identity, not a directory, and the two can disagree: a
// perfectly good token issued by the tenant the user happens to be signed in
// to is refused by an account that trusts another one, with the same HTTP 401
// InvalidAuthenticationInfo that means "no credential at all". The account
// supplies the missing half itself — every such rejection carries a challenge
// naming the tenant it will accept tokens from:
//
//	WWW-Authenticate: Bearer authorization_uri=https://login.microsoftonline.com/TENANT/oauth2/authorize resource_id=https://storage.azure.com
//
// The SDK parses that header and then throws the tenant away (a standing TODO
// in azblob's challenge policy), so it is read again here: once to ask the
// identity already in hand for a token in that tenant, which usually needs
// nobody's attention; then, when that identity is refused there, to ask the
// Azure CLI's other accounts in the tenant (cliaccount.go); and if all of that
// fails, to name the tenant in the message rather than leaving the user to
// guess it.

// challengeTenant returns the tenant a rejection names, or "" if it names none.
func challengeTenant(err error) string {
	var re *azcore.ResponseError
	if !errors.As(err, &re) || re.RawResponse == nil {
		return ""
	}
	return tenantFromAuthority(
		challengeParam(re.RawResponse.Header.Get("WWW-Authenticate"), "authorization_uri"))
}

// challengeParam extracts one parameter from a Bearer challenge. The values
// Entra sends hold neither spaces nor commas, quoted or not, so splitting on
// those separates them.
func challengeParam(header, name string) string {
	for _, field := range strings.FieldsFunc(header, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ','
	}) {
		if k, v, ok := strings.Cut(field, "="); ok && strings.EqualFold(k, name) {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// tenantFromAuthority picks the tenant out of an authorization URI.
func tenantFromAuthority(authority string) string {
	u, err := url.Parse(authority)
	if err != nil || u.Host == "" {
		return ""
	}
	tenant, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	switch strings.ToLower(tenant) {
	case "", "common", "organizations", "consumers":
		// Placeholder authorities name no particular directory, so there is
		// nothing here to learn.
		return ""
	}
	// The value goes on to a token request and into a file name, so it is held
	// to the shape of a tenant id — a GUID, or a domain such as
	// contoso.onmicrosoft.com — rather than trusted because it arrived over
	// TLS. Ending on a letter or a digit is what stops "..", which is a
	// perfectly good relative path and no kind of directory.
	if len(tenant) > 64 {
		return ""
	}
	for _, r := range tenant {
		switch {
		case alphanumeric(r), r == '.', r == '-':
		default:
			return ""
		}
	}
	if !alphanumeric(rune(tenant[0])) || !alphanumeric(rune(tenant[len(tenant)-1])) {
		return ""
	}
	return tenant
}

func alphanumeric(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

// tenantCredential asks for tokens in a particular tenant, wrapping a
// credential that was found without knowing which one to ask for.
type tenantCredential struct {
	azcore.TokenCredential
	tenant string
	// refused is told when no token can be had for the tenant. The error
	// itself cannot be relied on to say so — the identity chain reports it
	// with an unexported type — and the difference matters: an identity that
	// cannot go there is a reason to ask for another one.
	refused func(error)
	// cli is the Azure CLI's other accounts in the tenant, asked when the
	// identity in hand cannot get a token there. It is nil when the CLI holds
	// no other account in the tenant.
	cli *cliAccounts
}

func (t tenantCredential) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	if opts.TenantID == "" {
		opts.TenantID = t.tenant
	}
	// The CLI's accounts were found for this tenant, and answer for no other.
	cli := t.cli
	if opts.TenantID != t.tenant {
		cli = nil
	}
	// Once one of them has answered, it is asked first: the identity in hand
	// has been refused here already, and asking it again costs a process.
	if cli != nil && cli.hasAnswered() {
		if tk, err := cli.GetToken(ctx, opts); err == nil {
			return tk, nil
		}
	}
	tk, err := t.TokenCredential.GetToken(ctx, opts)
	if err != nil && cli != nil {
		var cerr error
		if tk, cerr = cli.GetToken(ctx, opts); cerr == nil {
			return tk, nil
		}
		err = errors.Join(err, cerr)
	}
	if err != nil && t.refused != nil {
		t.refused(err)
	}
	return tk, err
}

// forTenant points cred at a tenant, replacing any tenant already applied to it
// so a second challenge cannot stack another wrapper on the first. cli, which
// may be nil, is the Azure CLI's other accounts in that tenant.
func forTenant(cred azcore.TokenCredential, tenant string, refused func(error), cli *cliAccounts) azcore.TokenCredential {
	if t, ok := cred.(tenantCredential); ok {
		cred = t.TokenCredential
	}
	return tenantCredential{TokenCredential: cred, tenant: tenant, refused: refused, cli: cli}
}
