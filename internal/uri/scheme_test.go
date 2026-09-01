package uri

import "testing"

// Only a plain http:// asks for cleartext; the tool's own schemes mean TLS.
func TestSchemeDecidesTLS(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:10000/acct/c":        false,
		"HTTP://127.0.0.1:10000/acct/c":        false,
		"https://acct.blob.core.windows.net/c": true,
		"azure://acct/c":                       true,
		"az://acct/c":                          true,
	}
	for in, secure := range cases {
		u, err := Parse(in, Options{})
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}
		if u.Secure != secure {
			t.Errorf("Parse(%q).Secure = %v, want %v", in, u.Secure, secure)
		}
	}
}

func TestIsRemoteArg(t *testing.T) {
	remote := []string{"azure://a/c", "AZ://a", "https://a.blob.core.windows.net/c", "http://x/y"}
	for _, s := range remote {
		if !IsRemoteArg(s) {
			t.Errorf("%q was not taken for a remote location", s)
		}
	}
	local := []string{"a/b", "c:/windows/system32", "file://x", "ftp://x", "azure:/x", "./azure://x", ""}
	for _, s := range local {
		if IsRemoteArg(s) {
			t.Errorf("%q was taken for a remote location", s)
		}
	}
}

func TestServiceURL(t *testing.T) {
	cases := map[string]string{
		"azure://acct/c/k":                          "https://acct.blob.core.windows.net",
		"http://127.0.0.1:10000/devstoreaccount1/c": "http://127.0.0.1:10000/devstoreaccount1",
	}
	for in, want := range cases {
		u, err := Parse(in, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got := u.ServiceURL(); got != want {
			t.Errorf("ServiceURL(%q) = %q, want %q", in, got, want)
		}
	}
}
