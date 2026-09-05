package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/JohanLindvall/azcp/internal/cpflags"
	"github.com/JohanLindvall/azcp/internal/humanize"
	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/retryx"
	"github.com/JohanLindvall/azcp/internal/store/azure"
	"github.com/JohanLindvall/azcp/internal/store/local"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// Deref says how symbolic links named in the source are treated.
type Deref int

const (
	// DerefAuto follows links unless the copy is recursive, which is what cp
	// does when none of -H, -L or -P is given.
	DerefAuto Deref = iota
	// DerefCmdline follows only the links named on the command line (-H).
	DerefCmdline
	// DerefAlways follows every link (-L).
	DerefAlways
	// DerefNever copies links as links (-P).
	DerefNever
)

// Update selects which existing destinations get replaced.
type Update int

const (
	// UpdateAll replaces every existing destination.
	UpdateAll Update = iota
	// UpdateNone leaves every existing destination alone, silently.
	UpdateNone
	// UpdateNoneFail leaves every existing destination alone and fails.
	UpdateNoneFail
	// UpdateOlder replaces a destination only when the source is newer (-u).
	UpdateOlder
)

// Backup selects the backup naming scheme.
type Backup int

const (
	// BackupNone makes no backups.
	BackupNone Backup = iota
	// BackupSimple appends the suffix (--suffix, default "~").
	BackupSimple
	// BackupNumbered appends ".~N~", counting up from what is there.
	BackupNumbered
	// BackupExisting numbers where numbered backups exist, else appends.
	BackupExisting
)

// Output selects how results are reported.
type Output int

const (
	// OutputText is cp's own output, plus the live display and summary.
	OutputText Output = iota
	// OutputJSON writes one object per line, ending with a summary.
	OutputJSON
)

// GlobMode controls wildcard expansion of arguments.
type GlobMode int

const (
	// GlobAuto expands an argument that contains metacharacters and does not
	// name an existing file. It is the safe default: a file literally called
	// "a[1].txt" still copies.
	GlobAuto GlobMode = iota
	// GlobAlways expands any argument containing metacharacters.
	GlobAlways
	// GlobNever takes every argument literally.
	GlobNever
)

// Every value an enumerated option accepts, in the order --help and error
// messages list them.
var (
	reflinkModes = []choice[local.Reflink]{
		{"always", local.ReflinkAlways}, {"auto", local.ReflinkAuto}, {"never", local.ReflinkNever}}
	sparseModes = []choice[local.Sparse]{
		{"always", local.SparseAlways}, {"auto", local.SparseAuto}, {"never", local.SparseNever}}
	updateModes = []choice[Update]{
		{"all", UpdateAll}, {"none", UpdateNone}, {"none-fail", UpdateNoneFail}, {"older", UpdateOlder}}
	outputModes   = []choice[Output]{{"text", OutputText}, {"json", OutputJSON}}
	globModes     = []choice[GlobMode]{{"auto", GlobAuto}, {"always", GlobAlways}, {"never", GlobNever}}
	progressModes = []choice[progress.Mode]{
		{"auto", progress.ModeAuto}, {"always", progress.ModeAlways}, {"never", progress.ModeNever}}
	authModes = []choice[azure.AuthMode]{
		{"auto", azure.AuthAuto}, {"identity", azure.AuthIdentity}, {"browser", azure.AuthBrowser},
		{"device", azure.AuthDevice}, {"anonymous", azure.AuthAnonymous}}
	md5Modes = []choice[azure.MD5Check]{
		{"off", azure.MD5Off}, {"warn", azure.MD5Warn}, {"fail", azure.MD5Fail}, {"require", azure.MD5Require}}
)

// choice pairs a spelling accepted on the command line with what it selects.
type choice[T any] struct {
	name  string
	value T
}

// choose resolves v against what flag accepts, or says what it wanted.
func choose[T any](flag, v string, choices []choice[T]) (T, error) {
	names := make([]string, len(choices))
	for i, c := range choices {
		if c.name == v {
			return c.value, nil
		}
		names[i] = c.name
	}
	var zero T
	return zero, fmt.Errorf("invalid argument %q for '%s' (want %s)", v, flag, orList(names))
}

