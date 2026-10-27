// SPDX-License-Identifier: MIT

package tui

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/glob"
	"github.com/JohanLindvall/azcp/internal/humanize"
	"github.com/JohanLindvall/azcp/internal/store"
)

// filterTerm answers for one entry, or says it cannot yet: a directory's size,
// contents and age are unknown until something has walked it.
type filterTerm func(e entry, allocated bool) (match, known bool)

// filterHelp is shown beneath the prompt; the language is meant to be learnt
// from it rather than from the manual.
var filterHelp = []string{
	"Names: report  *.mp4  IMG_*",
	"size>50M  files>=100  type:dir",
	"newer:7d  older:2024-01-01",
	"has:.mp4 (holds a matching file)",
	"Any name may match; all else must",
	`-term excludes  "quote spaces"`,
}

var filterFields = []string{"name", "has", "size", "files", "newer", "older", "type"}

// nameFilter shows an entry when it matches any name term, or there are none,
// meets every condition, and matches no excluded term. An entry a term cannot
// judge is kept rather than hidden: an unmeasured directory may be any size,
// hold anything and have changed at any time.
type nameFilter struct {
	text                  string
	names, must, excluded []filterTerm
	has                   []contentTerm
	walks                 bool // something asked depends on a directory's contents
}

func parseFilter(text string) (nameFilter, error) {
	f := nameFilter{text: strings.TrimSpace(text)}
	terms, err := splitTerms(f.text)
	if err != nil {
		return nameFilter{}, err
	}
	for _, term := range terms {
		rest, excluded := strings.CutPrefix(term, "-")
		if !excluded || rest == "" {
			rest, excluded = term, false
		}
		t, isName, err := f.term(rest)
		if err != nil {
			return nameFilter{}, fmt.Errorf("filter %q: %w", term, err)
		}
		switch {
		case excluded:
			f.excluded = append(f.excluded, t)
		case isName:
			f.names = append(f.names, t)
		default:
			f.must = append(f.must, t)
		}
	}
	return f, nil
}

// splitTerms splits at spaces outside double quotes.
func splitTerms(s string) ([]string, error) {
	var terms []string
	var b strings.Builder
	quoted, started := false, false
	for _, r := range s {
		switch {
		case r == '"':
			quoted, started = !quoted, true
		case !quoted && unicode.IsSpace(r):
			if started {
				terms = append(terms, b.String())
			}
			b.Reset()
			started = false
		default:
			b.WriteRune(r)
			started = true
		}
	}
	if quoted {
		return nil, errors.New("unterminated quote in filter")
	}
	if started {
		terms = append(terms, b.String())
	}
	return terms, nil
}

// term reads one condition; isName says it is a name, one of the alternatives.
func (f *nameFilter) term(s string) (t filterTerm, isName bool, err error) {
	field, op, value := splitField(s)
	if field == "" && op == "" {
		m, err := termMatcher(s)
		return func(e entry, _ bool) (bool, bool) { return m(e.name), true }, true, err
	}
	if field == "" {
		field = "size" // >50M is short for size>50M
	}
	if !slices.Contains(filterFields, field) {
		return nil, false, fmt.Errorf("unknown field %q; use %s", field, strings.Join(filterFields, ", "))
	}
	switch field {
	case "name", "has", "type", "newer", "older":
		if op != ":" && op != "=" {
			return nil, false, fmt.Errorf("write %s:VALUE", field)
		}
	}
	switch field {
	case "name":
		m, err := termMatcher(value)
		return func(e entry, _ bool) (bool, bool) { return m(e.name), true }, true, err
	case "has":
		c, err := f.content(value)
		f.walks = true
		return containsTerm(c), false, err
	case "type":
		t, err := typeTerm(value)
		return t, false, err
	case "newer", "older":
		when, err := cli.ParseTimeSpec(value, time.Now())
		f.walks = true
		older := field == "older"
		return func(e entry, _ bool) (bool, bool) {
			n := e.newest()
			return !n.IsZero() && n.Before(when) == older, !n.IsZero()
		}, false, err
	}
	f.walks = true
	if op == ":" {
		op = "="
	}
	limit, err := parseCount(field, value)
	if err != nil {
		return nil, false, err
	}
	measure := func(e entry, allocated bool) (int64, bool) { return e.usageSize(allocated) }
	if field == "files" {
		measure = func(e entry, _ bool) (int64, bool) { return e.fileCount() }
	}
	cmp := comparisons[op]
	return func(e entry, allocated bool) (bool, bool) {
		n, known := measure(e, allocated)
		return known && cmp(n, limit), known
	}, false, nil
}

var comparisons = map[string]func(n, limit int64) bool{
	">":  func(n, limit int64) bool { return n > limit },
	">=": func(n, limit int64) bool { return n >= limit },
	"<":  func(n, limit int64) bool { return n < limit },
	"<=": func(n, limit int64) bool { return n <= limit },
	"=":  func(n, limit int64) bool { return n == limit },
}

// splitField finds FIELD OP VALUE, with OP one of : = < <= > >=. A term that
// is not one is a name and gives back no field or operator.
func splitField(s string) (field, op, value string) {
	i := 0
	for i < len(s) && (s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z') {
		i++
	}
	for _, o := range []string{">=", "<=", ":", "=", "<", ">"} {
		if strings.HasPrefix(s[i:], o) {
			field, op, value = strings.ToLower(s[:i]), o, s[i+len(o):]
			if field == "" && o == ":" {
				return "", "", s
			}
			if o == ":" {
				// size:>50M reads as size>50M.
				for _, inner := range []string{">=", "<=", "=", "<", ">"} {
					if rest, ok := strings.CutPrefix(value, inner); ok {
						return field, inner, rest
					}
				}
			}
			return field, op, value
		}
	}
	return "", "", s
}

