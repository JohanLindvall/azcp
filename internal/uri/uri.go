// Package uri parses the location arguments accepted on the command line. A
// location is either a plain local path or an Azure Blob Storage URL:
//
//	azure://ACCOUNT.blob.core.windows.net/CONTAINER/BLOB
//	azure://ACCOUNT/CONTAINER/BLOB                 (suffix filled in)
//	azure://ACCOUNT.blob.core.windows.net/C/B?SAS  (SAS token in the query)
//	https://ACCOUNT.blob.core.windows.net/C/B      (accepted for convenience)
//	http://127.0.0.1:10000/devstoreaccount1/C/B    (emulator, path-style)
//
// Anything that is not recognised as a remote URL is treated verbatim as a
// local path, so paths containing colons or percent signs are never mangled.
package uri

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// Scheme values for URL.Scheme.
const (
	SchemeFile  = "file"
	SchemeAzure = "azure"
)

// DefaultEndpointSuffix is appended to a bare account name. It can be
// overridden per-parse via Options, which the CLI wires to --endpoint-suffix
// and to AZURE_STORAGE_ENDPOINT_SUFFIX.
const DefaultEndpointSuffix = "blob.core.windows.net"

// URL is a parsed location.
type URL struct {
	Scheme string

	// Remote fields.
	Host      string // host[:port]
	Secure    bool   // https rather than http
	Account   string
	Container string
	Key       string
	SAS       string // query string, without the leading "?"
	PathStyle bool   // account appears in the path (storage emulator)

	// Local field.
	Path string

	// TrailingSlash records that the argument ended in "/", which cp uses to
	// distinguish "copy into" from "copy onto".
	TrailingSlash bool

	raw string
}

// Options tune parsing.
type Options struct {
	// EndpointSuffix is appended to bare account names. Empty means
	// DefaultEndpointSuffix.
	EndpointSuffix string
}

var (
	schemeRe = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.\-]*)://`)
	hostPort = regexp.MustCompile(`:[0-9]+$`)
)

// IsRemoteArg reports whether s looks like a remote URL, without fully parsing
// it. Used to decide early whether an argument needs credentials at all.
func IsRemoteArg(s string) bool {
	m := schemeRe.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	switch strings.ToLower(m[1]) {
	case "azure", "az":
		return true
	case "http", "https":
		return true
	}
	return false
}

// Parse parses a command-line location.
func Parse(s string, opt Options) (*URL, error) {
	if !IsRemoteArg(s) {
		return &URL{
			Scheme:        SchemeFile,
			Path:          s,
			TrailingSlash: len(s) > 1 && strings.HasSuffix(s, "/"),
			raw:           s,
		}, nil
	}
	m := schemeRe.FindStringSubmatch(s)
	scheme := strings.ToLower(m[1])
	rest := s[len(m[0]):]

	u := &URL{Scheme: SchemeAzure, Secure: true, raw: s}
	switch scheme {
	case "http":
		u.Secure = false
	case "https", "azure", "az":
	}

	// Split off the SAS query before touching the path, so a "?" inside a blob
	// name would have to be percent-encoded (as it must be in any URL).
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		u.SAS = strings.TrimPrefix(rest[i+1:], "?")
		rest = rest[:i]
	}

	host := rest
	pathPart := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		host, pathPart = rest[:i], rest[i+1:]
	}
	if host == "" {
		return nil, fmt.Errorf("%s: missing storage account", s)
	}
	u.TrailingSlash = strings.HasSuffix(pathPart, "/")

	suffix := opt.EndpointSuffix
	if suffix == "" {
		suffix = DefaultEndpointSuffix
	}

	switch {
	case hostPort.MatchString(host) || isIPLiteral(host):
		// Emulator / path-style: the first path element is the account.
		u.PathStyle = true
		u.Host = host
		acct, remainder := splitFirst(pathPart)
		if acct == "" {
			return nil, fmt.Errorf("%s: path-style URL needs an account name", s)
		}
		u.Account = acct
		pathPart = remainder
	case strings.Contains(host, "."):
		u.Host = host
		u.Account = host[:strings.IndexByte(host, '.')]
		if (scheme == "http" || scheme == "https") && !strings.Contains(host, ".blob.") &&
			!strings.HasSuffix(host, "."+suffix) {
			return nil, fmt.Errorf("%s: not an Azure Blob Storage endpoint "+
				"(expected a *.blob.* host, or use azure://)", s)
		}
	default:
		u.Account = host
		u.Host = host + "." + suffix
	}

	decoded, err := url.PathUnescape(pathPart)
	if err == nil {
		pathPart = decoded
	}
	u.Container, u.Key = splitFirst(pathPart)
	u.Key = strings.Trim(u.Key, "/")
	return u, nil
}

