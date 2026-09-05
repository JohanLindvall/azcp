package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/glob"
	"github.com/JohanLindvall/azcp/internal/logx"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/azure"
	"github.com/JohanLindvall/azcp/internal/store/local"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// scan walks the sources and feeds the workers. It runs on one goroutine so
// that directories are created before their contents, prompts are asked one at
// a time, and cp's ordering is preserved.
func (e *Engine) scan(ctx context.Context, out chan<- *task) error {
	dest, err := uri.Parse(e.opt.Dest, e.uriOK)
	if err != nil {
		return cli.Usage(err)
	}
	if err := e.checkUnsupported(dest); err != nil {
		return err
	}

	sources, err := e.expandSources(ctx)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		// Every source failed to resolve; the individual reasons are already
		// reported, and the non-zero exit status comes from the failure count.
		return nil
	}

	destNode, destErr := e.storeFor(dest).Stat(ctx, dest, true)
	if destErr != nil && !store.IsNotExist(destErr) {
		return fmt.Errorf("cannot access %s: %s", quote(dest.Display()), brief(destErr))
	}
	destIsDir, err := e.destIsDirectory(dest, destNode, len(sources))
	if err != nil {
		return err
	}

	for _, src := range sources {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		target, err := e.targetFor(ctx, dest, destIsDir, src)
		if err != nil {
			e.fail("%v", err)
			continue
		}
		if err := e.guardSelfCopy(src, target); err != nil {
			e.fail("%v", err)
			continue
		}
		e.rootDev, e.hasRootDev = local.DeviceOf(src.Sys)
		// Filter patterns are relative to what is being copied, not to how it
		// was spelled: --exclude 'build/**' prunes src/build whether the
		// source was written as ./src, /abs/path/src or azure://acct/c/src.
		// A named file is still matchable by its own name.
		rootRel := ""
		if !src.IsDir() {
			rootRel = src.URL.Base()
		}
		if err := e.plan(ctx, src, target, out, src.URL.Base(), rootRel, true); err != nil {
			return err
		}
	}
	return nil
}

// expandSources turns the source arguments into nodes, applying brace
// expansion and wildcard matching. A pattern that matches nothing, or a path
// that does not exist, is reported the way cp reports it and the scan carries
// on with the remaining arguments.
func (e *Engine) expandSources(ctx context.Context) ([]*store.Node, error) {
	var out []*store.Node
	for _, arg := range e.opt.Sources {
		raw := arg
		if e.opt.StripTrailingSlashes {
			raw = strings.TrimRight(raw, "/")
			if raw == "" {
				raw = "/"
			}
		}
		for _, expanded := range e.braceExpand(raw) {
			u, err := uri.Parse(expanded, e.uriOK)
			if err != nil {
				e.fail("%v", err)
				continue
			}
			nodes, err := e.resolveSource(ctx, u, u.Display())
			if err != nil {
				e.fail("%v", err)
				continue
			}
			out = append(out, nodes...)
		}
	}
	return out, ctx.Err()
}

func (e *Engine) braceExpand(arg string) []string {
	if e.opt.Glob == cli.GlobNever {
		return []string{arg}
	}
	return glob.ExpandBraces(arg)
}

// resolveSource stats or expands one source location.
func (e *Engine) resolveSource(ctx context.Context, u *uri.URL, arg string) ([]*store.Node, error) {
	s := e.storeFor(u)
	if !e.shouldGlob(ctx, u) {
		n, err := s.Stat(ctx, u, e.opt.DerefSource())
		if err != nil {
			if store.IsNotExist(err) {
				return nil, fmt.Errorf("cannot stat %s: No such file or directory", quote(arg))
			}
			return nil, fmt.Errorf("cannot stat %s: %s", quote(arg), brief(err))
		}
		return []*store.Node{n}, nil
	}

	nodes, err := store.Expand(ctx, s, u, store.ExpandOptions{
		Follow: e.opt.DerefSource(),
		Log:    e.log,
	})
	if err != nil {
		if store.IsNotExist(err) {
			return nil, fmt.Errorf("cannot stat %s: No such file or directory", quote(arg))
		}
		return nil, fmt.Errorf("cannot expand %s: %s", quote(arg), brief(err))
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no matches for %s", quote(arg))
	}
	e.log.Debug("pattern expanded", "pattern", arg, "matches", len(nodes))
	return nodes, nil
}

