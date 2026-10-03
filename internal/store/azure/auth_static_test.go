package azure

import (
	"context"
	"strings"
	"testing"

	"github.com/JohanLindvall/azcp/internal/uri"
)

func TestStaticCredentialsStayInTheirAccount(t *testing.T) {
	t.Setenv("AZURE_STORAGE_ACCOUNT", "first")
	t.Setenv("AZURE_STORAGE_KEY", "a-key")
	t.Setenv("AZURE_STORAGE_SAS_TOKEN", "?sig=signature")
	for _, connection := range []string{
		"AccountName=first;AccountKey=a-key",
		"BlobEndpoint=https://first.blob.core.windows.net;SharedAccessSignature=sig=signature",
	} {
		t.Setenv("AZURE_STORAGE_CONNECTION_STRING", connection)
		if got := lookupStatic("first"); got.connectionString != connection || got.accountKey == "" || got.sas == "" {
			t.Fatal("matching account lost credentials")
		}
		if got := lookupStatic("second"); got != (staticCredentials{}) {
			t.Fatal("credentials leaked to another account")
		}
	}
	t.Setenv("AZURE_STORAGE_CONNECTION_STRING", "UseDevelopmentStorage=true")
	if lookupStatic("devstoreaccount1").connectionString == "" || lookupStatic("first").connectionString != "" {
		t.Fatal("development connection matched the wrong account")
	}
}

func TestConnectionStringCannotRedirectAnotherAccount(t *testing.T) {
	t.Setenv("AZURE_STORAGE_CONNECTION_STRING", "AccountName=first;AccountKey=a2V5;BlobEndpoint=https://first.blob.core.windows.net")
	t.Setenv("AZURE_STORAGE_ACCOUNT", "first")
	t.Setenv("AZURE_STORAGE_SAS_TOKEN", "")
	t.Setenv("AZURE_STORAGE_SAS", "")
	s := New(Config{})
	s.creds.resolved = true // anonymous fallback, without consulting real credentials
	u, err := uri.Parse("azure://second/c/blob", uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.client(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.URL(), "https://second.blob.core.windows.net") {
		t.Fatalf("redirected to %s", c.URL())
	}
}

func TestClientsAreNotReusedAcrossCredentialGenerations(t *testing.T) {
	s := New(Config{Auth: AuthAnonymous})
	u, err := uri.Parse("azure://account/c/blob", uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.client(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	// A late constructor can refill the cache after dropClients. Such an old
	// entry must not be used in the next credential generation.
	s.authGen.Add(1)
	current, err := s.client(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	if old == current {
		t.Fatal("reused a client built with a rejected credential")
	}
}

func TestAuthenticationCannotPromptTwice(t *testing.T) {
	c := &Credentials{}
	if _, err := c.authenticate(context.Background(), &tokenRecorder{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.authenticate(context.Background(), &tokenRecorder{}); err == nil {
		t.Fatal("second prompt was allowed")
	}
	if c.Prompts() != 1 {
		t.Fatalf("prompts = %d", c.Prompts())
	}
}
