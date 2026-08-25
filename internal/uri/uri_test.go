package uri

import (
	"path/filepath"
	"testing"
)

func TestParseRemote(t *testing.T) {
	cases := []struct {
		in                              string
		host, acct, container, key, sas string
	}{
		{"azure://foo.blob.core.windows.net/c/a/b.txt", "foo.blob.core.windows.net", "foo", "c", "a/b.txt", ""},
		{"azure://foo/c/a/b.txt", "foo.blob.core.windows.net", "foo", "c", "a/b.txt", ""},
		{"az://foo/c", "foo.blob.core.windows.net", "foo", "c", "", ""},
		{"azure://foo/", "foo.blob.core.windows.net", "foo", "", "", ""},
		{"https://foo.blob.core.windows.net/c/k?sv=x&sig=y", "foo.blob.core.windows.net", "foo", "c", "k", "sv=x&sig=y"},
		{"azure://foo/c/a%20b.txt", "foo.blob.core.windows.net", "foo", "c", "a b.txt", ""},
		{"azure://foo/c/deep/nested/key/", "foo.blob.core.windows.net", "foo", "c", "deep/nested/key", ""},
	}
	for _, c := range cases {
		u, err := Parse(c.in, Options{})
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if !u.IsRemote() {
			t.Errorf("Parse(%q) not remote", c.in)
			continue
		}
		if u.Host != c.host || u.Account != c.acct || u.Container != c.container || u.Key != c.key || u.SAS != c.sas {
			t.Errorf("Parse(%q) = host=%q acct=%q cont=%q key=%q sas=%q",
				c.in, u.Host, u.Account, u.Container, u.Key, u.SAS)
		}
	}
}

func TestParseEmulator(t *testing.T) {
	u, err := Parse("http://127.0.0.1:10000/devstoreaccount1/c/k", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !u.PathStyle || u.Account != "devstoreaccount1" || u.Container != "c" || u.Key != "k" || u.Secure {
		t.Fatalf("emulator parse wrong: %+v", u)
	}
	if got := u.ServiceURL(); got != "http://127.0.0.1:10000/devstoreaccount1" {
		t.Errorf("ServiceURL = %q", got)
	}
}

func TestParseLocal(t *testing.T) {
	for _, s := range []string{"a/b.txt", "/abs/path", "./rel", "weird:name.txt", "C:/x", "-"} {
		u, err := Parse(s, Options{})
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		if u.IsRemote() || u.Path != s {
			t.Errorf("Parse(%q) = %+v, want local verbatim", s, u)
		}
	}
	u, _ := Parse("dir/", Options{})
	if !u.TrailingSlash {
		t.Error("trailing slash not recorded")
	}
}

func TestParseRejectsNonBlobHTTP(t *testing.T) {
	if _, err := Parse("https://example.com/a/b", Options{}); err == nil {
		t.Error("expected rejection of non-blob https URL")
	}
}

func TestJoinBaseDir(t *testing.T) {
	u, _ := Parse("azure://foo/c/a", Options{})
	if got := u.Join("b", "c.txt").String(); got != "azure://foo.blob.core.windows.net/c/a/b/c.txt" {
		t.Errorf("Join = %q", got)
	}
	if got := u.Base(); got != "a" {
		t.Errorf("Base = %q", got)
	}
	if got := u.Dir().String(); got != "azure://foo.blob.core.windows.net/c" {
		t.Errorf("Dir = %q", got)
	}
	l, _ := Parse("x/y", Options{})
	if got := l.Join("z").Path; got != "x/y/z" {
		t.Errorf("local Join = %q", got)
	}
	if got := l.Dir().Path; got != "x" {
		t.Errorf("local Dir = %q", got)
	}
	root, _ := Parse("azure://foo/c", Options{})
	if got := root.PathPart(); got != "c" {
		t.Errorf("PathPart = %q", got)
	}
}

func TestSASNotLeaked(t *testing.T) {
	u, _ := Parse("azure://foo/c/k?sig=SECRET", Options{})
	if got := u.Display(); got == "" || contains(got, "SECRET") {
		t.Errorf("Display leaked SAS: %q", got)
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}

// The root of an account has no path element; the account name stands in for
// one so that "copy the whole account into this directory" has a name to use.
func TestBaseOfAccountRoot(t *testing.T) {
	for _, in := range []string{
		"azure://foo.blob.core.windows.net/",
		"azure://foo.blob.core.windows.net",
		"azure://foo/",
	} {
		u, err := Parse(in, Options{})
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if got := u.Base(); got != "foo" {
			t.Errorf("Parse(%q).Base() = %q, want %q", in, got, "foo")
		}
	}
	// A container still names itself, not the account.
	u, _ := Parse("azure://foo/mycontainer", Options{})
	if got := u.Base(); got != "mycontainer" {
		t.Errorf("container Base = %q", got)
	}
	// Local paths are unaffected.
	l, _ := Parse("/", Options{})
	if got := l.Base(); got != "" {
		t.Errorf("local root Base = %q, want empty", got)
	}
}

// Windows paths arrive with backslashes; every path decision here is made on
// "/" boundaries, so they are normalised on the way in. This runs everywhere:
// on Unix it checks the normalisation is a no-op and leaves odd names alone.
func TestLocalPathSeparators(t *testing.T) {
	if filepath.Separator == '\\' {
		u, err := Parse(`C:\Users\jl\src`, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if u.Path != "C:/Users/jl/src" {
			t.Errorf("Path = %q, want forward slashes", u.Path)
		}
		if got := u.Base(); got != "src" {
			t.Errorf("Base = %q, want %q", got, "src")
		}
		if got := u.Join("x").Path; got != "C:/Users/jl/src/x" {
			t.Errorf("Join = %q", got)
		}
		if got := u.Dir().Path; got != "C:/Users/jl" {
			t.Errorf("Dir = %q", got)
		}
		return
	}
	// On Unix a backslash is an ordinary character in a file name and must
	// survive untouched.
	u, err := Parse(`weird\name.txt`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != `weird\name.txt` {
		t.Errorf("Path = %q, want the backslash left alone", u.Path)
	}
	if got := u.Base(); got != `weird\name.txt` {
		t.Errorf("Base = %q", got)
	}
}
