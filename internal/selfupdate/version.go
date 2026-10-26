// SPDX-License-Identifier: MIT

package selfupdate

import (
	"strconv"
	"strings"
)

// Only stable release versions opt into replacement. In particular, a dirty
// git describe, a Go pseudo-version and a prerelease must keep their identity.
func releaseVersion(s string) ([3]uint64, bool) {
	var v [3]uint64
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != len(v) {
		return v, false
	}
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return v, false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return v, false
			}
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func newer(latest, current string) bool {
	a, aok := releaseVersion(latest)
	b, bok := releaseVersion(current)
	if !aok || !bok {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}
