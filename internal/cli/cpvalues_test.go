// SPDX-License-Identifier: MIT

package cli

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/JohanLindvall/azcp/internal/store/local"
)

func TestGNUValueDiagnostics(t *testing.T) {
	version, err := exec.Command("cp", "--version").Output()
	if err != nil || !strings.Contains(string(version), "(GNU coreutils) 9.4\n") {
		t.Skip("GNU cp 9.4 is not available")
	}
	for _, flag := range []string{"--reflink=", "--reflink=a", "--reflink=bad",
		"--sparse=", "--sparse=a", "--update=", "--update=bad",
		"--preserve=", "--preserve=mode,", "--preserve= mode", "--no-preserve=",
		"--backup=n", "--backup=bad"} {
		t.Run(flag, func(t *testing.T) {
			_, err := Parse([]string{flag, "a", "b"})
			if err == nil {
				t.Fatal("invalid value was accepted")
			}
			cmd := exec.Command("cp", flag, "a", "b")
			cmd.Env = append(os.Environ(), "LC_ALL=C")
			out, cpErr := cmd.CombinedOutput()
			if cpErr == nil {
				t.Fatal("GNU cp accepted the test's invalid value")
			}
			if !strings.HasPrefix(string(out), "cp: "+err.Error()+"\n") {
				t.Fatalf("diagnostic %q disagrees with GNU cp: %s", err, out)
			}
		})
	}
}

func TestRequestBudgetsCannotOverflow(t *testing.T) {
	for _, arg := range []string{"--part-concurrency=65536", "--jobs=" + strconv.Itoa(math.MaxInt), "--retries=2147483648"} {
		if _, err := Parse([]string{arg, "a", "b"}); err == nil {
			t.Errorf("accepted %s", arg)
		}
	}
}

func TestGNUValuePrefixesAndEmptyBackup(t *testing.T) {
	t.Setenv("VERSION_CONTROL", "numbered")
	o := mustParse(t, "--reflink=ne", "--sparse=al", "--update=o", "--preserve=mod,time", "--backup=", "a", "b")
	if o.Reflink != local.ReflinkNever || o.Sparse != local.SparseAlways ||
		o.Update != UpdateOlder || !o.Preserve.Mode || !o.Preserve.Timestamps || o.Backup != BackupNumbered {
		t.Fatalf("prefixes resolved incorrectly: %+v", o)
	}
	if got := mustParse(t, "--update=n", "a", "b").Update; got != UpdateNone {
		t.Fatal("the none-fail extension changed GNU's unambiguous 'n'")
	}
}

func TestInvalidCopyOptionsFailBeforeOpeningFiles(t *testing.T) {
	for _, flags := range [][]string{
		{"--reflink=always", "--sparse=never"},
		{"--reflink", "--sparse=always"},
		{"-t", ".", "-t", "."},
	} {
		if _, err := Parse(append(flags, "a", "b")); err == nil {
			t.Fatalf("accepted %v", flags)
		}
	}
}

func TestHelpStopsParsingInOrder(t *testing.T) {
	if !mustParse(t, "--help", "--bad").ShowHelp || !mustParse(t, "--version", "--bad").ShowVersion {
		t.Fatal("terminating option did not stop parsing")
	}
	for _, argv := range [][]string{{"--bad", "--help"}, {"--preserve=nope", "--help"}} {
		if _, err := Parse(argv); err == nil {
			t.Fatalf("help hid an earlier error: %v", argv)
		}
	}
}

func TestBackupSuffixCannotBeAPathOrTheOriginalName(t *testing.T) {
	for _, suffix := range []string{"", "nested/backup", string(filepath.Separator)} {
		if got := mustParse(t, "-b", "--suffix="+suffix, "a", "b").Suffix; got != "~" {
			t.Fatalf("suffix %q became %q", suffix, got)
		}
	}
	if got := mustParse(t, "--backup=none", "-S", ".bak", "a", "b").Backup; got != BackupNone {
		t.Fatal("a suffix overrode explicit --backup=none")
	}
}
