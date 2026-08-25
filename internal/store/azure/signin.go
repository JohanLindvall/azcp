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
// request — and the useful response to each is to offer a sign-in rather than
// to print a status code and stop.
//
// The escalation happens once per run and only where somebody can answer it.

// signInState guards the one interactive escalation a run is allowed.
type signInState struct {
	mu   sync.Mutex
	done bool
}

// withSignIn runs fn and, if the storage service rejects the credential,
// signs in and runs it once more. fn must be safe to call twice.
func (s *Store) withSignIn(ctx context.Context, fn func() error) error {
	err := fn()
	if err == nil || !s.shouldSignIn(err) {
		return s.explainAuth(err)
	}
	if !s.escalate(ctx, err) {
		return s.explainAuth(err)
	}
	return s.explainAuth(fn())
}

// shouldSignIn reports whether a failure is one that signing in could fix.
func (s *Store) shouldSignIn(err error) bool {
	if s.cfg.Auth == AuthAnonymous || !s.cfg.Interactive {
		return false
	}
	var re *azcore.ResponseError
	if !errors.As(err, &re) {
		return false
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

// escalate performs the sign-in, reporting whether the caller should try again.
func (s *Store) escalate(ctx context.Context, cause error) bool {
	s.signIn.mu.Lock()
	defer s.signIn.mu.Unlock()
	if s.signIn.done {
		// Someone else already signed in; the caller's retry will pick up the
		// new credential along with everyone else's.
		return true
	}
	// Announced rather than logged: being asked to sign in halfway through a
	// command needs an explanation at any log level.
	logx.Errf("azcp: the storage account rejected the current credential (%s); signing in…\n",
		retryx.Describe(cause))
	s.log.Debug("escalating to an interactive sign-in", "cause", cause)
	cred, err := s.creds.Escalate(ctx)
	if err != nil {
		s.log.Warn("could not sign in", "error", err)
		return false
	}
	if cred == nil {
		// Nothing was gained — a sign-in was already declined or was not
		// possible — so retrying would fail the same way.
		return false
	}
	s.signIn.done = true
	// Cached clients were built around the old credential; drop them so the
	// next call builds fresh ones.
	s.mu.Lock()
	clear(s.clients)
	s.mu.Unlock()
	return true
}

// explainAuth restates a credential rejection in terms the user can act on.
// "HTTP 401 InvalidAuthenticationInfo" is accurate and useless.
func (s *Store) explainAuth(err error) error {
	var re *azcore.ResponseError
	if err == nil || !errors.As(err, &re) {
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
	return fmt.Errorf("the storage account rejected the signed-in identity (%s): "+
		"it may belong to a different tenant — try --tenant=ID, or --auth=device "+
		"to sign in as somebody else", retryx.Describe(err))
}
