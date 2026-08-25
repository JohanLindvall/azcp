// Package azure implements the store interface over Azure Blob Storage.
//
// Blob storage has no directories, only names that happen to contain slashes.
// This package presents the flat namespace as a tree so that cp's rules about
// files and directories keep working: a name with children behaves as a
// directory, a zero-byte blob whose name ends in "/" is an empty directory, and
// listings synthesise the intermediate prefixes a filesystem would have.
package azure

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"

	"github.com/JohanLindvall/azcp/internal/logx"
	"github.com/JohanLindvall/azcp/internal/retryx"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// Config configures the store.
type Config struct {
	Auth        AuthMode
	Log         *slog.Logger
	Interactive bool
	TenantID    string
	// MaxRetries is how many times the SDK pipeline retries a single HTTP
	// request before giving up. The engine layers whole-operation retries on
	// top of this for failures the pipeline cannot recover from.
	MaxRetries int32
	// TryTimeout bounds one HTTP attempt. Zero means no per-attempt bound,
	// which is right for large block transfers.
	TryTimeout time.Duration
	// CreateContainer allows the tool to create a missing destination
	// container instead of reporting it as absent.
	CreateContainer bool
	// UserAgent is appended to the SDK's telemetry string.
	UserAgent string
}

// Store is the Azure Blob Storage namespace.
type Store struct {
	cfg   Config
	log   *slog.Logger
	creds *Credentials

	// noServerCopy records that this endpoint rejected a server-side copy as
	// unimplemented, so the rest of the run streams without asking again.
	noServerCopy atomic.Bool

	mu      sync.Mutex
	clients map[string]*azblob.Client
	// madeContainers remembers containers already created or verified this run
	// so a large recursive upload does not re-check on every file.
	madeContainers map[string]bool
}

// New returns a store. No network traffic happens until the first operation.
func New(cfg Config) *Store {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Store{
		cfg: cfg,
		log: log,
		creds: &Credentials{
			Mode:        cfg.Auth,
			Log:         log,
			Interactive: cfg.Interactive,
			TenantID:    cfg.TenantID,
		},
		clients:        map[string]*azblob.Client{},
		madeContainers: map[string]bool{},
	}
}

func (s *Store) Scheme() string { return uri.SchemeAzure }

// Credentials exposes the credential resolver so the transfer path can obtain
// a bearer token for server-side copies.
func (s *Store) Credentials() *Credentials { return s.creds }

func (s *Store) clientOptions() *azblob.ClientOptions {
	return &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{
		Retry: policy.RetryOptions{
			MaxRetries: s.cfg.MaxRetries,
			TryTimeout: s.cfg.TryTimeout,
			// Substituting our own predicate serves two purposes: it keeps the
			// decision in one place alongside the whole-file retry loop, and it
			// gives us the one hook where a retry is definitely about to
			// happen, so every transient failure gets recorded.
			ShouldRetry: s.shouldRetry,
		},
		Telemetry: policy.TelemetryOptions{ApplicationID: s.cfg.UserAgent},
	}}
}

// shouldRetry decides whether the SDK should try a request again, and logs the
// failure that prompted it. It reproduces the SDK's default rule — the same
// status codes, plus transport-level faults — so behaviour is unchanged.
func (s *Store) shouldRetry(resp *http.Response, err error) bool {
	var retry bool
	switch {
	case err != nil:
		// The pipeline checks the caller's context before consulting us, so a
		// deadline reaching this point is the per-attempt timeout expiring —
		// the very case another attempt is meant to cover.
		retry = errors.Is(err, context.DeadlineExceeded) || retryx.IsTransient(err)
	case resp != nil:
		retry = retryx.RetryableStatus(resp.StatusCode)
	}
	if !retry {
		return false
	}
	attrs := make([]any, 0, 8)
	if resp != nil {
		attrs = append(attrs, "status", resp.StatusCode)
		if resp.Request != nil {
			attrs = append(attrs, "method", resp.Request.Method,
				"url", sanitizeURL(resp.Request.URL))
		}
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			attrs = append(attrs, "retry_after", ra)
		}
	}
	if err != nil {
		attrs = append(attrs, "error", logx.Redact(err.Error()))
	}
	s.log.Warn("request failed, retrying", attrs...)
	return true
}