// shouldGlob decides whether an argument is a pattern. In the default mode a
// local path that exists exactly as written is never treated as a pattern, so a
// file genuinely named "report[final].pdf" still copies.
func (e *Engine) shouldGlob(ctx context.Context, u *uri.URL) bool {
	switch e.opt.Glob {
	case cli.GlobNever:
		return false
	case cli.GlobAlways:
		return glob.HasMeta(u.PathPart())
	}
	if !glob.HasMeta(u.PathPart()) {
		return false
	}
	if u.IsRemote() {
		// The shell cannot see into a container, so a remote pattern is always
		// ours to expand.
		return true
	}
	if _, err := e.local.Stat(ctx, u, false); err == nil {
		return false
	}
	return true
}

// destIsDirectory applies cp's rules for whether the destination names a
// directory to copy into or a file to copy onto.
func (e *Engine) destIsDirectory(dest *uri.URL, node *store.Node, nsources int) (bool, error) {
	if e.opt.HasTargetDir {
		if node == nil {
			return false, fmt.Errorf("target directory %s: No such file or directory",
				quote(dest.Display()))
		}
		if !node.IsDir() {
			return false, fmt.Errorf("target %s is not a directory", quote(dest.Display()))
		}
		return true, nil
	}
	if e.opt.NoTargetDirectory {
		return false, nil
	}
	if e.opt.Parents {
		if node == nil || !node.IsDir() {
			return false, errors.New("with --parents, the destination must be a directory")
		}
		return true, nil
	}
	if node != nil && node.IsDir() {
		return true, nil
	}
	if nsources > 1 {
		if node == nil {
			return false, fmt.Errorf("target %s: No such file or directory", quote(dest.Display()))
		}
		return false, fmt.Errorf("target %s is not a directory", quote(dest.Display()))
	}
	// A trailing slash asserts a directory. Blob storage has no real
	// directories, so the assertion is simply honoured; on a filesystem it has
	// to already exist.
	if dest.TrailingSlash {
		if dest.IsRemote() {
			return true, nil
		}
		return false, fmt.Errorf("cannot create regular file %s: Not a directory",
			quote(dest.Display()))
	}
	return false, nil
}

// targetFor computes where one source lands.
func (e *Engine) targetFor(ctx context.Context, dest *uri.URL, destIsDir bool, src *store.Node) (*uri.URL, error) {
	if e.opt.Parents {
		parts := glob.SplitPath(src.URL.PathPart())
		if len(parts) == 0 {
			return nil, fmt.Errorf("with --parents, %s has no path to reproduce",
				quote(src.URL.Display()))
		}
		target := dest.Join(parts...)
		// Recreate the intermediate directories the source path implies.
		if len(parts) > 1 {
			parent := dest.Join(parts[:len(parts)-1]...)
			if err := e.storeFor(parent).MkdirAll(ctx, parent, 0o755); err != nil {
				return nil, err
			}
		}
		return target, nil
	}
	if destIsDir {
		base := src.URL.Base()
		if base == "" {
			return nil, fmt.Errorf("cannot determine a name for %s", quote(src.URL.Display()))
		}
		return dest.Join(base), nil
	}
	return dest, nil
}

// guardSelfCopy refuses a copy whose destination lies inside its own source,
// which would otherwise recurse until the disk filled.
func (e *Engine) guardSelfCopy(src *store.Node, dst *uri.URL) error {
	if !src.IsDir() || src.URL.IsRemote() != dst.IsRemote() {
		return nil
	}
	if src.URL.IsRemote() && !src.URL.SameAccount(dst) {
		return nil
	}
	sp := glob.SplitPath(src.URL.PathPart())
	dp := glob.SplitPath(dst.PathPart())
	if len(dp) < len(sp) {
		return nil
	}
	for i := range sp {
		if sp[i] != dp[i] {
			return nil
		}
	}
	if len(dp) == len(sp) {
		return fmt.Errorf("%s and %s are the same file",
			quote(src.URL.Display()), quote(dst.Display()))
	}
	return fmt.Errorf("cannot copy a directory, %s, into itself, %s",
		quote(src.URL.Display()), quote(dst.Display()))
}

