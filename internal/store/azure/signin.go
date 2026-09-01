package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"github.com/JohanLindvall/azcp/internal/logx"
	"github.com/JohanLindvall/azcp/internal/retryx"
)

// Discovery finds a credential; only the service can say whether it is the
// right one. An `az login` session for another tenant, an expired assignment,
// or no identity at all all produce the same symptom — a rejection on the first
// request — and the useful response to each is to improve the credential rather
// than to print a status code and stop.
//
// There are two ways to improve it. The service names the tenant it trusts when
// it turns a token away, and following that asks nothing of anybody. Failing
// that, there is a sign-in, which happens once per run and only where somebody
// can answer it.

// signInState guards the one interactive escalation a run is allowed.
type signInState struct {
	mu sync.Mutex
	// done records that escalation is spent: the sign-in happened, or it
	// cannot happen at all.
	done bool
	// resumed records that a saved sign-in has been offered as the answer
	// once. It costs nobody anything, so it does not spend the prompt, but it
	// is only worth trying once.
	resumed bool
}

// tenantState guards the tenant a rejection named, and the answer to asking
// for a token there.
type tenantState struct {
	mu    sync.Mutex
	known string
	// refusal is the most recent failure to get a token for that tenant. It is
	// matched by identity rather than by type, so a stale one cannot be
	// mistaken for a fresh failure.
	refusal error
}

// withSignIn runs fn and, if the storage service rejects the credential,
// improves the credential and runs it again. fn must be safe to call more than
// once.
func (s *Store) withSignIn(ctx context.Context, fn func() error) error {
	for {
		gen := s.authGen.Load()
		err := fn()
		if err == nil {
			return nil
		}
		if !s.refreshAuth(ctx, gen, err) {
			return s.explainAuth(err)
		}
	}
}

// withSignInValue is withSignIn for an operation that produces something.
func withSignInValue[T any](ctx context.Context, s *Store, fn func() (T, error)) (T, error) {
	var v T
	err := s.withSignIn(ctx, func() error {
		var e error
		v, e = fn()
		return e
	})
	return v, err
}

// refreshAuth answers a rejection and reports whether the operation is worth
// trying again. gen is the credential generation the failed attempt ran under:
// if the credential has moved on since, the retry needs nothing more than that,
// which is what keeps twenty rejected transfers to one sign-in between them.
//
// Each way of improving the credential is taken at most once per run, so the
// generation can only advance a fixed number of times and the caller's loop is
// bounded.
func (s *Store) refreshAuth(ctx context.Context, gen uint64, err error) bool {
	if s.authGen.Load() != gen {
		return true
	}
	if s.adoptTenant(err) {
		return true
	}
	if !s.shouldSignIn(err) {
		return false
	}
	return s.escalate(ctx, err)
}

// adoptTenant follows the tenant a rejection names, so that an identity which
// does have access there is used instead of a sign-in being demanded for it. It
// reports whether anything changed.
func (s *Store) adoptTenant(err error) bool {
	if s.cfg.TenantID != "" {
		// The user named a tenant; the service does not get to overrule it.
		return false
	}
	tenant := challengeTenant(err)
	if tenant == "" {
		return false
	}
	s.tenant.mu.Lock()
	defer s.tenant.mu.Unlock()
	if s.tenant.known != "" {
		// One credential cannot serve two directories, and following a second
		// challenge would only undo the first.
		return false
	}
	s.tenant.known = tenant
	if !s.creds.UseTenant(tenant, s.noteTenantRefusal) {
		return false
	}
	s.log.Info("the storage account named the tenant it trusts", "tenant", tenant)
	s.dropClients()
	s.authGen.Add(1)
	return true
}

// knownTenant returns the tenant the service named, if it has named one.
func (s *Store) knownTenant() string {
	s.tenant.mu.Lock()
	defer s.tenant.mu.Unlock()
	return s.tenant.known
}

// noteTenantRefusal records that no token could be had for the tenant the
// account named.
func (s *Store) noteTenantRefusal(err error) {
	s.tenant.mu.Lock()
	defer s.tenant.mu.Unlock()
	s.tenant.refusal = err
}

// tenantRefused reports whether err is an operation failing because the
// identity in hand cannot get a token for the tenant the account named.
func (s *Store) tenantRefused(err error) bool {
	s.tenant.mu.Lock()
	refusal := s.tenant.refusal
	s.tenant.mu.Unlock()
	return refusal != nil && errors.Is(err, refusal)
}

// shouldSignIn reports whether a failure is one that signing in could fix.
func (s *Store) shouldSignIn(err error) bool {
	if s.cfg.Auth == AuthAnonymous || !s.cfg.Interactive {
		return false
	}
	var re *azcore.ResponseError
	if !errors.As(err, &re) {
		// Not a rejection by the service. The one failure here worth a sign-in
		// is the identity in hand being unable to get a token for the tenant
		// the account named, which nothing but another identity can fix.
		return s.tenantRefused(err)
	}
	switch re.StatusCode {
	case http.StatusUnauthorized:
		// The request carried no usable identity, or one the service would not
		// accept. Either way a fresh sign-in is the remedy.
		return true
	case http.StatusForbidden:
		// With an identity in hand, 403 means that identity lacks the role, and
		// signing in as the same person again changes nothing. With no identity
		// it means credentials were needed after all.
		return s.creds.IsAnonymous()
	}
	return false
}

