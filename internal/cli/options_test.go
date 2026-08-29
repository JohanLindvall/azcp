package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/store/local"
)

// mustParse parses a full command line and fails the test on any error.
func mustParse(t *testing.T, argv ...string) *Options {
	t.Helper()
	o, err := Parse(argv)
	if err != nil {
		t.Fatalf("Parse(%v): %v", argv, err)
	}
	return o
}

// The short forms -b and -u take no argument, exactly as in GNU cp, so they
// cluster with whatever follows instead of swallowing it.
func TestShortBackupAndUpdateCluster(t *testing.T) {
	o := mustParse(t, "-bt", "dir", "src")
	if o.Backup == BackupNone {
		t.Error("-b did not enable backups")
	}
	if !o.HasTargetDir || o.Dest != "dir" {
		t.Errorf("-bt did not leave t its directory: dest %q", o.Dest)
	}

	o = mustParse(t, "-uv", "a", "b")
	if o.Update != UpdateOlder {
		t.Error("-u did not select --update=older")
	}
	if !o.Verbose {
		t.Error("-v was swallowed by -u")
	}
}

func TestBackupHonoursVersionControl(t *testing.T) {
	t.Setenv("VERSION_CONTROL", "numbered")
	o := mustParse(t, "-b", "a", "b")
	if o.Backup != BackupNumbered {
		t.Errorf("VERSION_CONTROL=numbered: got %v", o.Backup)
	}
	t.Setenv("VERSION_CONTROL", "garbage")
	if _, err := Parse([]string{"-b", "a", "b"}); err == nil {
		t.Error("an invalid VERSION_CONTROL was accepted")
	}
}

func TestUpdateValues(t *testing.T) {
	for arg, want := range map[string]Update{
		"--update":           UpdateOlder,
		"--update=all":       UpdateAll,
		"--update=none":      UpdateNone,
		"--update=none-fail": UpdateNoneFail,
		"--update=older":     UpdateOlder,
	} {
		if got := mustParse(t, arg, "a", "b").Update; got != want {
			t.Errorf("%s: got %v, want %v", arg, got, want)
		}
	}
	if _, err := Parse([]string{"--update=sometimes", "a", "b"}); err == nil {
		t.Error("--update=sometimes was accepted")
	}
}

func TestDebugImpliesVerboseAndDetail(t *testing.T) {
	o := mustParse(t, "--debug", "a", "b")
	if !o.Verbose || o.LogLevel != "debug" {
		t.Errorf("--debug: verbose=%v log-level=%q", o.Verbose, o.LogLevel)
	}
}

func TestArchiveThenNoPreserve(t *testing.T) {
	o := mustParse(t, "-a", "--no-preserve=ownership", "src", "dst")
	if !o.Recursive || o.Deref != DerefNever {
		t.Error("-a did not imply -dR")
	}
	want := local.Preserve{Mode: true, Timestamps: true, Links: true, XAttr: true, Context: true}
	if o.Preserve != want {
		t.Errorf("preserve after --no-preserve=ownership: %+v", o.Preserve)
	}
}

// The later of -i and -n wins, as it does in cp.
func TestInteractiveAndNoClobberOrder(t *testing.T) {
	o := mustParse(t, "-i", "-n", "a", "b")
	if o.Interactive || !o.NoClobber {
		t.Error("-n after -i should win")
	}
	o = mustParse(t, "-n", "-i", "a", "b")
	if !o.Interactive || o.NoClobber {
		t.Error("-i after -n should win")
	}
}

func TestSuffixImpliesBackup(t *testing.T) {
	o := mustParse(t, "-S", ".orig", "a", "b")
	if o.Suffix != ".orig" || o.Backup == BackupNone {
		t.Errorf("suffix %q backup %v", o.Suffix, o.Backup)
	}
}

