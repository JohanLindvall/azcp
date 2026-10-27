// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/engine"
	"github.com/JohanLindvall/azcp/internal/humanize"
	"github.com/JohanLindvall/azcp/internal/logx"
	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// The command line reaches the same selections as the file manager: --select
// is the pane filter applied to each operand directory, and --rm is the
// delete key. Both reuse what the panes do, so a filter means one thing and a
// deletion keeps the same guarantees wherever it was asked for.

// Select replaces each source directory in opt with the entries of it that
// opt.Select picks, written as literal operands for the engine.
func Select(ctx context.Context, opt *cli.Options, log *slog.Logger) error {
	f, err := parseSelect(opt.Select)
	if err != nil {
		return err
	}
	picked, err := newBackend(opt, log).selectEach(ctx, f, opt.Sources)
	if err != nil {
		return err
	}
	for _, t := range picked {
		if sharesContainerURL(t.entry) {
			return fmt.Errorf("cannot copy %s: %w", engine.Quote(t.shown), errSharedURL)
		}
	}
	opt.Sources = opt.Sources[:0]
	for _, t := range picked {
		opt.Sources = append(opt.Sources, operand(t.loc.url, t.node.IsDir()))
	}
	// Selected names are exact; a bracket in one is not a pattern.
	opt.Glob = cli.GlobNever
	return nil
}

func parseSelect(text string) (nameFilter, error) {
	f, err := parseFilter(text)
	if err != nil {
		return nameFilter{}, cli.Usage(fmt.Errorf("--select: %w", err))
	}
	return f, nil
}

// target is an entry with the name messages give it, which starts from the
// operand as it was typed, the way cp's own messages do.
type target struct {
	entry
	shown string
}

func shownBelow(raw string, base location, e entry) string {
	if !base.url.IsRemote() {
		return filepath.Join(raw, e.name)
	}
	if sharesContainerURL(e) {
		// Its URL is the container's; name it where the listing found it.
		u := *e.loc.url
		u.Key = "/"
		return u.Display()
	}
	return e.loc.display()
}

// below names path, at or below the local directory dir, starting from shown:
// the name dir has on the command line.
func below(shown, dir, path string) string {
	rel, err := filepath.Rel(dir, path)
	switch {
	case err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)):
		return path
	case rel == ".":
		return shown
	}
	return filepath.Join(shown, rel)
}

func (t target) nameOf(u *uri.URL) string {
	if u.IsRemote() {
		return u.Display()
	}
	return below(t.shown, t.loc.url.Path, u.Path)
}

// detail is reason for a failure somewhere below what a message names, so a
// filesystem error also says which path it was about.
func detail(err error, name func(string) string) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return engine.Quote(name(filepath.ToSlash(pe.Path))) + ": " + reason(err)
	}
	return reason(err)
}

// explain words err, which stopped t's removal, naming entries the way the
// operand named them. It returns the entry the message is about as well.
func (t target) explain(err error) (name, msg string) {
	var re *removeError
	var le *listError
	switch {
	case errors.As(err, &re):
		name = t.nameOf(re.node.URL)
		return name, fmt.Sprintf("cannot remove %s: %s", engine.Quote(name), reason(re.err))
	case errors.As(err, &le):
		local := func(p string) string { return below(t.shown, t.loc.url.Path, p) }
		return t.shown, fmt.Sprintf("cannot list %s in full: %s", engine.Quote(t.shown), detail(le.err, local))
	case errors.Is(err, errSelectionChanged):
		return t.shown, fmt.Sprintf("cannot remove %s: it changed while it was being listed", engine.Quote(t.shown))
	}
	return t.shown, fmt.Sprintf("cannot remove %s: %s", engine.Quote(t.shown), reason(err))
}

// phrased is a message about err that still answers errors.Is and errors.As.
type phrased struct {
	msg string
	err error
}

func (p *phrased) Error() string { return p.msg }
func (p *phrased) Unwrap() error { return p.err }