// plan turns one source node into tasks, recursing into directories.
func (e *Engine) plan(ctx context.Context, src *store.Node, dst *uri.URL,
	out chan<- *task, display, rel string, top bool) error {

	if err := ctx.Err(); err != nil {
		return err
	}
	// A named source is filtered too, so --exclude means the same thing
	// whether a file was reached by recursion or spelled out.
	if e.filter.active() && !src.IsDir() &&
		(!e.filter.allow(rel) || !e.filter.withinWindow(blobMTime(src))) {
		e.log.Debug("filtered out", "path", src.URL.Display(), "relative", rel)
		e.prog.Saw(1)
		e.markSkipped()
		return nil
	}
	// A symlink is resolved here rather than at transfer time, so everything
	// downstream sees the kind of thing it is actually copying.
	if src.IsSymlink() {
		if !e.derefAt(top) {
			return e.planSymlink(ctx, src, dst, out, display)
		}
		resolved, err := e.storeFor(src.URL).Stat(ctx, src.URL, true)
		if err != nil || resolved.IsSymlink() {
			// Nothing at the other end: cp reports the link as unstattable.
			e.fail("cannot stat %s: No such file or directory", quote(src.URL.Display()))
			return nil
		}
		src = resolved
	}

	switch {
	case src.IsDir():
		if !e.opt.Recursive {
			e.fail("-r not specified; omitting directory %s", quote(src.URL.Display()))
			return nil
		}
		return e.planDir(ctx, src, dst, out, display, rel)

	case src.Kind == store.KindOther:
		e.prog.Saw(1)
		e.note("skipping %s: %s cannot be copied",
			quote(src.URL.Display()), src.Kind.String())
		e.markSkipped()
		return nil

	default:
		return e.emit(ctx, src, dst, out, display)
	}
}

// derefAt reports whether a symlink at this position should be followed.
func (e *Engine) derefAt(top bool) bool {
	switch e.opt.Deref {
	case cli.DerefAlways:
		return true
	case cli.DerefNever:
		return false
	case cli.DerefCmdline:
		return top
	default:
		return !e.opt.Recursive
	}
}

func (e *Engine) planDir(ctx context.Context, src *store.Node, dst *uri.URL,
	out chan<- *task, display, rel string) error {

	// A depth limit is the backstop. Identity-based loop detection is exact,
	// but it depends on the platform being able to identify a file at all;
	// this holds wherever that does not.
	if depth := strings.Count(rel, "/"); depth > maxCopyDepth {
		e.fail("refusing to descend more than %d levels into %s "+
			"(a symbolic link loop?)", maxCopyDepth, quote(src.URL.Display()))
		return nil
	}

	// Following symbolic links can turn the tree into a graph. Remembering
	// which directories have been entered keeps a link that points back up
	// from looping forever.
	if e.opt.DerefWalk() && !src.URL.IsRemote() {
		if info, err := os.Stat(src.URL.Path); err == nil {
			if id, _, ok := local.IDOf(src.URL.Path, info); ok {
				if e.visitedDirs[id] {
					e.log.Warn("skipping directory already visited through a symbolic link",
						"path", src.URL.Display())
					return nil
				}
				e.visitedDirs[id] = true
			}
		}
	}

	if e.pruner != nil && rel == "" {
		// The top of a recursive copy is the only place a deletion may reach.
		e.pruner.root(dst)
	}
	mode := fs.FileMode(0o755)
	if e.opt.Preserve.Mode {
		mode = src.Mode.Perm()
	}
	if !e.opt.DryRun {
		if err := e.storeFor(dst).MkdirAll(ctx, dst, mode|0o200); err != nil {
			e.fail("cannot create directory %s: %s", quote(dst.Display()), brief(err))
			return nil
		}
	}

	// A remote subtree is enumerated in one go rather than a request per
	// prefix: see planRemoteTree.
	if src.URL.IsRemote() {
		return e.planRemoteTree(ctx, src, dst, out, display, rel)
	}

	entries, err := e.storeFor(src.URL).ReadDir(ctx, src.URL)
	if err != nil {
		if interrupted(ctx, err) {
			return nil
		}
		e.fail("cannot read directory %s: %s", quote(src.URL.Display()), brief(err))
		return nil
	}

	if len(entries) == 0 && dst.IsRemote() && !e.opt.DryRun {
		// Blob storage has no empty directories; write the marker every Azure
		// tool uses so the shape survives a round trip.
		if err := e.az.MkdirMarker(ctx, dst); err != nil {
			e.log.Warn("cannot record empty directory", "path", dst.Display(), "error", err)
		}
	}
	for _, child := range entries {
		if e.crossesFilesystem(child) {
			e.log.Info("not descending into a directory on another file system",
				"path", child.URL.Display())
			continue
		}
		childRel := joinRel(rel, child.Name())
		e.recordKept(dst.Join(child.Name()))
		// An excluded directory is pruned rather than walked and discarded.
		if child.IsDir() && !e.filter.descend(childRel) {
			e.log.Debug("pruning excluded directory", "path", child.URL.Display())
			continue
		}
		if err := e.plan(ctx, child, dst.Join(child.Name()), out,
			display+"/"+child.Name(), childRel, false); err != nil {
			return err
		}
	}

	// Directory attributes are applied once the contents are written, so that
	// a read-only mode or an old mtime cannot interfere with filling it.
	if !dst.IsRemote() && !src.URL.IsRemote() && e.opt.Preserve.Any() && !e.opt.DryRun {
		if info, err := os.Lstat(src.URL.Path); err == nil {
			e.deferredDirs = append(e.deferredDirs, deferredDir{path: dst.Path, info: info})
		}
	}
	return nil
}

