// SPDX-License-Identifier: MIT

// Package tui implements the opt-in two-pane file manager. Its virtual
// containers are collections of named locations, not another storage backend.
package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// A virtual container can contain further virtual containers and real mounts
// at any level. Only leaves have URLs; branches never name stored data.
type virtualNode struct {
	url      *uri.URL
	children collections
}

type collections map[string]*virtualNode

// address keeps the virtual breadcrumb while url identifies the real object.
// A nil url is a collection, which has no data of its own to copy or remove.
type location struct {
	url     *uri.URL
	address string
	parent  *location
}

func (l location) display() string {
	if l.address != "" {
		return l.address
	}
	return l.url.Display()
}

type entry struct {
	name      string
	loc       location
	node      *store.Node
	reference bool
}

func (e entry) directory() bool {
	return e.node == nil || e.node.IsDir() || (e.node.IsSymlink() && e.node.Mode.IsDir())
}

func loadCollections(filename, suffix string) (collections, error) {
	explicit := filename != ""
	if !explicit {
		dir, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		filename = filepath.Join(dir, "azcp", "tui.json")
	}
	f, err := os.Open(filename)
	if !explicit && errors.Is(err, os.ErrNotExist) {
		return collections{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read TUI configuration: %w", err)
	}
	defer f.Close()
	var cfg struct {
		Containers map[string]json.RawMessage `json:"containers"`
	}
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("read TUI configuration: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("TUI configuration must contain one JSON object")
	}
	return decodeContainers(cfg.Containers, filepath.Dir(filename), suffix, 0)
}

func decodeContainers(values map[string]json.RawMessage, dir, suffix string, depth int) (collections, error) {
	if depth > 32 {
		return nil, errors.New("virtual containers exceed 32 levels of nesting")
	}
	result := collections{}
	for name, value := range values {
		if !validName(name) {
			return nil, fmt.Errorf("invalid virtual container or mount name %q", name)
		}
		var children map[string]json.RawMessage
		if err := json.Unmarshal(value, &children); err == nil && children != nil {
			c, err := decodeContainers(children, dir, suffix, depth+1)
			if err != nil {
				return nil, err
			}
			result[name] = &virtualNode{children: c}
			continue
		}
		var raw string
		if depth == 0 || json.Unmarshal(value, &raw) != nil || raw == "" || strings.HasPrefix(raw, "virtual://") {
			return nil, fmt.Errorf("%q must be a container object or a local/Azure folder string", name)
		}
		if !uri.IsRemoteArg(raw) && !filepath.IsAbs(raw) {
			raw = filepath.Join(dir, raw)
		}
		u, err := parseURL(raw, suffix)
		if err != nil {
			return nil, fmt.Errorf("mount %q: %w", name, err)
		}
		result[name] = &virtualNode{url: u}
	}
	return result, nil
}

func validName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
			return false
		}
	}
	return true
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

func (c collections) resolve(raw, suffix string) (location, error) {
	if !strings.HasPrefix(raw, "virtual://") {
		u, err := parseURL(raw, suffix)
		return location{url: u}, err
	}
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return location{}, errors.New("invalid virtual location")
	}
	if u.Host == "" {
		if u.Path != "" && u.Path != "/" {
			return location{}, errors.New("virtual location needs a container name")
		}
		return location{address: "virtual://"}, nil
	}
	parts := []string{u.Host}
	if p := strings.TrimSuffix(strings.TrimPrefix(u.EscapedPath(), "/"), "/"); p != "" {
		parts = append(parts, strings.Split(p, "/")...)
	}
	address := "virtual://"
	var base *uri.URL
	for i, part := range parts {
		name, err := url.PathUnescape(part)
		if err != nil || (name == "." || name == "..") && (base == nil || !base.IsRemote()) {
			return location{}, errors.New("invalid path inside virtual container")
		}
		if i > 0 {
			address += "/"
		}
		address += part
		if base != nil {
			if !base.IsRemote() && strings.ContainsAny(name, "/\\") {
				return location{}, errors.New("invalid path inside local mount")
			}
			base = base.Join(name)
			continue
		}
		n, ok := c[name]
		if !ok {
			return location{}, fmt.Errorf("unknown virtual container or mount %q", name)
		}
		base, c = n.url, n.children
	}
	return location{url: base, address: address}, nil
}

func (c collections) parent(l location, suffix string) location {
	if l.parent != nil {
		return *l.parent
	}
	if l.address != "" {
		rest := strings.TrimPrefix(l.address, "virtual://")
		if i := strings.LastIndexByte(rest, '/'); i >= 0 {
			p, _ := c.resolve("virtual://"+rest[:i], suffix)
			return p
		}
		return location{address: "virtual://"}
	}
	if !l.url.IsRemote() {
		p := filepath.ToSlash(filepath.Dir(l.url.Path))
		return location{url: l.url.WithPathPart(p)}
	}
	return location{url: l.url.Dir()}
}

func (c collections) list(l location) []entry {
	var entries []entry
	if path := strings.TrimPrefix(l.address, "virtual://"); path != "" {
		for _, part := range strings.Split(path, "/") {
			name, _ := url.PathUnescape(part)
			n := c[name]
			if n == nil {
				return nil
			}
			c = n.children
		}
	}
	for name, n := range c {
		address := l.address
		if address != "virtual://" {
			address += "/"
		}
		e := entry{name: name, reference: true,
			loc: location{url: n.url, address: address + url.PathEscape(name)}}
		if n.url != nil {
			e.node = &store.Node{URL: n.url, Kind: store.KindDir}
		}
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.name, b.name) })
	return entries
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
