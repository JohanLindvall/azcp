package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/JohanLindvall/azcp/internal/cpflags"
)

var preserveModes = []choice[string]{
	{"mode", "mode"}, {"timestamps", "timestamps"}, {"ownership", "ownership"},
	{"links", "links"}, {"context", "context"}, {"xattr", "xattr"}, {"all", "all"},
}

var backupModes = []choice[Backup]{
	{"none", BackupNone}, {"off", BackupNone},
	{"simple", BackupSimple}, {"never", BackupSimple},
	{"existing", BackupExisting}, {"nil", BackupExisting},
	{"numbered", BackupNumbered}, {"t", BackupNumbered},
}

// GNU's argmatch accepts unambiguous value prefixes, including multiple
// spellings for the same value, and prints the alternatives grouped by value.
func chooseCP[T comparable](flag, value string, choices []choice[T]) (T, error) {
	var matched T
	matches := map[T]bool{}
	for _, c := range choices {
		if c.name == value {
			return c.value, nil
		}
		if strings.HasPrefix(c.name, value) {
			matched = c.value
			matches[c.value] = true
		}
	}
	if len(matches) == 1 {
		return matched, nil
	}
	kind := "invalid"
	if len(matches) > 1 {
		kind = "ambiguous"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s argument '%s' for '%s'\nValid arguments are:", kind, value, flag)
	for i, c := range choices {
		if i > 0 && choices[i-1].value == c.value {
			fmt.Fprintf(&b, ", '%s'", c.name)
		} else {
			fmt.Fprintf(&b, "\n  - '%s'", c.name)
		}
	}
	var zero T
	return zero, fmt.Errorf("%s", b.String())
}

func setCPChoice[T comparable](dst *T, f cpflags.Flag, value string, choices []choice[T]) error {
	got, err := chooseCP(f.Name(), value, choices)
	if err == nil {
		*dst = got
	}
	return err
}

// GNU cp falls back to '~' for an empty suffix or one containing a path
// separator. In particular an empty suffix must never rename a file to itself.
func validBackupSuffix(s string) string {
	if s == "" || strings.ContainsFunc(s, func(r rune) bool { return r < 128 && os.IsPathSeparator(uint8(r)) }) {
		return "~"
	}
	return s
}