// selectEach lists every directory and keeps what the filter selects. Only an
// entry the filter can positively judge is selected: unlike a pane, which keeps
// an unmeasured directory on show, a batch acts on what it picks without
// anyone looking first.
func (b *backend) selectEach(ctx context.Context, f nameFilter, dirs []string) ([]target, error) {
	var picked []target
	for _, raw := range dirs {
		if raw == "" {
			// Made absolute, the empty name would be the working directory.
			return nil, errors.New("cannot stat '': No such file or directory")
		}
		base, err := parseLocation(raw, b.opt.EndpointSuffix)
		if err != nil {
			return nil, err
		}
		shown := engine.Quote(base.display())
		name := func(p string) string { return p }
		if !base.url.IsRemote() {
			shown = engine.Quote(raw)
			name = func(p string) string { return below(raw, base.url.Path, p) }
		}
		n, err := b.store(base.url).Stat(ctx, base.url, true)
		if err != nil {
			return nil, &phrased{fmt.Sprintf("cannot stat %s: %s", shown, reason(err)), err}
		}
		if !n.IsDir() {
			return nil, fmt.Errorf("cannot select from %s: Not a directory", shown)
		}
		entries, err := b.list(ctx, base)
		if err != nil {
			return nil, &phrased{fmt.Sprintf("cannot list %s: %s", shown, detail(err, name)), err}
		}
		if err := b.measureFor(ctx, f, base, entries); err != nil {
			return nil, &phrased{fmt.Sprintf("cannot measure %s: %s", shown, detail(err, name)), err}
		}
		for _, e := range entries {
			if f.selects(e) {
				picked = append(picked, target{e, shownBelow(raw, base, e)})
			}
		}
	}
	return picked, nil
}

// measureFor walks the directories among entries once, together, when the
// filter asks about sizes, ages or contents: the walk Z takes in a pane.
func (b *backend) measureFor(ctx context.Context, f nameFilter, base location, entries []entry) error {
	if !f.walks {
		return nil
	}
	var dirs []int
	for i, e := range entries {
		if e.node.IsDir() {
			dirs = append(dirs, i)
		}
	}
	if len(dirs) == 0 {
		return nil
	}
	selected := make([]entry, len(dirs))
	for i, row := range dirs {
		selected[i] = entries[row]
	}
	quiet := progress.New(progress.Config{Mode: progress.ModeNever})
	defer quiet.Stop()
	trees, err := b.directorySizes(ctx, base, selected, true, f.has, quiet)
	if err != nil {
		return err
	}
	for i, row := range dirs {
		entries[row].dirSize = trees[i]
	}
	return nil
}

// selects is match without the benefit of the doubt: a term that cannot judge
// an entry neither selects it nor fails to exclude it.
func (f nameFilter) selects(e entry) bool {
	holds := func(t filterTerm) bool {
		match, known := t(e, false)
		return match && known
	}
	if len(f.names) > 0 && !slices.ContainsFunc(f.names, holds) {
		return false
	}
	for _, t := range f.must {
		if !holds(t) {
			return false
		}
	}
	return !slices.ContainsFunc(f.excluded, func(t filterTerm) bool {
		match, known := t(e, false)
		return match || !known
	})
}

// Remove deletes the operands, or what opt.Select picks from them, and lists
// them instead unless opt.Confirmed. Every selection is listed in full before
// anything is removed, as the file manager's delete does, and the filter is
// weighed again on that listing, so what it judged is what goes. It returns
// how many entries failed.
func Remove(ctx context.Context, opt *cli.Options, log *slog.Logger) (int, error) {
	return newBackend(opt, log).removeBatch(ctx, opt)
}

func (b *backend) removeBatch(ctx context.Context, opt *cli.Options) (int, error) {
	r := &removal{b: b, opt: opt, dryRun: opt.DryRun || !opt.Confirmed, start: time.Now()}
	err := r.run(ctx)
	r.finish(ctx, err)
	return r.failed, err
}

// removal is one --rm run and its account of what it did.
type removal struct {
	b      *backend
	opt    *cli.Options
	filter *nameFilter
	dryRun bool
	start  time.Time

	removed, failed int
	containers      int // removed whole, their contents uncounted
	files, bytes    int64
	failures        []map[string]string
}

func (r *removal) run(ctx context.Context) error {
	targets, err := r.targets(ctx)
	if err != nil {
		return err
	}
	kept := targets[:0]
	for _, t := range targets {
		err := removable(t.entry)
		if err == nil && t.node.IsDir() && !r.opt.Recursive {
			err = errors.New("Is a directory")
		}
		if err != nil {
			r.fail(t.shown, fmt.Sprintf("cannot remove %s: %v", engine.Quote(t.shown), err), err)
			continue
		}
		kept = append(kept, t)
	}
	targets = kept
	trees := make([]inventoried, len(targets))
	for i, t := range targets {
		// A container picked for what it holds is listed too, so it is judged
		// on what the removal takes, and goes blob by blob: deleted whole, it
		// would take whatever was written to it after it was weighed.
		tree, err := r.b.inventoryOne(ctx, t.entry, r.filter != nil && r.filter.walks)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			_, msg := t.explain(err)
			return &phrased{msg, err}
		}
		trees[i] = tree
	}
	targets, trees = r.rejudge(targets, trees)
	skip := covered(trees)
	for i, tree := range trees {
		if !skip[i] {
			if err := r.one(ctx, targets[i], tree); err != nil {
				return err
			}
		}
	}
	return nil
}