func TestOperandShapes(t *testing.T) {
	for _, argv := range [][]string{
		{},
		{"onlysource"},
		{"-T", "a", "b", "c"},
		{"-t", "dir"},
		{"-t", "dir", "-T", "a"},
		{"--benchmark", "a", "b"},
	} {
		if _, err := Parse(argv); err == nil {
			t.Errorf("Parse(%v) should have failed", argv)
		}
	}

	o := mustParse(t, "a", "b", "dest")
	if len(o.Sources) != 2 || o.Dest != "dest" {
		t.Errorf("sources %v dest %q", o.Sources, o.Dest)
	}
	o = mustParse(t, "-t", "dir", "a", "b")
	if len(o.Sources) != 2 || o.Dest != "dir" {
		t.Errorf("-t: sources %v dest %q", o.Sources, o.Dest)
	}
}

func TestConflictingOptionsRefused(t *testing.T) {
	for _, argv := range [][]string{
		{"-l", "-s", "a", "b"},
		{"--delete", "a", "b"},
		{"-b", "-n", "a", "b"},
	} {
		if _, err := Parse(argv); err == nil {
			t.Errorf("Parse(%v) should have failed", argv)
		}
	}
}

func TestMetadataParsing(t *testing.T) {
	o := mustParse(t, "--metadata", "a=1,b=x=y", "--metadata", "c_2=3", "s", "d")
	want := map[string]string{"a": "1", "b": "x=y", "c_2": "3"}
	if len(o.Metadata) != len(want) {
		t.Fatalf("metadata %v", o.Metadata)
	}
	for k, v := range want {
		if o.Metadata[k] != v {
			t.Errorf("metadata[%q] = %q, want %q", k, o.Metadata[k], v)
		}
	}
	for _, bad := range []string{"1x=2", "sp ace=1", "=v", "novalue"} {
		if _, err := Parse([]string{"--metadata", bad, "s", "d"}); err == nil {
			t.Errorf("metadata %q was accepted", bad)
		}
	}
}

func TestParseTimeSpec(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	if got, err := ParseTimeSpec("2024-05-06", now); err != nil ||
		!got.Equal(time.Date(2024, 5, 6, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date: %v %v", got, err)
	}
	if got, err := ParseTimeSpec("36h", now); err != nil || !got.Equal(now.Add(-36*time.Hour)) {
		t.Errorf("age hours: %v %v", got, err)
	}
	if got, err := ParseTimeSpec("7d", now); err != nil || !got.Equal(now.Add(-7*24*time.Hour)) {
		t.Errorf("age days: %v %v", got, err)
	}
	if _, err := ParseTimeSpec("yesterday", now); err == nil {
		t.Error("nonsense time was accepted")
	}
}

func TestBenchSpec(t *testing.T) {
	o := mustParse(t, "--benchmark=3x2MiB", "dest")
	if !o.Benchmark || o.BenchFiles != 3 || o.BenchSize != 2<<20 {
		t.Errorf("benchmark spec: files %d size %d", o.BenchFiles, o.BenchSize)
	}
	for _, bad := range []string{"x", "0x1M", "3x0", "3xhuge"} {
		if _, err := Parse([]string{"--benchmark=" + bad, "dest"}); err == nil {
			t.Errorf("--benchmark=%s was accepted", bad)
		}
	}
}

func TestJobsDefaultDependsOnDestination(t *testing.T) {
	local := mustParse(t, "a", "b")
	remote := mustParse(t, "a", "azure://acct/c/")
	if local.Jobs > remote.Jobs {
		t.Errorf("local default %d should not exceed network default %d",
			local.Jobs, remote.Jobs)
	}
	fixed := mustParse(t, "-j", "3", "a", "azure://acct/c/")
	if fixed.Jobs != 3 {
		t.Errorf("-j 3: got %d", fixed.Jobs)
	}
}

func TestHelpListsEveryVisibleOption(t *testing.T) {
	var b strings.Builder
	PrintUsage(&b)
	out := b.String()
	for _, needle := range []string{"--backup[=CONTROL]", "-b", "--update[=UPDATE]",
		"--debug", "--jobs=N", "--exclude=PATTERN"} {
		if !strings.Contains(out, needle) {
			t.Errorf("help output lacks %q", needle)
		}
	}
}
