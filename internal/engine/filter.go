package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/JohanLindvall/azcp/internal/glob"
)

// Selecting which files in a tree to copy is where the obvious competitor is
// weakest: it offers four overlapping flags whose patterns match the file name
// only, whose path variants take no wildcards, and whose behaviour has been the
// subject of long-running confusion. azcp already has a complete shell-style
// matcher, so filtering is the same language as everything else here:
//
//	--exclude '*.tmp'          any file so named, at any depth
//	--exclude 'build/**'       a whole subtree, pruned rather than walked
//	--include '**/*.parquet'   only these, wherever they are
//	--exclude '!(*.gz)'        extended patterns work too
//
// A pattern containing no "/" is matched against the name alone, which is what
// people mean by "*.tmp"; one containing a "/" is matched against the path
// relative to the copy root, anchored at that root.

// filter decides which entries of a tree take part in the copy.
type filter struct {
	// newerThan and olderThan bound the modification time. They are a
	// watermark for pipelines that track their own high-water mark rather
	// than comparing against the destination.
	newerThan time.Time
	olderThan time.Time

	includes []pattern
	excludes []pattern
}

// pattern is one compiled --include or --exclude entry.
type pattern struct {
	glob *glob.Pattern
	// nameOnly records that the pattern has no separator and so applies to
	// the base name, at any depth.
	nameOnly bool
}

// newFilter compiles the --include and --exclude patterns. Brace expansion is
// applied first, so --exclude '*.{tmp,bak}' means what it looks like.
func newFilter(includes, excludes []string, newerThan, olderThan time.Time) (*filter, error) {
	f := &filter{newerThan: newerThan, olderThan: olderThan}
	var err error
	if f.includes, err = compilePatterns(includes, "--include"); err != nil {
		return nil, err
	}
	if f.excludes, err = compilePatterns(excludes, "--exclude"); err != nil {
		return nil, err
	}
	return f, nil
}

func compilePatterns(raw []string, flag string) ([]pattern, error) {
	var out []pattern
	for _, r := range raw {
		for _, expanded := range glob.ExpandBraces(r) {
			p, err := glob.Compile(expanded)
			if err != nil {
				return nil, fmt.Errorf("bad pattern %q for %s: %w", expanded, flag, err)
			}
			out = append(out, pattern{glob: p, nameOnly: !strings.Contains(expanded, "/")})
		}
	}
	return out, nil
}

// active reports whether any filtering was asked for at all, so the common case
// costs nothing.
func (f *filter) active() bool {
	return f != nil && (len(f.includes) > 0 || len(f.excludes) > 0 ||
		!f.newerThan.IsZero() || !f.olderThan.IsZero())
}

// withinWindow applies --newer-than and --older-than. A node with no usable
// timestamp is kept: dropping data because its date could not be read would be
// the wrong way round.
func (f *filter) withinWindow(mtime time.Time) bool {
	if f == nil || mtime.IsZero() {
		return true
	}
	if !f.newerThan.IsZero() && !mtime.After(f.newerThan) {
		return false
	}
	if !f.olderThan.IsZero() && !mtime.Before(f.olderThan) {
		return false
	}
	return true
}

// excluded reports whether an entry is ruled out by --exclude. A directory that
// is excluded is not descended into, which is the point: pruning a subtree beats
// listing it and discarding the results.
func (f *filter) excluded(rel string) bool {
	if f == nil {
		return false
	}
	return matchAny(f.excludes, rel)
}

// included reports whether an entry survives --include. With no --include given
// everything is included; that is the difference between "no filter" and "an
// empty filter".
func (f *filter) included(rel string) bool {
	if f == nil || len(f.includes) == 0 {
		return true
	}
	return matchAny(f.includes, rel)
}

// allow is the whole decision for a file: excluded wins over included, as it
// does in every tool that offers both.
func (f *filter) allow(rel string) bool {
	if f == nil {
		return true
	}
	return !f.excluded(rel) && f.included(rel)
}

// descend reports whether a directory is worth walking into. An --include list
// cannot rule a directory out, because the files it is looking for may be
// inside; only --exclude prunes.
func (f *filter) descend(rel string) bool {
	return !f.excluded(rel)
}

func matchAny(pats []pattern, rel string) bool {
	if rel == "" {
		return false
	}
	base := rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		base = rel[i+1:]
	}
	for _, p := range pats {
		subject := rel
		if p.nameOnly {
			subject = base
		}
		if p.glob.Match(subject) {
			return true
		}
	}
	return false
}