// targets resolves the operands, or what the filter picks from them.
func (r *removal) targets(ctx context.Context) ([]target, error) {
	if r.opt.Select != "" {
		f, err := parseSelect(r.opt.Select)
		if err != nil {
			return nil, err
		}
		r.filter = &f
		return r.b.selectEach(ctx, f, r.opt.Sources)
	}
	var targets []target
	for _, raw := range r.opt.Sources {
		if raw == "" {
			return nil, errors.New("cannot remove '': No such file or directory")
		}
		if !uri.IsRemoteArg(raw) && throughDot(raw) {
			r.fail(raw, "refusing to remove '.' or '..' directory: skipping "+engine.Quote(raw), nil)
			continue
		}
		l, err := parseLocation(raw, r.opt.EndpointSuffix)
		if err != nil {
			return nil, err
		}
		shown := raw
		if l.url.IsRemote() {
			shown = l.display()
		}
		n, err := r.b.store(l.url).Stat(ctx, l.url, false)
		if err != nil {
			return nil, &phrased{fmt.Sprintf("cannot remove %s: %s", engine.Quote(shown), reason(err)), err}
		}
		targets = append(targets, target{entry{name: n.Name(), loc: l, node: n}, shown})
	}
	return targets, nil
}

// throughDot reports a local operand ending in . or .., or on Windows a bare
// drive such as C:, which is that drive's current directory. Each names a
// directory the path passes through rather than an entry in one, and rm
// refuses them all.
func throughDot(raw string) bool {
	if vol := filepath.VolumeName(raw); vol == raw && len(vol) == 2 {
		return true
	}
	base := filepath.Base(raw)
	return base == "." || base == ".."
}

// rejudge weighs the filter again on each inventory, which is what the
// removal will take. The selection was made on an earlier walk, and anything
// written or changed since then went unjudged.
func (r *removal) rejudge(targets []target, trees []inventoried) ([]target, []inventoried) {
	if r.filter == nil || !r.filter.walks {
		return targets, trees
	}
	kept := 0
	for i, t := range targets {
		e := t.entry
		e.node = trees[i].nodes[0]
		var err error
		if e.node.IsDir() {
			e.dirSize, err = measured(trees[i].nodes, r.filter.has)
		}
		if err != nil || !r.filter.selects(e) {
			if r.opt.Output != cli.OutputJSON {
				logx.Errf("%s: kept %s: it changed after it was selected and no longer matches\n",
					cli.Program, engine.Quote(t.shown))
			}
			continue
		}
		targets[kept], trees[kept] = t, trees[i]
		kept++
	}
	return targets[:kept], trees[:kept]
}

// measured totals an inventory the way the walk behind a pane's sizes does.
func measured(tree []*store.Node, terms []contentTerm) (*sizeTree, error) {
	t := &sizeTree{contains: map[string]bool{}}
	for _, n := range tree[1:] {
		if !n.IsRegular() {
			continue
		}
		if n.Size < 0 || n.Size > math.MaxInt64-t.bytes {
			return nil, errors.New("directory size exceeds the supported range")
		}
		t.note(n, leaf(n.URL), terms)
	}
	t.settle(terms)
	return t, nil
}

// leaf is the last component of u's name, which a has: term looks at.
func leaf(u *uri.URL) string {
	if u.IsRemote() {
		return u.Key[strings.LastIndexByte(u.Key, '/')+1:]
	}
	return filepath.Base(u.Path)
}

// covered marks the trees that another in the batch removes already: a later
// duplicate, one whose top another directory's inventory reached, and
// anything in a container, which goes whole. Removed on their own as well,
// they would fail on what the other had taken.
func covered(trees []inventoried) []bool {
	skip := make([]bool, len(trees))
	tops := make(map[string]int, len(trees))
	for i, t := range trees {
		key := nodeKey(t.nodes[0])
		if _, dup := tops[key]; dup {
			skip[i] = true
			continue
		}
		tops[key] = i
	}
	for i, t := range trees {
		top := t.nodes[0].URL
		if isContainer(top) {
			for j, other := range trees {
				u := other.nodes[0].URL
				if j != i && !isContainer(u) && top.SameAccount(u) && u.Container == top.Container {
					skip[j] = true
				}
			}
			continue
		}
		for _, n := range t.nodes[1:] {
			if j, ok := tops[nodeKey(n)]; ok && j != i {
				skip[j] = true
			}
		}
	}
	return skip
}

