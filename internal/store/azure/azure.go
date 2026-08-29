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
	// IncludeMetadata asks listings to return each blob's metadata. It costs
	// a larger response, so it is only worth it when something will read it.
	IncludeMetadata bool
	// PeakRequests is how many requests this run can have outstanding at once.
	// It sizes the connection pool; see transport.go for why that matters.
	PeakRequests int
	// BytesPerSecond caps throughput across the whole run. Zero is unlimited.
	BytesPerSecond int64
}

// Store is the Azure Blob Storage namespace.
type Store struct {
	cfg   Config
	log   *slog.Logger
	creds *Credentials

	clientOnce sync.Once
	http       *http.Client

	// signIn guards the single interactive escalation a run is allowed, and
	// tenant the one directory a run follows the service to. authGen counts
	// both: an operation that failed under an older credential is worth
	// retrying for that reason alone.
	signIn  signInState
	tenant  tenantState
	authGen atomic.Uint64

	// noCopyRoute remembers, per endpoint and route, that a server-side copy
	// mechanism is not implemented there, so the rest of the run stops asking.
	noCopyRoute sync.Map

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
		Transport: s.httpClient(),
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

// httpClient returns the shared client, built once so every account in a run
// draws on the same connection pool.
func (s *Store) httpClient() *http.Client {
	s.clientOnce.Do(func() {
		s.http = newHTTPClient(s.cfg.PeakRequests+s.listAhead(), s.cfg.BytesPerSecond)
	})
	return s.http
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
	var n *store.Node
	err := s.withSignIn(ctx, func() error {
		var e error
		n, e = s.stat(ctx, u)
		return e
	})
	return n, err
}

func (s *Store) stat(ctx context.Context, u *uri.URL) (*store.Node, error) {
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
			return nil, err
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
			return nil, err
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
		return false, err
	}
	return len(page.Segment.BlobItems) == 0, nil
}

// ReadDir lists the immediate children of a container or prefix. Containers are
// listed when u addresses the account root.
func (s *Store) ReadDir(ctx context.Context, u *uri.URL) ([]*store.Node, error) {
	var out []*store.Node
	err := s.withSignIn(ctx, func() error {
		var e error
		out, e = s.readDir(ctx, u)
		return e
	})
	return out, err
}

func (s *Store) readDir(ctx context.Context, u *uri.URL) ([]*store.Node, error) {
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
	pager := cc.NewListBlobsHierarchyPager("/", &container.ListBlobsHierarchyOptions{
		Prefix:  &prefix,
		Include: container.ListBlobsInclude{Metadata: s.cfg.IncludeMetadata},
	})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			if isNotFound(err) {
				return nil, notExist(u, err)
			}
			return nil, err
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
	// Ties — a blob and a prefix sharing a name — put the directory first, so
	// deduplication below keeps it rather than whichever the sort left there.
	sort.SliceStable(out, func(i, j int) bool {
		if ni, nj := out[i].Name(), out[j].Name(); ni != nj {
			return ni < nj
		}
		return out[i].IsDir() && !out[j].IsDir()
	})
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

	// A walk streams results to fn, so it cannot simply be run twice. The
	// sign-in retry covers the first listing request, which is where a
	// rejected credential shows up; anything failing later has already
	// produced output and is reported as it is.
	first := true
	return s.withSignIn(ctx, func() error {
		if !first {
			s.log.Debug("restarting the listing after signing in", "path", u.Display())
		}
		first = false
		return s.walkAll(ctx, u, onError, fn)
	})
}

func (s *Store) walkAll(ctx context.Context, u *uri.URL,
	onError func(*uri.URL, error) error, fn func(*store.Node) error) error {

	if u.Container == "" {
		containers, err := s.listContainers(ctx, u)
		if err != nil {
			return err
		}
		return s.walkContainers(ctx, containers, onError, fn)
	}
	return s.walkContainer(ctx, u, fn)
}

// One listing per container is unavoidable — the service cannot list across
// containers — but doing them one after another is not. Each is a round trip
// that answers in tens of milliseconds with nothing else happening meanwhile,
// so an account of ten thousand small containers spends its whole scan waiting,
// and every transfer waits behind it. Measured against an endpoint 50ms away,
// 400 containers took 18.5s listed one at a time and 0.9s with a look-ahead of
// 64. The ordering cp depends on was never what made it slow.
const (
	// maxListAhead caps the listings in flight however large the run is.
	maxListAhead = 64
	// listShare is the fraction of the run's request budget the scan may use.
	// The transfers are what the budget is for; the scan only has to stay far
	// enough ahead of them.
	listShare = 4
	// listBuffer is how many nodes a look-ahead listing may hold before it
	// waits for the consumer, which is what bounds the memory this costs:
	// maxListAhead × listBuffer nodes, and no more.
	listBuffer = 256
)