// sanitizeURL drops the query string, which is where a SAS token lives.
func sanitizeURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	if c.RawQuery != "" {
		c.RawQuery = "<redacted>"
	}
	return c.String()
}

// client returns a client for the account addressed by u, creating it on first
// use. Credential discovery happens once per process and is shared by every
// account, except where a URL carries its own SAS token.
func (s *Store) client(ctx context.Context, u *uri.URL) (*azblob.Client, error) {
	key := u.ServiceURL() + "|" + u.SAS
	s.mu.Lock()
	c, ok := s.clients[key]
	s.mu.Unlock()
	if ok {
		return c, nil
	}

	c, how, err := s.newClient(ctx, u)
	if err != nil {
		return nil, err
	}
	s.log.Debug("connected to storage account",
		"account", u.Account, "endpoint", u.ServiceURL(), "auth", how)

	s.mu.Lock()
	// Another goroutine may have won the race; keep whichever is already
	// cached so every caller shares one connection pool.
	if existing, ok := s.clients[key]; ok {
		c = existing
	} else {
		s.clients[key] = c
	}
	s.mu.Unlock()
	return c, nil
}

func (s *Store) newClient(ctx context.Context, u *uri.URL) (*azblob.Client, string, error) {
	opts := s.clientOptions()

	// A SAS token on the URL is the most specific thing the user can say, so it
	// wins over everything else.
	if u.SAS != "" {
		c, err := azblob.NewClientWithNoCredential(u.ServiceURL()+"?"+u.SAS, opts)
		return c, "SAS token from the URL", err
	}
	if s.cfg.Auth == AuthAnonymous {
		c, err := azblob.NewClientWithNoCredential(u.ServiceURL(), opts)
		return c, "anonymous", err
	}

	static := lookupStatic(u.Account)
	switch {
	case static.connectionString != "":
		c, err := azblob.NewClientFromConnectionString(static.connectionString, opts)
		return c, "AZURE_STORAGE_CONNECTION_STRING", err
	case static.sas != "":
		c, err := azblob.NewClientWithNoCredential(u.ServiceURL()+"?"+static.sas, opts)
		return c, "AZURE_STORAGE_SAS_TOKEN", err
	case static.accountKey != "":
		cred, err := azblob.NewSharedKeyCredential(u.Account, static.accountKey)
		if err != nil {
			return nil, "", fmt.Errorf("account key for %s: %w", u.Account, err)
		}
		c, err := azblob.NewClientWithSharedKeyCredential(u.ServiceURL(), cred, opts)
		return c, "shared account key", err
	}

	cred, how, err := s.creds.Resolve(ctx)
	if err != nil {
		return nil, "", err
	}
	if cred == nil {
		c, err := azblob.NewClientWithNoCredential(u.ServiceURL(), opts)
		return c, how, err
	}
	c, err := azblob.NewClient(u.ServiceURL(), cred, opts)
	return c, how, err
}

func (s *Store) containerClient(ctx context.Context, u *uri.URL) (*container.Client, error) {
	c, err := s.client(ctx, u)
	if err != nil {
		return nil, err
	}
	return c.ServiceClient().NewContainerClient(u.Container), nil
}

func (s *Store) serviceClient(ctx context.Context, u *uri.URL) (*service.Client, error) {
	c, err := s.client(ctx, u)
	if err != nil {
		return nil, err
	}
	return c.ServiceClient(), nil
}

// ---------------------------------------------------------------------------
// Naming operations
// ---------------------------------------------------------------------------