// orList renders names as "a, b or c".
func orList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// Options is the fully resolved configuration for one invocation.
type Options struct {
	ShowHelp    bool
	ShowVersion bool

	// cp behaviour
	Recursive            bool
	AttributesOnly       bool
	Backup               Backup
	Suffix               string
	Force                bool
	Interactive          bool
	NoClobber            bool
	Update               Update
	Deref                Deref
	Preserve             local.Preserve
	Parents              bool
	Reflink              local.Reflink
	RemoveDestination    bool
	Sparse               local.Sparse
	StripTrailingSlashes bool
	HardLink             bool
	SymbolicLink         bool
	TargetDirectory      string
	HasTargetDir         bool
	NoTargetDirectory    bool
	Verbose              bool
	// ContextExplicit records that SELinux context handling was asked for by
	// name rather than swept in by --preserve=all, so that -a does not warn
	// about something the user never mentioned.
	ContextExplicit bool
	OneFileSystem   bool
	SELinux         bool

	// Transfer behaviour
	Jobs int
	// jobsSet records that --jobs was given, so the default is not second-
	// guessed after the operands reveal what kind of copy this is.
	jobsSet         bool
	PartSize        int64
	PartConcurrency int
	Retries         int
	RetryDelay      time.Duration
	RetryMaxDelay   time.Duration
	Timeout         time.Duration
	BandwidthLimit  int64
	MaxErrors       int
	DryRun          bool
	Resume          bool
	Delete          bool
	Glob            GlobMode
	Exclude         []string
	Include         []string
	// FilesFrom names a file of further SOURCE operands, one per line; "-" is
	// standard input. It is how a pipeline hands over a list too long for a
	// command line, and what an AzCopy --list-of-files becomes.
	FilesFrom string

	// Presentation
	Progress         progress.Mode
	ProgressInterval time.Duration
	LogLevel         string
	LogFormat        string
	LogFile          string
	Output           Output

	// Benchmark, when set, measures throughput to the destination instead of
	// copying anything. BenchFiles and BenchSize say how much data to use.
	Benchmark  bool
	BenchFiles int
	BenchSize  int64

	// Azure
	Auth               azure.AuthMode
	TenantID           string
	EndpointSuffix     string
	CreateContainer    bool
	ContentType        string
	AccessTier         string
	PutMD5             bool
	CheckMD5           azure.MD5Check
	ContentEncoding    string
	ContentDisposition string
	ContentLanguage    string
	CacheControl       string
	Metadata           map[string]string
	CopyMetadata       bool
	Decompress         bool
	NewerThan          time.Time
	OlderThan          time.Time

	Sources []string
	Dest    string
}

// Defaults returns the configuration used when no options are given.
func Defaults() *Options {
	return &Options{
		Suffix:           backupSuffixDefault(),
		Jobs:             8,
		PartSize:         8 << 20,
		PartConcurrency:  4,
		Retries:          retryx.Default.MaxAttempts,
		RetryDelay:       retryx.Default.BaseDelay,
		RetryMaxDelay:    retryx.Default.MaxDelay,
		ProgressInterval: progress.DefaultInterval,
		BenchFiles:       10,
		BenchSize:        64 << 20,
		LogLevel:         "warn",
		LogFormat:        "text",
		Auth:             azure.AuthAuto,
		// What --help and the README promise: a blob that carries a checksum
		// is verified, and a mismatch fails the transfer.
		CheckMD5: azure.MD5Fail,
	}
}

// parseBenchSpec reads "10x64MiB": how many files, and how big each one is.
func parseBenchSpec(spec string) (int, int64, error) {
	countText, sizeText, ok := strings.Cut(strings.ToLower(spec), "x")
	if !ok {
		return 0, 0, fmt.Errorf("invalid argument %q for '--benchmark' "+
			"(want COUNTxSIZE, e.g. 10x64MiB)", spec)
	}
	n, err := strconv.Atoi(strings.TrimSpace(countText))
	if err != nil || n < 1 {
		return 0, 0, fmt.Errorf("invalid file count %q for '--benchmark'", countText)
	}
	size, err := humanize.ParseSize(sizeText)
	if err != nil || size < 1 {
		return 0, 0, fmt.Errorf("invalid size %q for '--benchmark'", sizeText)
	}
	return n, size, nil
}