// maxCopyDepth bounds how deep a recursive copy will go. Real trees are
// nowhere near this; a loop reaches it quickly.
const maxCopyDepth = 512

// recordKept notes that the source provides this destination path, so --delete
// leaves it alone. The path is recorded whether or not the entry is ultimately
// copied: a file skipped because it was already up to date is still a file the
// source has.
func (e *Engine) recordKept(dst *uri.URL) {
	if e.pruner == nil {
		return
	}
	for _, root := range e.pruner.roots {
		if r, ok := store.RelUnder(root.PathPart(), dst.PathPart()); ok && r != "" {
			e.pruner.wrote(root, r)
			return
		}
	}
}

// joinRel appends an element to a copy-root-relative path.
func joinRel(base, name string) string {
	if base == "" {
		return name
	}
	return base + "/" + name
}

// crossesFilesystem reports whether --one-file-system should stop at this
// entry. Like cp, only directories are considered: a mount point is a
// directory, and the files beside it are on the filesystem being copied.
func (e *Engine) crossesFilesystem(n *store.Node) bool {
	if !e.opt.OneFileSystem || !e.hasRootDev || !n.IsDir() || n.URL.IsRemote() {
		return false
	}
	dev, ok := local.DeviceOf(n.Sys)
	return ok && dev != e.rootDev
}

// planRemoteTree queues a whole remote subtree from a single flat listing.
//
// Descending prefix by prefix costs one request per directory and holds the
// first transfer up until the last directory has been listed. Against a real
// endpoint, where a round trip is tens of milliseconds, a few hundred
// directories is several seconds of doing nothing. One flat listing returns up
// to five thousand blobs per request, so the same tree is a couple of round
// trips and transfers begin immediately.
func (e *Engine) planRemoteTree(ctx context.Context, src *store.Node, dst *uri.URL,
	out chan<- *task, display, rootRel string) error {

	base := src.URL.PathPart()
	made := map[string]bool{}
	// Tracked only when the destination is blob storage, which needs a marker
	// blob to represent an empty directory.
	var empty *emptyDirs
	if dst.IsRemote() {
		empty = newEmptyDirs()
	}

	onError := func(u *uri.URL, err error) error {
		if interrupted(ctx, err) {
			return nil
		}
		e.fail("cannot read %s: %s", quote(u.Display()), brief(err))
		return nil
	}

	err := e.storeFor(src.URL).WalkAll(ctx, src.URL, onError, func(n *store.Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, ok := store.RelUnder(base, n.URL.PathPart())
		if !ok || rel == "" {
			return nil
		}
		filterRel := joinRel(rootRel, rel)
		target := dst.Join(strings.Split(rel, "/")...)
		e.recordKept(target)
		if n.IsDir() {
			if !e.filter.descend(filterRel) {
				return nil
			}
		} else if !e.filter.allow(filterRel) || !e.filter.withinWindow(blobMTime(n)) {
			e.prog.Saw(1)
			e.markSkipped()
			return nil
		}

		if n.IsDir() {
			e.ensureDir(ctx, target, made)
			empty.dir(target)
			return nil
		}
		e.ensureDir(ctx, target.Dir(), made)
		empty.file(target)
		return e.emit(ctx, n, target, out, display+"/"+rel)
	})
	if err != nil {
		return err
	}

	// Blob storage has no empty directories; give the ones that stayed empty
	// the marker every Azure tool uses so the shape survives the copy.
	if !e.opt.DryRun {
		for _, u := range empty.leaves() {
			if merr := e.az.MkdirMarker(ctx, u); merr != nil {
				e.log.Warn("cannot record empty directory",
					"path", u.Display(), "error", merr)
			}
		}
	}
	return nil
}