// Stat describes the node at u. Because prefixes are not real objects, a name
// that is not a blob is probed once more as a prefix before being reported
// missing.
func (s *Store) Stat(ctx context.Context, u *uri.URL, _ bool) (*store.Node, error) {
	switch {
	case u.Container == "":
		// The account root always exists as far as the tool is concerned; it
		// behaves as a directory containing the containers.
		return &store.Node{URL: u, Kind: store.KindDir, Mode: fs.ModeDir | 0o755}, nil

	case u.Key == "":
		cc, err := s.containerClient(ctx, u)
		if err != nil {
			return nil, err
		}
		props, err := cc.GetProperties(ctx, nil)
		if err != nil {
			if isNotFound(err) {
				return nil, notExist(u, err)
			}
			return nil, fmt.Errorf("stat %s: %w", u.Display(), err)
		}
		n := &store.Node{URL: u, Kind: store.KindDir, Mode: fs.ModeDir | 0o755}
		if props.LastModified != nil {
			n.ModTime = *props.LastModified
		}
		return n, nil
	}

	// A trailing slash is an explicit statement that the user means a prefix.
	if !u.TrailingSlash {
		cc, err := s.containerClient(ctx, u)
		if err != nil {
			return nil, err
		}
		props, err := cc.NewBlobClient(u.Key).GetProperties(ctx, nil)
		if err == nil {
			return blobNode(u, &props), nil
		}
		if !isNotFound(err) {
			return nil, fmt.Errorf("stat %s: %w", u.Display(), err)
		}
	}

	// Not a blob: is anything filed underneath it?
	empty, err := s.prefixEmpty(ctx, u)
	if err != nil {
		return nil, err
	}
	if !empty {
		return &store.Node{URL: u, Kind: store.KindDir, Mode: fs.ModeDir | 0o755}, nil
	}
	return nil, notExist(u, nil)
}

// prefixEmpty reports whether nothing is filed under u's key as a prefix.
func (s *Store) prefixEmpty(ctx context.Context, u *uri.URL) (bool, error) {
	cc, err := s.containerClient(ctx, u)
	if err != nil {
		return false, err
	}
	prefix := u.Key + "/"
	one := int32(1)
	pager := cc.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix: &prefix, MaxResults: &one,
	})
	if !pager.More() {
		return true, nil
	}
	page, err := pager.NextPage(ctx)
	if err != nil {
		if isNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("list %s: %w", u.Display(), err)
	}
	return len(page.Segment.BlobItems) == 0, nil
}

// ReadDir lists the immediate children of a container or prefix. Containers are
// listed when u addresses the account root.
func (s *Store) ReadDir(ctx context.Context, u *uri.URL) ([]*store.Node, error) {
	if u.Container == "" {
		return s.listContainers(ctx, u)
	}
	cc, err := s.containerClient(ctx, u)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if u.Key != "" {
		prefix = strings.TrimSuffix(u.Key, "/") + "/"
	}
	var out []*store.Node
	pager := cc.NewListBlobsHierarchyPager("/", &container.ListBlobsHierarchyOptions{Prefix: &prefix})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			if isNotFound(err) {
				return nil, notExist(u, err)
			}
			return nil, fmt.Errorf("list %s: %w", u.Display(), err)
		}
		if page.Segment == nil {
			continue
		}
		for _, p := range page.Segment.BlobPrefixes {
			if p.Name == nil {
				continue
			}
			name := strings.TrimSuffix(*p.Name, "/")
			out = append(out, &store.Node{
				URL:  u.WithPathPart(u.Container + "/" + name),
				Kind: store.KindDir,
				Mode: fs.ModeDir | 0o755,
			})
		}
		for _, b := range page.Segment.BlobItems {
			if b.Name == nil || *b.Name == prefix {
				// The zero-byte marker that stands for the directory itself.
				continue
			}
			out = append(out, itemNode(u, b))
		}
	}
	// Hierarchical listings return prefixes and blobs in separate groups; sort
	// so callers see one lexical sequence, as they would from a filesystem.
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return dedupeByName(out), nil
}