// escalate performs the sign-in, reporting whether it improved the credential.
func (s *Store) escalate(ctx context.Context, cause error) bool {
	s.signIn.mu.Lock()
	defer s.signIn.mu.Unlock()
	if s.signIn.done {
		// The run's one sign-in is spent, and the caller's attempt already ran
		// with whatever it produced.
		return false
	}
	s.announce(cause)
	s.log.Debug("escalating to an interactive sign-in", "cause", cause)
	cred, kind, err := s.creds.Escalate(ctx)
	if err != nil {
		s.log.Warn("could not sign in", "error", err)
		s.signIn.done = true
		return false
	}
	if cred == nil {
		// Nothing was gained — a sign-in was already declined or was not
		// possible — so retrying would fail the same way.
		s.signIn.done = true
		return false
	}
	if kind == kindResumed && !s.signIn.resumed {
		// A sign-in saved by an earlier run cost nobody anything, so it does
		// not spend the prompt: if the account refuses that identity too,
		// there is still somebody to ask.
		s.signIn.resumed = true
	} else {
		s.signIn.done = true
	}
	s.dropClients()
	s.authGen.Add(1)
	return true
}

// announce says why a sign-in is about to happen. Announced rather than
// logged: being asked to sign in halfway through a command needs an
// explanation at any log level.
//
// Where the account has said which tenant it trusts, that is worth naming — it
// is the directory the browser will ask to sign in to — and a refusal to issue
// a token for it arrives with several lines of AADSTS explanation that belong
// in the log rather than here.
func (s *Store) announce(cause error) {
	tenant := s.knownTenant()
	if s.tenantRefused(cause) {
		logx.Errf("azcp: this storage account is in tenant %s, which the current "+
			"credential cannot get a token for; signing in…\n", tenant)
		return
	}
	signingIn := "signing in…"
	if tenant != "" {
		signingIn = "signing in to tenant " + tenant + "…"
	}
	logx.Errf("azcp: the storage account rejected the current credential (%s); %s\n",
		retryx.Describe(cause), signingIn)
}

// dropClients discards clients built around a credential that has been
// replaced, so the next call builds fresh ones.
func (s *Store) dropClients() {
	s.mu.Lock()
	clear(s.clients)
	s.mu.Unlock()
}

// explainAuth restates a credential rejection in terms the user can act on.
// "HTTP 401 InvalidAuthenticationInfo" is accurate and useless.
func (s *Store) explainAuth(err error) error {
	if err == nil {
		return nil
	}
	var re *azcore.ResponseError
	if !errors.As(err, &re) {
		if s.tenantRefused(err) {
			// The whole AADSTS answer runs to several lines and belongs in the
			// log, not in a cp-style message.
			s.log.Debug("no token could be had for the tenant the account named",
				"tenant", s.knownTenant(), "error", err)
			return fmt.Errorf("this storage account is in tenant %s, and the signed-in "+
				"identity cannot obtain a token for it: sign in with an account from "+
				"that tenant, or use --auth=device to sign in as somebody else",
				s.knownTenant())
		}
		return err
	}
	if re.StatusCode != http.StatusUnauthorized && re.StatusCode != http.StatusForbidden {
		return err
	}
	if !s.creds.Consulted() {
		// A SAS token or an account key was supplied for this account, so the
		// identity chain was never consulted and is not what was refused.
		return fmt.Errorf("the storage account rejected the credentials given for it (%s): "+
			"check the SAS token or account key, including its expiry and permissions",
			retryx.Describe(err))
	}
	if s.creds.IsAnonymous() {
		return fmt.Errorf("not signed in to Azure (%s): run `az login`, put a SAS "+
			"token in the URL, or run this in a terminal to sign in interactively",
			retryx.Describe(err))
	}
	if re.StatusCode == http.StatusForbidden {
		return fmt.Errorf("the signed-in identity is not allowed to do this (%s): "+
			"it needs a role such as Storage Blob Data Reader or Contributor on this account",
			retryx.Describe(err))
	}
	if tenant := s.knownTenant(); tenant != "" {
		return fmt.Errorf("this storage account is in tenant %s, which the signed-in "+
			"identity does not belong to (%s): sign in with an account from that tenant, "+
			"or use --auth=device to sign in as somebody else", tenant, retryx.Describe(err))
	}
	return fmt.Errorf("the storage account rejected the signed-in identity (%s): "+
		"it may belong to a different tenant — try --tenant=ID, or --auth=device "+
		"to sign in as somebody else", retryx.Describe(err))
}