// parseMetadata reads "k=v,k=v" into the map. Blob metadata names have to be
// valid identifiers, so a name that could not be stored is refused here rather
// than by the service halfway through a transfer.
func parseMetadata(spec string, into map[string]string) error {
	for pair := range strings.SplitSeq(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return fmt.Errorf("invalid metadata %q (want NAME=VALUE)", pair)
		}
		if !validMetadataName(k) {
			return fmt.Errorf("invalid metadata name %q: names must start with "+
				"a letter or underscore and contain only letters, digits and "+
				"underscores", k)
		}
		into[k] = v
	}
	return nil
}

func validMetadataName(k string) bool {
	for i, r := range k {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return k != ""
}

// ParseTimeSpec reads a point in time written as an RFC 3339 timestamp, a plain
// date, or an age such as "7d" or "36h" meaning that long before now.
func ParseTimeSpec(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty time")
	}
	for _, layout := range []string{
		time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	if d, err := parseAge(s); err == nil {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q "+
		"(want 2006-01-02, an RFC 3339 timestamp, or an age such as 7d)", s)
}

// parseAge accepts Go durations plus a day suffix, which is what people
// actually write for this.
func parseAge(s string) (time.Duration, error) {
	if rest, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(days * 24 * float64(time.Hour)), nil
	}
	return time.ParseDuration(s)
}

// defaultJobs picks how many files to move at once when the user has not said.
//
// A network transfer spends nearly all its time waiting, so many in flight is
// what fills the link; the figure scales with the machine the way other
// transfer tools do. A copy that never leaves the filesystem is the opposite
// case — the disk is the bottleneck, and seeking between many files makes it
// slower rather than faster — so it stays modest.
//
// It also decides how many sockets a run opens, since HTTP/1.1 carries one
// request per connection and each job may have --part-concurrency of them
// outstanding. Thirty-two jobs of four parts is a hundred and twenty-eight
// requests in flight, which is far more than a fast link needs — at 70 MB/s
// and a 15 ms round trip there is about a megabyte in flight altogether, eight
// kilobytes per connection — and it is half the sockets the figure used to
// open. Middleboxes count flows; links count bytes.
func defaultJobs(network bool) int {
	if !network {
		return min(4, max(runtime.NumCPU(), 1))
	}
	return min(32, max(8, 2*runtime.NumCPU()))
}

// TouchesNetwork reports whether either side of the copy is a remote URL.
func (o *Options) TouchesNetwork() bool {
	return uri.IsRemoteArg(o.Dest) || slices.ContainsFunc(o.Sources, uri.IsRemoteArg)
}

// PeakRequests is how many requests the run can have outstanding at once, used
// to size the HTTP connection pool.
func (o *Options) PeakRequests() int { return o.Jobs * o.PartConcurrency }

// UsageError marks a problem with the command line, which the caller reports
// with exit status 2 and a pointer at --help.
type UsageError struct{ err error }

// Usage wraps err as a command-line problem.
func Usage(err error) *UsageError { return &UsageError{err} }