func (s *Store) listContainers(ctx context.Context, u *uri.URL) ([]*store.Node, error) {
	sc, err := s.serviceClient(ctx, u)
	if err != nil {
		return nil, err
	}
	var out []*store.Node
	pager := sc.NewListContainersPager(nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list containers on %s: %w", u.Account, err)
		}
		for _, c := range page.ContainerItems {
			if c.Name == nil {
				continue
			}
			n := &store.Node{
				URL:  u.WithPathPart(*c.Name),
				Kind: store.KindDir,
				Mode: fs.ModeDir | 0o755,
			}
			if c.Properties != nil && c.Properties.LastModified != nil {
				n.ModTime = *c.Properties.LastModified
			}
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// WalkAll lists everything beneath u with a single flat listing per container,
// synthesising the intermediate prefixes so a "**" pattern sees the same tree
// shape it would on a filesystem.
func (s *Store) WalkAll(ctx context.Context, u *uri.URL,
	onError func(*uri.URL, error) error, fn func(*store.Node) error) error {

	if u.Container == "" {
		containers, err := s.listContainers(ctx, u)
		if err != nil {
			return err
		}
		for _, c := range containers {
			if err := fn(c); err != nil {
				return err
			}
			if err := s.walkContainer(ctx, c.URL, fn); err != nil {
				if oerr := onError(c.URL, err); oerr != nil {
					return oerr
				}
			}
		}
		return nil
	}
	return s.walkContainer(ctx, u, fn)
}

func (s *Store) walkContainer(ctx context.Context, u *uri.URL, fn func(*store.Node) error) error {
	cc, err := s.containerClient(ctx, u)
	if err != nil {
		return err
	}
	prefix := ""
	if u.Key != "" {
		prefix = strings.TrimSuffix(u.Key, "/") + "/"
	}
	seenDirs := map[string]bool{}
	pager := cc.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: &prefix})
	for pager.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := pager.NextPage(ctx)
		if err != nil {
			if isNotFound(err) {
				return notExist(u, err)
			}
			return fmt.Errorf("list %s: %w", u.Display(), err)
		}
		if page.Segment == nil {
			continue
		}
		for _, b := range page.Segment.BlobItems {
			if b.Name == nil {
				continue
			}
			name := *b.Name
			// Emit the directories on the way down, once each, so patterns can
			// match a prefix even though no object exists for it.
			for _, dir := range ancestors(prefix, name) {
				if seenDirs[dir] {
					continue
				}
				seenDirs[dir] = true
				if err := fn(&store.Node{
					URL:  u.WithPathPart(u.Container + "/" + dir),
					Kind: store.KindDir,
					Mode: fs.ModeDir | 0o755,
				}); err != nil {
					return err
				}
			}
			if strings.HasSuffix(name, "/") {
				// An explicit empty-directory marker; already emitted above.
				continue
			}
			if err := fn(itemNode(u, b)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ancestors lists the directory prefixes of name that lie below base.
func ancestors(base, name string) []string {
	rest := strings.TrimPrefix(name, base)
	if rest == name && base != "" {
		return nil
	}
	var out []string
	for i, c := range rest {
		if c == '/' {
			out = append(out, base+rest[:i])
		}
	}
	return out
}

// MkdirAll makes u usable as a destination. Containers are the one part of the
// blob namespace that must really exist, so this verifies (and optionally
// creates) the container and does nothing else: prefixes spring into being when
// the first blob is written.
func (s *Store) MkdirAll(ctx context.Context, u *uri.URL, _ fs.FileMode) error {
	if u.Container == "" {
		return nil
	}
	key := u.Account + "/" + u.Container
	s.mu.Lock()
	done := s.madeContainers[key]
	s.mu.Unlock()
	if done {
		return nil
	}

	cc, err := s.containerClient(ctx, u)
	if err != nil {
		return err
	}
	_, err = cc.GetProperties(ctx, nil)
	switch {
	case err == nil:
	case !isNotFound(err):
		return fmt.Errorf("check container %s: %w", key, err)
	case !s.cfg.CreateContainer:
		return fmt.Errorf("container %q does not exist in account %q "+
			"(pass --create-container to create it)", u.Container, u.Account)
	default:
		if _, cerr := cc.Create(ctx, nil); cerr != nil &&
			!bloberror.HasCode(cerr, bloberror.ContainerAlreadyExists) {
			return fmt.Errorf("create container %s: %w", key, cerr)
		}
		s.log.Info("created container", "account", u.Account, "container", u.Container)
	}

	s.mu.Lock()
	s.madeContainers[key] = true
	s.mu.Unlock()
	return nil
}

// MkdirMarker writes the zero-byte blob that represents an empty directory, so
// that an empty directory survives a round trip through blob storage.
func (s *Store) MkdirMarker(ctx context.Context, u *uri.URL) error {
	if u.Key == "" {
		return nil
	}
	cc, err := s.containerClient(ctx, u)
	if err != nil {
		return err
	}
	name := strings.TrimSuffix(u.Key, "/") + "/"
	_, err = cc.NewBlockBlobClient(name).UploadBuffer(ctx, nil, nil)
	if err != nil {
		return fmt.Errorf("create directory marker %s: %w", u.Display(), err)
	}
	s.log.Debug("wrote directory marker", "blob", u.Display())
	return nil
}

// Remove deletes a blob.
func (s *Store) Remove(ctx context.Context, u *uri.URL) error {
	if u.Key == "" {
		return fmt.Errorf("refusing to delete container %q: "+
			"this tool only removes blobs", u.Container)
	}
	cc, err := s.containerClient(ctx, u)
	if err != nil {
		return err
	}
	if _, err := cc.NewBlobClient(u.Key).Delete(ctx, nil); err != nil {
		if isNotFound(err) {
			return notExist(u, err)
		}
		return fmt.Errorf("delete %s: %w", u.Display(), err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func blobNode(u *uri.URL, props *blob.GetPropertiesResponse) *store.Node {
	n := &store.Node{URL: u, Kind: store.KindFile, Mode: 0o644}
	if props.ContentLength != nil {
		n.Size = *props.ContentLength
	}
	if props.LastModified != nil {
		n.ModTime = *props.LastModified
	}
	if props.ContentType != nil {
		n.ContentType = *props.ContentType
	}
	if props.ETag != nil {
		n.ETag = string(*props.ETag)
	}
	if props.AccessTier != nil {
		n.AccessTier = *props.AccessTier
	}
	n.MD5 = props.ContentMD5
	// A zero-byte blob whose name ends in "/" is how every Azure tool spells an
	// empty directory.
	if n.Size == 0 && strings.HasSuffix(u.Key, "/") {
		n.Kind = store.KindDir
		n.Mode = fs.ModeDir | 0o755
	}
	return n
}

func itemNode(base *uri.URL, b *container.BlobItem) *store.Node {
	u := base.WithPathPart(base.Container + "/" + *b.Name)
	n := &store.Node{URL: u, Kind: store.KindFile, Mode: 0o644}
	if p := b.Properties; p != nil {
		if p.ContentLength != nil {
			n.Size = *p.ContentLength
		}
		if p.LastModified != nil {
			n.ModTime = *p.LastModified
		}
		if p.ContentType != nil {
			n.ContentType = *p.ContentType
		}
		if p.ETag != nil {
			n.ETag = string(*p.ETag)
		}
		if p.AccessTier != nil {
			n.AccessTier = string(*p.AccessTier)
		}
		n.MD5 = p.ContentMD5
	}
	if n.Size == 0 && strings.HasSuffix(*b.Name, "/") {
		n.Kind = store.KindDir
		n.Mode = fs.ModeDir | 0o755
	}
	return n
}

func dedupeByName(in []*store.Node) []*store.Node {
	out := in[:0]
	var last string
	for _, n := range in {
		if name := n.Name(); name != last {
			out = append(out, n)
			last = name
		}
	}
	return out
}

// isNotFound covers both a missing blob and a missing container.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound) {
		return true
	}
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound
}

// notExist wraps an error so callers can use store.IsNotExist regardless of
// which namespace produced it. The Azure error code is kept in the message
// because "container not found" and "blob not found" point at different
// mistakes.
func notExist(u *uri.URL, cause error) error {
	var respErr *azcore.ResponseError
	if errors.As(cause, &respErr) && respErr.ErrorCode != "" {
		return fmt.Errorf("%s (%s): %w", u.Display(), respErr.ErrorCode, store.ErrNotExist)
	}
	return fmt.Errorf("%s: %w", u.Display(), store.ErrNotExist)
}
