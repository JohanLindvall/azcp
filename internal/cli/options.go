package cli

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/JohanLindvall/azcp/internal/cpflags"
	"github.com/JohanLindvall/azcp/internal/humanize"
	"github.com/JohanLindvall/azcp/internal/progress"
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
	DerefCmdline
	DerefAlways
	DerefNever
)

// Update selects which existing destinations get replaced.
type Update int

const (
	UpdateAll Update = iota
	UpdateNone
	UpdateNoneFail
	UpdateOlder
)

// Backup selects the backup naming scheme.
type Backup int

const (
	BackupNone Backup = iota
	BackupSimple
	BackupNumbered
	BackupExisting
)

// Output selects how results are reported.
type Output int

const (
	OutputText Output = iota
	OutputJSON
)

// GlobMode controls wildcard expansion of arguments.
type GlobMode int

const (
	// GlobAuto expands an argument that contains metacharacters and does not
	// name an existing file. It is the safe default: a file literally called
	// "a[1].txt" still copies.
	GlobAuto GlobMode = iota
	GlobAlways
	GlobNever
)

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
		Retries:          6,
		RetryDelay:       300 * time.Millisecond,
		RetryMaxDelay:    30 * time.Second,
		ProgressInterval: progress.DefaultInterval,
		BenchFiles:       10,
		BenchSize:        64 << 20,
		LogLevel:         "warn",
		LogFormat:        "text",
		Auth:             azure.AuthAuto,
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
	for _, pair := range strings.Split(spec, ",") {
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
		return time.Time{}, fmt.Errorf("empty time")
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
	if uri.IsRemoteArg(o.Dest) {
		return true
	}
	for _, s := range o.Sources {
		if uri.IsRemoteArg(s) {
			return true
		}
	}
	return false
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
	if err := o.resolveOperands(res.Operands); err != nil {
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
	case "copy-contents":
		// Only affects recursion into special files, which this tool never
		// does; accepted so existing command lines keep working.
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
		switch v := valueOr(f, "always"); v {
		case "always":
			o.Reflink = local.ReflinkAlways
		case "auto":
			o.Reflink = local.ReflinkAuto
		case "never":
			o.Reflink = local.ReflinkNever
		default:
			return fmt.Errorf("invalid argument %q for '--reflink' "+
				"(want always, auto or never)", v)
		}
	case "remove-destination":
		o.RemoveDestination = true
	case "sparse":
		switch f.Value {
		case "always":
			o.Sparse = local.SparseAlways
		case "auto":
			o.Sparse = local.SparseAuto
		case "never":
			o.Sparse = local.SparseNever
		default:
			return fmt.Errorf("invalid argument %q for '--sparse' "+
				"(want always, auto or never)", f.Value)
		}
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
	case "update", "u":
		v := valueOr(f, "older")
		switch v {
		case "all":
			o.Update = UpdateAll
		case "none":
			o.Update = UpdateNone
		case "none-fail":
			o.Update = UpdateNoneFail
		case "older":
			o.Update = UpdateOlder
		default:
			return fmt.Errorf("invalid argument %q for '--update' "+
				"(want all, none, none-fail or older)", v)
		}
	case "verbose", "v":
		o.Verbose = true
	case "one-file-system", "x":
		o.OneFileSystem = true
	case "Z", "context":
		o.SELinux, o.ContextExplicit = true, true

	// --- extensions ---------------------------------------------------------
	case "jobs", "j":
		n, err := positiveInt(f.Value, "--jobs")
		if err != nil {
			return err
		}
		o.Jobs, o.jobsSet = n, true
	case "part-size":
		n, err := humanize.ParseSize(f.Value)
		if err != nil {
			return fmt.Errorf("invalid argument for '--part-size': %w", err)
		}
		o.PartSize = n
	case "part-concurrency":
		n, err := positiveInt(f.Value, "--part-concurrency")
		if err != nil {
			return err
		}
		o.PartConcurrency = n
	case "retries":
		n, err := positiveInt(f.Value, "--retries")
		if err != nil {
			return err
		}
		o.Retries = n
	case "retry-delay":
		d, err := time.ParseDuration(f.Value)
		if err != nil {
			return fmt.Errorf("invalid argument for '--retry-delay': %w", err)
		}
		o.RetryDelay = d
	case "retry-max-delay":
		d, err := time.ParseDuration(f.Value)
		if err != nil {
			return fmt.Errorf("invalid argument for '--retry-max-delay': %w", err)
		}
		o.RetryMaxDelay = d
	case "bwlimit":
		n, err := humanize.ParseSize(f.Value)
		if err != nil {
			return fmt.Errorf("invalid argument for '--bwlimit': %w", err)
		}
		o.BandwidthLimit = n
	case "timeout":
		d, err := time.ParseDuration(f.Value)
		if err != nil {
			return fmt.Errorf("invalid argument for '--timeout': %w", err)
		}
		o.Timeout = d
	case "max-errors":
		n, err := strconv.Atoi(f.Value)
		if err != nil || n < 0 {
			return fmt.Errorf("invalid argument %q for '--max-errors' "+
				"(want a count, or 0 for no limit)", f.Value)
		}
		o.MaxErrors = n
	case "progress":
		m, err := progress.ParseMode(f.Value)
		if err != nil {
			return err
		}
		o.Progress = m
	case "no-progress":
		o.Progress = progress.ModeNever
	case "progress-interval":
		d, err := time.ParseDuration(f.Value)
		if err != nil {
			return fmt.Errorf("invalid argument for '--progress-interval': %w", err)
		}
		if d < 20*time.Millisecond {
			return fmt.Errorf("--progress-interval must be at least 20ms")
		}
		o.ProgressInterval = d
	case "output":
		switch strings.ToLower(f.Value) {
		case "text":
			o.Output = OutputText
		case "json":
			o.Output = OutputJSON
		default:
			return fmt.Errorf("invalid argument %q for '--output' (want text or json)", f.Value)
		}
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
	case "delete":
		o.Delete = true
	case "resume":
		o.Resume = true
	case "dry-run":
		o.DryRun = true
	case "glob":
		switch f.Value {
		case "auto":
			o.Glob = GlobAuto
		case "always":
			o.Glob = GlobAlways
		case "never":
			o.Glob = GlobNever
		default:
			return fmt.Errorf("invalid argument %q for '--glob' "+
				"(want auto, always or never)", f.Value)
		}
	case "auth":
		m, err := azure.ParseAuthMode(f.Value)
		if err != nil {
			return err
		}
		o.Auth = m
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
		m, err := azure.ParseMD5Check(f.Value)
		if err != nil {
			return err
		}
		o.CheckMD5 = m
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

func positiveInt(s, what string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid argument %q for '%s' (want a positive number)", s, what)
	}
	return n, nil
}

func applyPreserve(p *local.Preserve, list string, on bool) (explicitContext bool, err error) {
	for _, item := range strings.Split(list, ",") {
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