func (e *UsageError) Error() string { return e.err.Error() }
func (e *UsageError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error {
	return &UsageError{fmt.Errorf(format, args...)}
}

// Parse turns argv (without the program name) into options.
func Parse(argv []string) (*Options, error) {
	res, err := cpflags.Parse(specs, argv)
	if err != nil {
		return nil, &UsageError{err}
	}
	o := Defaults()

	// Applied in order, so that a later option overrides an earlier one the way
	// cp behaves: "--preserve=all --no-preserve=ownership" drops ownership.
	for _, f := range res.Flags {
		if err := o.apply(f); err != nil {
			return nil, &UsageError{err}
		}
	}
	if o.ShowHelp || o.ShowVersion {
		return o, nil
	}
	operands := res.Operands
	if o.FilesFrom != "" {
		listed, err := readFilesFrom(o.FilesFrom)
		if err != nil {
			return nil, err
		}
		// Listed names are sources, and go in front so that whatever was
		// typed keeps its place: the destination stays last.
		operands = append(listed, operands...)
	}
	if err := o.resolveOperands(operands); err != nil {
		return nil, err
	}
	if !o.jobsSet {
		o.Jobs = defaultJobs(o.TouchesNetwork())
	}
	return o, o.validate()
}

func (o *Options) apply(f cpflags.Flag) error {
	name := f.Spec.Long
	if name == "" {
		name = string(f.Spec.Short)
	}
	switch name {
	// --- cp ----------------------------------------------------------------
	case "archive", "a":
		o.Recursive = true
		o.Deref = DerefNever
		o.Preserve = local.Preserve{Mode: true, Ownership: true, Timestamps: true,
			Links: true, XAttr: true, Context: true}
	case "attributes-only":
		o.AttributesOnly = true
	case "backup":
		mode, err := parseBackup(f.Value, f.HasValue)
		if err != nil {
			return err
		}
		o.Backup = mode
	case "b":
		// GNU's -b is --backup without the argument.
		mode, err := parseBackup("", false)
		if err != nil {
			return err
		}
		o.Backup = mode
	case "copy-contents":
		// Only affects recursion into special files, which this tool never
		// does; accepted so existing command lines keep working.
	case "debug":
		// cp's --debug explains how each file was copied. The nearest
		// equivalents here are -v and the debug-level log records, which is
		// where the transfer decisions are narrated.
		o.Verbose = true
		o.LogLevel = "debug"
	case "d":
		o.Deref = DerefNever
		o.Preserve.Links = true
	case "force", "f":
		o.Force = true
	case "interactive", "i":
		o.Interactive = true
		o.NoClobber = false
	case "H":
		o.Deref = DerefCmdline
	case "link", "l":
		o.HardLink = true
	case "dereference", "L":
		o.Deref = DerefAlways
	case "no-clobber", "n":
		o.NoClobber = true
		o.Interactive = false
	case "no-dereference", "P":
		o.Deref = DerefNever
	case "p":
		o.Preserve.Mode = true
		o.Preserve.Ownership = true
		o.Preserve.Timestamps = true
	case "preserve":
		list := f.Value
		if !f.HasValue {
			list = "mode,ownership,timestamps"
		}
		explicit, err := applyPreserve(&o.Preserve, list, true)
		o.ContextExplicit = o.ContextExplicit || explicit
		return err
	case "no-preserve":
		_, err := applyPreserve(&o.Preserve, f.Value, false)
		return err
	case "parents":
		o.Parents = true
	case "recursive", "R", "r":
		o.Recursive = true
	case "reflink":
		return setChoice(&o.Reflink, f, valueOr(f, "always"), reflinkModes)
	case "remove-destination":
		o.RemoveDestination = true
	case "sparse":
		return setChoice(&o.Sparse, f, f.Value, sparseModes)
	case "strip-trailing-slashes":
		o.StripTrailingSlashes = true
	case "symbolic-link", "s":
		o.SymbolicLink = true
	case "suffix", "S":
		o.Suffix = f.Value
		if o.Backup == BackupNone {
			o.Backup = BackupExisting
		}
	case "target-directory", "t":
		o.TargetDirectory = f.Value
		o.HasTargetDir = true
	case "no-target-directory", "T":
		o.NoTargetDirectory = true
	case "u":
		o.Update = UpdateOlder
	case "update":
		return setChoice(&o.Update, f, valueOr(f, "older"), updateModes)
	case "verbose", "v":
		o.Verbose = true
	case "one-file-system", "x":
		o.OneFileSystem = true
	case "Z", "context":
		o.SELinux, o.ContextExplicit = true, true

	// --- extensions ---------------------------------------------------------
	case "jobs", "j":
		if err := setPositive(&o.Jobs, f); err != nil {
			return err
		}
		o.jobsSet = true
	case "part-size":
		return setSize(&o.PartSize, f)
	case "part-concurrency":
		return setPositive(&o.PartConcurrency, f)
	case "retries":
		return setPositive(&o.Retries, f)
	case "retry-delay":
		return setDuration(&o.RetryDelay, f)
	case "retry-max-delay":
		return setDuration(&o.RetryMaxDelay, f)
	case "bwlimit":
		return setSize(&o.BandwidthLimit, f)
	case "timeout":
		return setDuration(&o.Timeout, f)
	case "max-errors":
		n, err := strconv.Atoi(f.Value)
		if err != nil || n < 0 {
			return fmt.Errorf("invalid argument %q for '--max-errors' "+
				"(want a count, or 0 for no limit)", f.Value)
		}
		o.MaxErrors = n
	case "progress":
		return setChoice(&o.Progress, f, lower(f.Value), progressModes)
	case "no-progress":
		o.Progress = progress.ModeNever
	case "progress-interval":
		if err := setDuration(&o.ProgressInterval, f); err != nil {
			return err
		}
		if o.ProgressInterval < 20*time.Millisecond {
			return errors.New("--progress-interval must be at least 20ms")
		}
	case "output":
		return setChoice(&o.Output, f, lower(f.Value), outputModes)
	case "benchmark":
		o.Benchmark = true
		if f.HasValue && f.Value != "" {
			n, size, err := parseBenchSpec(f.Value)
			if err != nil {
				return err
			}
			o.BenchFiles, o.BenchSize = n, size
		}
	case "log-level":
		o.LogLevel = f.Value
	case "log-format":
		o.LogFormat = f.Value
	case "log-file":
		o.LogFile = f.Value
	case "exclude":
		o.Exclude = append(o.Exclude, f.Value)
	case "include":
		o.Include = append(o.Include, f.Value)
	case "files-from":
		o.FilesFrom = f.Value
	case "delete":
		o.Delete = true
	case "resume":
		o.Resume = true
	case "dry-run":
		o.DryRun = true
	case "glob":
		return setChoice(&o.Glob, f, f.Value, globModes)
	case "auth":
		return setChoice(&o.Auth, f, lower(f.Value), authModes)
	case "tenant":
		o.TenantID = f.Value
	case "endpoint-suffix":
		o.EndpointSuffix = strings.TrimPrefix(f.Value, ".")
	case "create-container":
		o.CreateContainer = true
	case "content-type":
		o.ContentType = f.Value
	case "put-md5":
		o.PutMD5 = true
	case "check-md5":
		return setChoice(&o.CheckMD5, f, lower(f.Value), md5Modes)
	case "content-encoding":
		o.ContentEncoding = f.Value
	case "content-disposition":
		o.ContentDisposition = f.Value
	case "content-language":
		o.ContentLanguage = f.Value
	case "cache-control":
		o.CacheControl = f.Value
	case "metadata":
		if o.Metadata == nil {
			o.Metadata = map[string]string{}
		}
		return parseMetadata(f.Value, o.Metadata)
	case "copy-metadata":
		o.CopyMetadata = true
	case "decompress":
		o.Decompress = true
	case "newer-than":
		t, err := ParseTimeSpec(f.Value, time.Now())
		if err != nil {
			return fmt.Errorf("invalid argument for '--newer-than': %w", err)
		}
		o.NewerThan = t
	case "older-than":
		t, err := ParseTimeSpec(f.Value, time.Now())
		if err != nil {
			return fmt.Errorf("invalid argument for '--older-than': %w", err)
		}
		o.OlderThan = t
	case "access-tier":
		o.AccessTier = f.Value

	case "help":
		o.ShowHelp = true
	case "version":
		o.ShowVersion = true
	default:
		return fmt.Errorf("unhandled option %q", name)
	}
	return nil
}

func valueOr(f cpflags.Flag, dflt string) string {
	if f.HasValue && f.Value != "" {
		return f.Value
	}
	return dflt
}

// lower normalises a value for the options whose words are not case-sensitive
// — the extensions, that is; cp's own reject "Always" and so do we.
func lower(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

// The set* helpers read one option's value into its field, naming the option
// in the message when the value will not do.

func setChoice[T any](dst *T, f cpflags.Flag, v string, choices []choice[T]) error {
	got, err := choose(f.Name(), v, choices)
	if err != nil {
		return err
	}
	*dst = got
	return nil
}

func setPositive(dst *int, f cpflags.Flag) error {
	n, err := strconv.Atoi(f.Value)
	if err != nil || n < 1 {
		return fmt.Errorf("invalid argument %q for '%s' (want a positive number)", f.Value, f.Name())
	}
	*dst = n
	return nil
}

func setDuration(dst *time.Duration, f cpflags.Flag) error {
	d, err := time.ParseDuration(f.Value)
	if err != nil {
		return fmt.Errorf("invalid argument for '%s': %w", f.Name(), err)
	}
	*dst = d
	return nil
}

func setSize(dst *int64, f cpflags.Flag) error {
	n, err := humanize.ParseSize(f.Value)
	if err != nil {
		return fmt.Errorf("invalid argument for '%s': %w", f.Name(), err)
	}
	*dst = n
	return nil
}

func applyPreserve(p *local.Preserve, list string, on bool) (explicitContext bool, err error) {
	for item := range strings.SplitSeq(list, ",") {
		switch strings.TrimSpace(item) {
		case "":
		case "mode":
			p.Mode = on
		case "ownership":
			p.Ownership = on
		case "timestamps":
			p.Timestamps = on
		case "links":
			p.Links = on
		case "xattr":
			p.XAttr = on
		case "context":
			p.Context = on
			explicitContext = on
		case "all":
			*p = local.Preserve{Mode: on, Ownership: on, Timestamps: on,
				Links: on, XAttr: on, Context: on}
		default:
			return false, fmt.Errorf("invalid attribute %q "+
				"(want mode, ownership, timestamps, links, xattr, context or all)", item)
		}
	}
	return explicitContext, nil
}

func parseBackup(v string, has bool) (Backup, error) {
	if !has || v == "" {
		v = os.Getenv("VERSION_CONTROL")
	}
	switch v {
	case "", "existing", "nil":
		return BackupExisting, nil
	case "none", "off":
		return BackupNone, nil
	case "numbered", "t":
		return BackupNumbered, nil
	case "simple", "never":
		return BackupSimple, nil
	}
	return BackupNone, fmt.Errorf("invalid backup type %q "+
		"(want none, numbered, existing or simple)", v)
}

func backupSuffixDefault() string {
	if s := os.Getenv("SIMPLE_BACKUP_SUFFIX"); s != "" {
		return s
	}
	return "~"
}

// stdin is what --files-from=- reads. Tests point it elsewhere.
var stdin io.Reader = os.Stdin

// readFilesFrom reads one operand per line. Blank lines are skipped and a
// trailing carriage return is dropped, so a list written on Windows works;
// nothing else is interpreted, because a file name may contain anything.
func readFilesFrom(name string) ([]string, error) {
	r := stdin
	if name != "-" {
		f, err := os.Open(name)
		if err != nil {
			return nil, fmt.Errorf("cannot read the file list: %w", err)
		}
		defer f.Close()
		r = f
	}
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if line := strings.TrimSuffix(sc.Text(), "\r"); line != "" {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("cannot read the file list %s: %w", name, err)
	}
	return out, nil
}

// resolveOperands splits the positional arguments into sources and a
// destination, following cp's three call shapes.
func (o *Options) resolveOperands(operands []string) error {
	if o.HasTargetDir {
		if len(operands) == 0 {
			return usagef("missing file operand")
		}
		o.Sources = operands
		o.Dest = o.TargetDirectory
		return nil
	}
	if o.Benchmark {
		if len(operands) != 1 {
			return usagef("--benchmark takes one destination and nothing else")
		}
		o.Dest = operands[0]
		return nil
	}
	switch len(operands) {
	case 0:
		return usagef("missing file operand")
	case 1:
		return usagef("missing destination file operand after '%s'", operands[0])
	}
	if o.NoTargetDirectory && len(operands) > 2 {
		return usagef("extra operand '%s'", operands[2])
	}
	o.Sources = operands[:len(operands)-1]
	o.Dest = operands[len(operands)-1]
	return nil
}

func (o *Options) validate() error {
	if o.HasTargetDir && o.NoTargetDirectory {
		return usagef("cannot combine --target-directory (-t) and --no-target-directory (-T)")
	}
	if o.HardLink && o.SymbolicLink {
		return usagef("cannot make both hard and symbolic links")
	}
	if o.Delete && !o.Recursive {
		return usagef("--delete only makes sense with a recursive copy (-r)")
	}
	if o.Backup != BackupNone && o.NoClobber {
		return usagef("options --backup and --no-clobber are mutually exclusive")
	}
	if o.Jobs < 1 {
		o.Jobs = 1
	}
	if o.PartConcurrency < 1 {
		o.PartConcurrency = 1
	}
	return nil
}

// DerefSource reports whether a source named on the command line should be
// resolved through symbolic links.
func (o *Options) DerefSource() bool {
	switch o.Deref {
	case DerefAlways, DerefCmdline:
		return true
	case DerefNever:
		return false
	default:
		// cp follows links unless it is recursing.
		return !o.Recursive
	}
}

// DerefWalk reports whether links met during recursion should be resolved.
func (o *Options) DerefWalk() bool { return o.Deref == DerefAlways }