func splitFirst(p string) (first, rest string) {
	p = strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}

func isIPLiteral(host string) bool {
	if strings.HasPrefix(host, "[") {
		return true
	}
	for _, c := range host {
		if c != '.' && (c < '0' || c > '9') {
			return false
		}
	}
	return strings.Count(host, ".") == 3
}

// IsRemote reports whether u addresses blob storage.
func (u *URL) IsRemote() bool { return u.Scheme == SchemeAzure }

// PathPart returns the location's path in a single "/"-separated string, which
// is the space that pattern matching operates in. For blob storage that is
// "container/key"; for a local file it is the path as written.
func (u *URL) PathPart() string {
	if !u.IsRemote() {
		return u.Path
	}
	if u.Key == "" {
		return u.Container
	}
	return u.Container + "/" + u.Key
}

// WithPathPart returns a copy of u whose path is replaced. The result never
// carries the original's trailing-slash marker, since the new path is
// authoritative.
func (u *URL) WithPathPart(p string) *URL {
	c := *u
	c.TrailingSlash = false
	c.SAS = u.SAS
	if !u.IsRemote() {
		c.Path = p
		c.raw = p
		return &c
	}
	c.Container, c.Key = splitFirst(strings.TrimPrefix(p, "/"))
	c.Key = strings.Trim(c.Key, "/")
	c.raw = c.String()
	return &c
}

// Join appends path elements.
func (u *URL) Join(elems ...string) *URL {
	clean := make([]string, 0, len(elems))
	for _, e := range elems {
		if e = strings.Trim(e, "/"); e != "" {
			clean = append(clean, e)
		}
	}
	if len(clean) == 0 {
		return u.WithPathPart(u.PathPart())
	}
	base := u.PathPart()
	if !u.IsRemote() {
		// Preserve a local path's exact spelling ("." , "..", leading "/").
		return u.WithPathPart(path.Join(append([]string{base}, clean...)...))
	}
	if base == "" {
		return u.WithPathPart(strings.Join(clean, "/"))
	}
	return u.WithPathPart(strings.TrimRight(base, "/") + "/" + strings.Join(clean, "/"))
}

// Base returns the last path element, which is what cp appends to a directory
// destination.
func (u *URL) Base() string {
	p := strings.TrimRight(u.PathPart(), "/")
	if p == "" {
		return ""
	}
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Dir returns the location's parent.
func (u *URL) Dir() *URL {
	p := strings.TrimRight(u.PathPart(), "/")
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		if u.IsRemote() {
			return u.WithPathPart("")
		}
		return u.WithPathPart(".")
	}
	if i == 0 {
		return u.WithPathPart("/")
	}
	return u.WithPathPart(p[:i])
}

// ServiceURL returns the blob service endpoint for the account.
func (u *URL) ServiceURL() string {
	scheme := "https"
	if !u.Secure {
		scheme = "http"
	}
	if u.PathStyle {
		return fmt.Sprintf("%s://%s/%s", scheme, u.Host, u.Account)
	}
	return fmt.Sprintf("%s://%s", scheme, u.Host)
}

// String renders the location in the canonical form the tool accepts back.
func (u *URL) String() string {
	if !u.IsRemote() {
		return u.Path
	}
	var b strings.Builder
	if u.PathStyle {
		// A path-style endpoint is only reachable over the scheme it was given
		// on, so echoing "azure://" back would not round-trip.
		if u.Secure {
			b.WriteString("https://")
		} else {
			b.WriteString("http://")
		}
		b.WriteString(u.Host)
		b.WriteByte('/')
		b.WriteString(u.Account)
	} else {
		b.WriteString("azure://")
		b.WriteString(u.Host)
	}
	if u.Container != "" {
		b.WriteByte('/')
		b.WriteString(u.Container)
	}
	if u.Key != "" {
		b.WriteByte('/')
		b.WriteString(u.Key)
	}
	if u.SAS != "" {
		b.WriteString("?<sas>")
	}
	return b.String()
}

// Display is String with any credential material removed; it is what gets
// written to the terminal and to logs.
func (u *URL) Display() string { return u.String() }

// Raw returns the argument exactly as the user typed it.
func (u *URL) Raw() string { return u.raw }

// SameAccount reports whether two locations live in the same storage account,
// which determines whether a server-side copy is worth attempting.
func (u *URL) SameAccount(o *URL) bool {
	return u.IsRemote() && o.IsRemote() && u.Host == o.Host && u.Account == o.Account
}