// one removes a single selection, or in a dry run measures it.
func (r *removal) one(ctx context.Context, t target, tree inventoried) error {
	var files, bytes int64
	count := func(n *store.Node) error {
		if n.IsRegular() {
			files++
			bytes += max(n.Size, 0)
		}
		return ctx.Err()
	}
	for _, n := range tree.nodes {
		_ = count(n)
	}
	top := tree.nodes[0]
	whole := isContainer(top.URL) && !tree.listed
	if r.dryRun && whole {
		// The inventory leaves a container unlisted because one request
		// deletes it; a listing of what would go needs its contents.
		err := r.b.remote.WalkAll(ctx, top.URL, func(_ *uri.URL, err error) error { return err }, count)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &phrased{fmt.Sprintf("cannot list %s: %s", engine.Quote(t.shown), reason(err)), err}
		}
	}
	if !r.dryRun {
		doomed := tree
		if r.opt.WholeContainers {
			// Asked for: one request, though it takes what arrived since.
			doomed.listed = false
		}
		if err := r.b.remove(ctx, []inventoried{doomed}); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			name, msg := t.explain(err)
			r.fail(name, msg, err)
			return nil
		}
		if whole {
			r.containers++
		}
	}
	r.removed++
	r.files += files
	r.bytes += bytes
	if r.dryRun || r.opt.Verbose || r.opt.Output == cli.OutputJSON {
		report(r.opt, r.dryRun, t, files, bytes)
	}
	return nil
}

// fail reports an entry that is not removed, as rm would, or as an event.
func (r *removal) fail(name, msg string, err error) {
	r.failed++
	if err != nil {
		// The SDK's own text is a dump of the whole exchange: worth keeping,
		// but only for someone who asked for detail.
		r.b.log.Debug("removal failure detail", "path", name, "error", err)
	}
	if r.opt.Output == cli.OutputJSON {
		r.failures = append(r.failures, map[string]string{"path": name, "error": msg})
		logx.Printf("%s\n", jsonLine(map[string]any{"event": "error", "path": name, "error": msg}))
		return
	}
	logx.Errf("%s: %s\n", cli.Program, msg)
}

// finish closes the run's account: the totals on stderr, or in JSON an event
// for a failure that stopped the run and then the summary. A command-line
// problem is the caller's to report.
func (r *removal) finish(ctx context.Context, err error) {
	var usage *cli.UsageError
	if errors.As(err, &usage) {
		return
	}
	interrupted := ctx.Err() != nil
	if r.opt.Output == cli.OutputJSON {
		failed := r.failed
		if err != nil && !interrupted {
			failed++
			r.failures = append(r.failures, map[string]string{"error": err.Error()})
			logx.Printf("%s\n", jsonLine(map[string]any{"event": "error", "error": err.Error()}))
		}
		warns, errs := logx.Counts()
		summary := map[string]any{
			"event":           "summary",
			"version":         cli.VersionString(),
			"removed":         r.removed,
			"containers":      r.containers,
			"failed":          failed,
			"files":           r.files,
			"bytes":           r.bytes,
			"elapsed_seconds": time.Since(r.start).Seconds(),
			"dry_run":         r.dryRun,
			"interrupted":     interrupted,
			"warnings":        warns,
			"errors":          errs,
		}
		if len(r.failures) > 0 {
			summary["failures"] = r.failures
		}
		logx.Printf("%s\n", jsonLine(summary))
		return
	}
	if err != nil {
		return
	}
	verb := "Removed"
	if r.dryRun {
		verb = "Would remove"
	}
	totals := fmt.Sprintf(": %s in %s %s", humanize.Bytes(r.bytes), humanize.Count(r.files), humanize.Plural(r.files, "file", "files"))
	if r.containers > 0 {
		totals = "" // A container went in one request, its contents uncounted.
	}
	logx.Errf("  %s %d %s%s\n", verb, r.removed, humanize.Plural(int64(r.removed), "entry", "entries"), totals)
	if r.dryRun && !r.opt.DryRun && r.removed > 0 {
		logx.Errf("  Nothing was removed; add --yes to remove them\n")
	}
}

func report(opt *cli.Options, dryRun bool, e target, files, bytes int64) {
	if opt.Output == cli.OutputJSON {
		kind := "removed"
		if dryRun {
			kind = "would-remove"
		}
		logx.Printf("%s\n", jsonLine(map[string]any{"event": kind, "path": e.shown,
			"directory": e.node.IsDir(), "files": files, "bytes": bytes}))
		return
	}
	name := engine.Quote(e.shown)
	switch {
	case dryRun && e.node.IsDir():
		logx.Printf("would remove %s (%s in %s %s)\n", name, humanize.Bytes(bytes),
			humanize.Count(files), humanize.Plural(files, "file", "files"))
	case dryRun:
		logx.Printf("would remove %s (%s)\n", name, humanize.Bytes(bytes))
	case isContainer(e.loc.url):
		logx.Printf("removed container %s\n", name)
	case e.node.IsDir():
		logx.Printf("removed directory %s\n", name)
	default:
		logx.Printf("removed %s\n", name)
	}
}

func jsonLine(v any) string {
	line, err := json.Marshal(v)
	if err != nil {
		return `{"event":"error","error":"could not encode this record"}`
	}
	return string(line)
}
