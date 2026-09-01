package cli

import (
	"strings"
	"testing"

	"github.com/JohanLindvall/azcp/internal/store/azure"
	"github.com/JohanLindvall/azcp/internal/store/local"
)

// --help and the README both say a mismatch fails the transfer by default. The
// default has to agree with them, and once did not.
func TestCheckMD5DefaultsToFail(t *testing.T) {
	if got := mustParse(t, "a", "b").CheckMD5; got != azure.MD5Fail {
		t.Errorf("default --check-md5 = %v, want fail", got)
	}
	if got := mustParse(t, "--check-md5=off", "a", "b").CheckMD5; got != azure.MD5Off {
		t.Errorf("--check-md5=off = %v", got)
	}
}

func TestEnumeratedValues(t *testing.T) {
	if got := mustParse(t, "--reflink", "a", "b").Reflink; got != local.ReflinkAlways {
		t.Errorf("bare --reflink = %v, want always", got)
	}
	if got := mustParse(t, "--reflink=never", "a", "b").Reflink; got != local.ReflinkNever {
		t.Errorf("--reflink=never = %v", got)
	}
	if got := mustParse(t, "--sparse=always", "a", "b").Sparse; got != local.SparseAlways {
		t.Errorf("--sparse=always = %v", got)
	}
	if got := mustParse(t, "--glob=never", "a", "b").Glob; got != GlobNever {
		t.Errorf("--glob=never = %v", got)
	}
	if got := mustParse(t, "--output=JSON", "a", "b").Output; got != OutputJSON {
		t.Errorf("--output=JSON = %v; the value is case-insensitive", got)
	}
}

// A rejected value names the option and lists what it takes, in the order
// --help gives them, so the message can be acted on without opening the help.
func TestRejectedValuesNameTheOption(t *testing.T) {
	cases := map[string]string{
		"--reflink=maybe":         `invalid argument "maybe" for '--reflink' (want always, auto or never)`,
		"--sparse=x":              `invalid argument "x" for '--sparse' (want always, auto or never)`,
		"--update=x":              `invalid argument "x" for '--update' (want all, none, none-fail or older)`,
		"--output=xml":            `invalid argument "xml" for '--output' (want text or json)`,
		"--glob=x":                `invalid argument "x" for '--glob' (want auto, always or never)`,
		"--jobs=0":                `invalid argument "0" for '--jobs' (want a positive number)`,
		"-j0":                     `invalid argument "0" for '--jobs' (want a positive number)`,
		"--retries=-1":            `invalid argument "-1" for '--retries' (want a positive number)`,
		"--part-size=huge":        `invalid argument for '--part-size':`,
		"--bwlimit=fast":          `invalid argument for '--bwlimit':`,
		"--timeout=soon":          `invalid argument for '--timeout':`,
		"--retry-delay=later":     `invalid argument for '--retry-delay':`,
		"--progress-interval=1ms": `--progress-interval must be at least 20ms`,
	}
	for arg, want := range cases {
		_, err := Parse([]string{arg, "a", "b"})
		if err == nil {
			t.Errorf("%s was accepted", arg)
			continue
		}
		if !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: %q\n         want prefix %q", arg, err, want)
		}
	}
}

func TestOrList(t *testing.T) {
	cases := map[string][]string{
		"":          nil,
		"a":         {"a"},
		"a or b":    {"a", "b"},
		"a, b or c": {"a", "b", "c"},
	}
	for want, in := range cases {
		if got := orList(in); got != want {
			t.Errorf("orList(%v) = %q, want %q", in, got, want)
		}
	}
}
