package azure

import (
	"context"
	"iter"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

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

// walkContainer emits every blob under u, and the directories a filesystem
// would have had on the way down to each.
func (s *Store) walkContainer(ctx context.Context, u *uri.URL, fn func(*store.Node) error) error {
	cc, err := s.containerClient(ctx, u)
	if err != nil {
		return err
	}
	prefix := ""
	if u.Key != "" {
		prefix = strings.TrimSuffix(u.Key, "/") + "/"
	}
	// Building the nodes is the listing goroutine's work, so a divided listing
	// shares that out along with the waiting.
	nodes := func(items []*container.BlobItem) []*store.Node {
		out := make([]*store.Node, 0, len(items))
		for _, b := range items {
			if b.Name != nil {
				out = append(out, itemNode(u, b))
			}
		}
		return out
	}

	seenDirs := map[string]bool{}
	emitDir := func(key string, n *store.Node) error {
		if seenDirs[key] {
			return nil
		}
		seenDirs[key] = true
		if n == nil {
			n = dirNode(u.WithPathPart(u.Container + "/" + key))
		}
		return fn(n)
	}
	emit := func(page []*store.Node) error {
		for _, n := range page {
			// Emit the directories on the way down, once each, so patterns can
			// match a prefix even though no object exists for it.
			for dir := range ancestors(prefix, n.URL.Key) {
				if err := emitDir(dir, nil); err != nil {
					return err
				}
			}
			if n.IsDir() {
				// A zero-byte blob whose name ended in "/": the marker for the
				// directory itself, which its own ancestors stop short of.
				if err := emitDir(n.URL.Key, n); err != nil {
					return err
				}
				continue
			}
			if err := fn(n); err != nil {
				return err
			}
		}
		return nil
	}
	if err := s.listUnder(ctx, cc, prefix, nodes, emit); err != nil {
		if isNotFound(err) {
			return notExist(u, err)
		}
		return err
	}
	return nil
}

// ancestors yields the directory prefixes of name that lie below base,
// outermost first.
//
// It yields rather than returns a slice because a listing asks this of every
// blob and the answer is nearly always one the walk already has: a directory of
// a hundred files names its parents a hundred times over, and only the first
// tells the caller anything. Handing back a fresh slice each time — three
// allocations for a path a few levels deep — was a per-blob cost for a per-
// directory fact.
func ancestors(base, name string) iter.Seq[string] {
	return func(yield func(string) bool) {
		if !strings.HasPrefix(name, base) {
			return
		}
		for i := len(base); i < len(name); i++ {
			if name[i] == '/' && !yield(name[:i]) {
				return
			}
		}
	}
}
