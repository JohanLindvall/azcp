// SPDX-License-Identifier: MIT

// Package tui implements the opt-in two-pane file manager. Azure accounts,
// containers and blob prefixes appear as a normal directory hierarchy.
package tui

import (
	"net/url"
	"path/filepath"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

type location struct {
	url    *uri.URL
	parent *location
}

func (l location) display() string { return l.url.Display() }

type entry struct {
	name string
	loc  location
	node *store.Node
}

func (e entry) directory() bool {
	return e.node == nil || e.node.IsDir() || (e.node.IsSymlink() && e.node.Mode.IsDir())
}

func parseURL(raw, suffix string) (*uri.URL, error) {
	u, err := uri.Parse(raw, uri.Options{EndpointSuffix: suffix})
	if err != nil || u.IsRemote() {
		return u, err
	}
	p, err := filepath.Abs(u.Path)
	if err != nil {
		return nil, err
	}
	return u.WithPathPart(filepath.ToSlash(p)), nil
}

func parseLocation(raw, suffix string) (location, error) {
	u, err := parseURL(raw, suffix)
	return location{url: u}, err
}

func parentLocation(l location) location {
	if l.parent != nil {
		return *l.parent
	}
	if !l.url.IsRemote() {
		p := filepath.ToSlash(filepath.Dir(l.url.Path))
		return location{url: l.url.WithPathPart(p)}
	}
	return location{url: l.url.Dir()}
}

func rootLocation(l location) location {
	if l.url.IsRemote() {
		// Stay within a container-scoped SAS when returning to its root.
		return location{url: l.url.WithPathPart(l.url.Container)}
	}
	root := filepath.VolumeName(l.url.Path) + string(filepath.Separator)
	return location{url: l.url.WithPathPart(filepath.ToSlash(root))}
}

// uri.URL.String is deliberately display-only: it hides SAS tokens and does
// not escape blob names. Engine operands need the exact name and credential.
func operand(u *uri.URL, directory bool) string {
	if !u.IsRemote() {
		return u.Path
	}
	s := u.ServiceURL()
	if u.Secure && !u.PathStyle {
		s = "azure://" + u.Host
	}
	if u.Container != "" {
		s += "/" + url.PathEscape(u.Container)
	}
	if u.Key != "" {
		s += "/" + url.PathEscape(u.Key)
	}
	if directory {
		s += "/"
	}
	if u.SAS != "" {
		s += "?" + u.SAS
	}
	return s
}