func parseCount(field, value string) (int64, error) {
	if field == "files" {
		return strconv.ParseInt(value, 10, 64)
	}
	return humanize.ParseSize(value)
}

func typeTerm(value string) (filterTerm, error) {
	var want func(entry) bool
	switch strings.ToLower(value) {
	case "d", "dir", "directory":
		want = entry.directory
	case "f", "file":
		want = func(e entry) bool { return !e.directory() && e.kind() != store.KindSymlink }
	case "l", "link", "symlink":
		want = func(e entry) bool { return e.kind() == store.KindSymlink }
	default:
		return nil, errors.New("type is dir, file or link")
	}
	return func(e entry, _ bool) (bool, bool) { return want(e), true }, nil
}

func (f *nameFilter) content(pattern string) (contentTerm, error) {
	for _, c := range f.has {
		if c.pattern == pattern {
			return c, nil
		}
	}
	m, err := termMatcher(pattern)
	if err != nil {
		return contentTerm{}, err
	}
	c := contentTerm{pattern: pattern, match: m}
	f.has = append(f.has, c)
	return c, nil
}

func containsTerm(c contentTerm) filterTerm {
	return func(e entry, _ bool) (bool, bool) {
		switch {
		case e.usage != nil && e.usage.kind == store.KindDir:
			// The usage view holds the whole tree, so it can simply look.
			stack := slices.Clone(e.usage.children)
			for len(stack) > 0 {
				n := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if n.kind == store.KindDir {
					stack = append(stack, n.children...)
				} else if c.match(n.name) {
					return true, true
				}
			}
			return false, true
		case !e.directory():
			return c.match(e.name), true
		case e.dirSize != nil:
			found, known := e.dirSize.contains[c.pattern]
			return found, known
		}
		return false, false
	}
}

func termMatcher(term string) (func(string) bool, error) {
	if term == "" {
		return nil, errors.New("nothing to match")
	}
	if !glob.HasMeta(term) && !strings.Contains(term, "/") {
		term = strings.ToLower(term)
		return func(name string) bool { return strings.Contains(strings.ToLower(name), term) }, nil
	}
	expanded, err := glob.ExpandBraces(term)
	if err != nil {
		return nil, err
	}
	var segs []glob.Segment
	for _, alt := range expanded {
		p, err := glob.Compile(alt)
		if err != nil {
			return nil, err
		}
		if len(p.Segs) != 1 || p.Abs || p.TrailingSlash || p.Segs[0].Kind == glob.SegDoubleStar {
			return nil, errors.New("patterns match one name, not a path; has:PATTERN looks inside directories")
		}
		segs = append(segs, p.Segs[0])
	}
	return func(name string) bool {
		return slices.ContainsFunc(segs, func(s glob.Segment) bool { return s.Match(name) })
	}, nil
}

func (f nameFilter) active() bool { return f.text != "" }

// unmeasured says whether a walk of directory e would tell the filter more.
func (f nameFilter) unmeasured(e entry) bool {
	if e.node == nil || !e.node.IsDir() || !f.walks {
		return false
	}
	if e.dirSize == nil {
		return true
	}
	return slices.ContainsFunc(f.has, func(c contentTerm) bool {
		_, known := e.dirSize.contains[c.pattern]
		return !known
	})
}

// match weighs sizes as the pane shows them: allocated or logical in the usage
// view.
func (f nameFilter) match(e entry, allocated bool) bool {
	passes := func(t filterTerm) bool {
		match, known := t(e, allocated)
		return match || !known
	}
	if len(f.names) > 0 && !slices.ContainsFunc(f.names, passes) {
		return false
	}
	for _, t := range f.must {
		if !passes(t) {
			return false
		}
	}
	return !slices.ContainsFunc(f.excluded, func(t filterTerm) bool {
		match, known := t(e, allocated)
		return match && known
	})
}

// refilter applies the pane's filter to everything listed. Hidden entries stay
// with the pane, so changing the filter costs no listing, and lose their
// marks, so nothing out of sight can be copied, moved or deleted.
func (p *pane) refilter() {
	if len(p.entries) == 0 {
		return
	}
	focus := p.cursor - 1
	all := append(slices.Clone(p.entries[1:]), p.hidden...)
	marked := p.selected
	p.entries, p.hidden = p.entries[:1], nil
	p.selected, p.cursor, p.offset = map[int]bool{}, 0, 0
	for i, e := range all {
		if !p.listFilter.match(e, false) {
			p.hidden = append(p.hidden, e)
			continue
		}
		p.entries = append(p.entries, e)
		if marked[i+1] {
			p.selected[len(p.entries)-1] = true
		}
		if i == focus {
			p.cursor = len(p.entries) - 1
		}
	}
	p.sortEntries()
	if p.cursor == 0 && focus >= 0 {
		p.cursor = min(1, len(p.entries)-1)
	}
}

func (a *app) applyFilter(text string) {
	f, err := parseFilter(text)
	if err != nil {
		a.report(err)
		return
	}
	a.modal = nil
	p := &a.panes[a.active]
	if p.usage != nil {
		p.filter = f
		p.usageEntries(nil)
		a.status = "Filter applied; selection marks cleared"
		return
	}
	p.listFilter = f
	p.refilter()
	a.status = "Filter cleared"
	if f.active() {
		a.status = "Filter applied to this pane until cleared"
	}
	if slices.ContainsFunc(p.entries, f.unmeasured) {
		// Asking for sizes or contents is asking for the walk that finds them.
		a.measure(true, f.unmeasured)
	}
}