// emptyDirs works out which destination directories will end a remote copy
// with nothing at all beneath them. Only those need an empty-directory marker
// blob: anything under a prefix — a blob, or a deeper directory's own marker —
// already makes the prefix behave as a directory, so writing markers up the
// chain would spend a request per level saying what the leaf already says.
//
// It relies on the walk's order, ancestors before their contents: every
// directory is registered before anything beneath it arrives to clear it.
// A nil tracker (a local destination needs no markers) accepts every call and
// has no leaves.
type emptyDirs struct {
	byPath map[string]*uri.URL
}

func newEmptyDirs() *emptyDirs { return &emptyDirs{byPath: map[string]*uri.URL{}} }

// dir records a directory as empty until something arrives beneath it. Its
// ancestors stop being empty now: whatever this directory ends up holding —
// contents or its own marker — will sit under every one of them.
func (e *emptyDirs) dir(u *uri.URL) {
	if e == nil {
		return
	}
	e.occupied(u)
	e.byPath[u.PathPart()] = u
}

// file marks every directory on the way to a blob as having content.
func (e *emptyDirs) file(u *uri.URL) {
	if e == nil {
		return
	}
	e.occupied(u)
}

// occupied clears u's ancestors. The climb stops at the first directory
// already cleared, because whatever cleared it climbed on from there: an
// entry's absence always means the whole chain above it is absent too.
func (e *emptyDirs) occupied(u *uri.URL) {
	for d := u.Dir(); d.PathPart() != ""; d = d.Dir() {
		if _, ok := e.byPath[d.PathPart()]; !ok {
			return
		}
		delete(e.byPath, d.PathPart())
	}
}

// leaves returns the directories that stayed empty.
func (e *emptyDirs) leaves() []*uri.URL {
	if e == nil {
		return nil
	}
	out := make([]*uri.URL, 0, len(e.byPath))
	for _, u := range e.byPath {
		out = append(out, u)
	}
	return out
}

// ensureDir creates a destination directory once. A large tree would otherwise
// re-issue the same request for every file in it.
func (e *Engine) ensureDir(ctx context.Context, u *uri.URL, made map[string]bool) {
	key := u.PathPart()
	if made[key] {
		return
	}
	made[key] = true
	if e.opt.DryRun {
		return
	}
	if err := e.storeFor(u).MkdirAll(ctx, u, 0o755); err != nil {
		e.fail("cannot create directory %s: %s", quote(u.Display()), brief(err))
	}
}

func (e *Engine) planSymlink(ctx context.Context, src *store.Node, dst *uri.URL,
	out chan<- *task, display string) error {

	if dst.IsRemote() && !e.preservesToBlob() {
		e.prog.Saw(1)
		e.note("skipping symbolic link %s: blob storage has no links "+
			"(use -L to copy what it points at, or -a to record it)",
			quote(src.URL.Display()))
		e.markSkipped()
		return nil
	}
	return e.emit(ctx, src, dst, out, display)
}

