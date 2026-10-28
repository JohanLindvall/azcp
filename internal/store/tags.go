// SPDX-License-Identifier: MIT

package store

import (
	"errors"
	"fmt"
	"strings"
)

// ValidateBlobTags checks Azure's limits before a write can change a blob.
func ValidateBlobTags(tags map[string]string) error {
	if len(tags) > 10 {
		return errors.New("a blob can have at most 10 index tags")
	}
	valid := func(s string) bool {
		for _, c := range s {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune(" +-./:=_", c)) {
				return false
			}
		}
		return true
	}
	for k, v := range tags {
		if len(k) < 1 || len(k) > 128 || !valid(k) {
			return fmt.Errorf("invalid blob index tag key %q: use 1–128 letters, digits, spaces or + - . / : = _", k)
		}
		if len(v) > 256 || !valid(v) {
			return fmt.Errorf("invalid value for blob index tag %q: use 0–256 letters, digits, spaces or + - . / : = _", k)
		}
	}
	return nil
}