// listAhead is how many containers this run lists at once.
func (s *Store) listAhead() int {
	return min(max(s.cfg.PeakRequests/listShare, 1), maxListAhead)
}

// walkContainers emits each container and everything inside it, in order,
// while several containers are being listed at once.
//
// What the caller sees is unchanged: fn is called from this goroutine, one
// container after another in the order they were listed, ancestors before
// their contents. Only the waiting is shared out.
func (s *Store) walkContainers(ctx context.Context, containers []*store.Node,
	onError func(*uri.URL, error) error, fn func(*store.Node) error) error {

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type listing struct {
		container *store.Node
		nodes     chan *store.Node
		err       chan error
	}

	// Bounded, so the dispatcher queues at most listAhead containers in front
	// of the one being consumed — that many listings are in flight, plus the
	// one in hand. It is what keeps both the goroutine count and the memory
	// finite on an account with a hundred thousand containers.
	pending := make(chan *listing, s.listAhead())

	go func() {
		defer close(pending)
		for _, c := range containers {
			l := &listing{
				container: c,
				nodes:     make(chan *store.Node, listBuffer),
				err:       make(chan error, 1),
			}
			select {
			case pending <- l:
			case <-ctx.Done():
				return
			}
			go func(c *store.Node, l *listing) {
				defer close(l.nodes)
				l.err <- s.walkContainer(ctx, c.URL, func(n *store.Node) error {
					select {
					case l.nodes <- n:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}(c, l)
		}
	}()

	for l := range pending {
		if err := fn(l.container); err != nil {
			return err
		}
		for n := range l.nodes {
			if err := fn(n); err != nil {
				return err
			}
		}
		if err := <-l.err; err != nil {
			if oerr := onError(l.container.URL, err); oerr != nil {
				return oerr
			}
		}
	}
	return ctx.Err()
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
	pager := cc.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix:  &prefix,
		Include: container.ListBlobsInclude{Metadata: s.cfg.IncludeMetadata},
	})
	for pager.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := pager.NextPage(ctx)
		if err != nil {
			if isNotFound(err) {
				return notExist(u, err)
			}
			return err
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
func (s *Store) MkdirAll(ctx context.Context, u *uri.URL, mode fs.FileMode) error {
	return s.withSignIn(ctx, func() error { return s.mkdirAll(ctx, u, mode) })
}

func (s *Store) mkdirAll(ctx context.Context, u *uri.URL, _ fs.FileMode) error {
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

// Remove deletes a blob. A directory in this namespace is usually just a
// prefix with nothing of its own to delete, but an empty one is a zero-byte
// marker blob, and removing the directory means removing the marker — the
// same way removing an empty local directory removes something real.
func (s *Store) Remove(ctx context.Context, u *uri.URL) error {
	if u.Key == "" {
		return fmt.Errorf("refusing to delete container %q: "+
			"this tool only removes blobs", u.Container)
	}
	cc, err := s.containerClient(ctx, u)
	if err != nil {
		return err
	}
	err = deleteBlob(ctx, cc, u.Key)
	if err == nil {
		return nil
	}
	if !isNotFound(err) {
		return err
	}
	if !strings.HasSuffix(u.Key, "/") {
		if merr := deleteBlob(ctx, cc, u.Key+"/"); merr == nil || !isNotFound(merr) {
			return merr
		}
	}
	return notExist(u, err)
}

func deleteBlob(ctx context.Context, cc *container.Client, key string) error {
	_, err := cc.NewBlobClient(key).Delete(ctx, nil)
	return err
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func blobNode(u *uri.URL, props *blob.GetPropertiesResponse) *store.Node {
	n := &store.Node{URL: u, Kind: store.KindFile, Mode: 0o644}
	n.Metadata = flatten(props.Metadata)
	n.ContentEncoding = deref(props.ContentEncoding)
	n.ContentDisposition = deref(props.ContentDisposition)
	n.ContentLanguage = deref(props.ContentLanguage)
	n.CacheControl = deref(props.CacheControl)
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
	n.Metadata = flatten(b.Metadata)
	if p := b.Properties; p != nil {
		n.ContentEncoding = deref(p.ContentEncoding)
		n.ContentDisposition = deref(p.ContentDisposition)
		n.ContentLanguage = deref(p.ContentLanguage)
		n.CacheControl = deref(p.CacheControl)
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

// flatten turns the SDK's map of pointers into a plain one; a nil value and an
// absent key mean the same thing here.
func flatten(m map[string]*string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if v != nil {
			out[k] = *v
		}
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