// emit applies the overwrite rules and, if the file is to be copied, queues it.
func (e *Engine) emit(ctx context.Context, src *store.Node, dst *uri.URL,
	out chan<- *task, display string) error {

	e.prog.Saw(1)
	t := &task{src: src, dst: dst, display: display}

	if e.needsDestCheck() {
		dn, err := e.storeFor(dst).Stat(ctx, dst, false)
		switch {
		case err == nil:
			proceed, backup, derr := e.decideOverwrite(src, dn)
			if derr != nil {
				e.fail("%v", derr)
				return nil
			}
			if !proceed {
				e.log.Debug("skipping existing destination",
					"source", src.URL.Display(), "destination", dst.Display())
				e.markSkipped()
				return nil
			}
			t.backup = backup
		case !store.IsNotExist(err):
			e.fail("cannot access %s: %s", quote(dst.Display()), brief(err))
			return nil
		}
	}
	t.removeFirst = e.opt.RemoveDestination

	e.prog.Plan(1, src.Size)
	if e.opt.DryRun {
		if e.opt.Output == cli.OutputJSON {
			logx.Printf("%s\n", jsonLine(map[string]any{
				"event": "would-copy", "source": src.URL.Display(),
				"destination": dst.Display(), "bytes": src.Size,
			}))
		} else {
			logx.Printf("%s -> %s\n", quote(src.URL.Display()), quote(dst.Display()))
		}
		pt := e.prog.Begin(display, src.Size, direction(src.URL, dst))
		pt.Done(nil)
		return nil
	}
	select {
	case out <- t:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unfinishedDownload reports whether the destination is a download that never
// finished, which -n, -u and -i must not mistake for a copy already made.
func unfinishedDownload(src, dst *store.Node) bool {
	return src.URL.IsRemote() && !dst.URL.IsRemote() &&
		azure.IncompleteDownload(dst.URL.Path)
}

// needsDestCheck reports whether the destination has to be inspected before
// writing. Skipping the check when no option depends on it saves one round trip
// per file, which is the difference between a fast and a slow upload of many
// small files.
func (e *Engine) needsDestCheck() bool {
	return e.opt.NoClobber || e.opt.Interactive ||
		e.opt.Update != cli.UpdateAll || e.opt.Backup != cli.BackupNone
}

// decideOverwrite applies -n, -i, -u and -b to an existing destination.
func (e *Engine) decideOverwrite(src, dst *store.Node) (bool, string, error) {
	if unfinishedDownload(src, dst) {
		// Not a destination to be weighed against the source: it is this copy,
		// stopped part-way. Left alone it stays broken, and it is the one thing
		// on disk that looks finished without being it.
		return true, "", nil
	}
	if e.opt.NoClobber {
		return false, "", nil
	}
	switch e.opt.Update {
	case cli.UpdateNone:
		return false, "", nil
	case cli.UpdateNoneFail:
		return false, "", fmt.Errorf("not replacing %s", quote(dst.URL.Display()))
	case cli.UpdateOlder:
		// Compared on the timestamps -p restores, so a blob carrying its
		// file's original mtime is measured against that rather than against
		// the moment it was uploaded — otherwise `-au` out of blob storage
		// copies everything again on every run. Blob timestamps have
		// millisecond resolution, so an equal time means "not newer".
		if !blobMTime(src).After(blobMTime(dst)) {
			return false, "", nil
		}
	}
	if e.opt.Interactive {
		ok, err := e.promptOverwrite(dst.URL)
		if err != nil || !ok {
			return false, "", err
		}
	}
	if e.opt.Backup != cli.BackupNone {
		if dst.URL.IsRemote() {
			return false, "", fmt.Errorf("--backup is not supported for blob destinations (%s)",
				quote(dst.URL.Display()))
		}
		name, err := e.backupName(dst.URL.Path)
		if err != nil {
			return false, "", err
		}
		return true, name, nil
	}
	return true, "", nil
}

// promptOverwrite asks on the terminal, with the live display stood down.
func (e *Engine) promptOverwrite(dst *uri.URL) (bool, error) {
	e.promptMu.Lock()
	defer e.promptMu.Unlock()
	if e.cancelAll {
		return false, nil
	}
	if e.promptIn == nil {
		e.promptIn = bufio.NewReader(e.stdin)
	}
	var line string
	var readErr error
	logx.WithTerminal(func() {
		fmt.Fprintf(os.Stderr, "%s: overwrite %s? ", cli.Program, quote(dst.Display()))
		line, readErr = e.promptIn.ReadString('\n')
	})
	if readErr != nil {
		// No one is there to answer; treat that as "leave it alone" rather
		// than silently overwriting.
		e.cancelAll = true
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// markSkipped counts a file deliberately left alone, where the closing summary
// and the JSON report both read it from.
func (e *Engine) markSkipped() { e.prog.Skipped(1) }

// fail reports a problem with one source or destination and counts it, so the
// command exits non-zero without stopping.
func (e *Engine) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	e.failed.Add(1)
	// Counted where the transfers are counted, so the closing summary and the
	// exit status cannot disagree about whether anything went wrong.
	e.prog.Failed(1)
	e.recordFailure(Failure{Error: msg})
	if e.opt.Output == cli.OutputJSON {
		logx.Printf("%s\n", jsonLine(map[string]any{"event": "error", "error": msg}))
		return
	}
	e.log.Log(context.Background(), reportLevel(slog.LevelError), "copy problem", "detail", msg)
	logx.Errf("%s: %s\n", cli.Program, msg)
}
