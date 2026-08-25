package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/JohanLindvall/azcp/internal/cpflags"
	"github.com/JohanLindvall/azcp/internal/humanize"
	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/store/azure"
	"github.com/JohanLindvall/azcp/internal/store/local"
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
	OneFileSystem        bool
	SELinux              bool

	// Transfer behaviour
	Jobs            int
	PartSize        int64
	PartConcurrency int
	Retries         int
	RetryDelay      time.Duration
	RetryMaxDelay   time.Duration
	Timeout         time.Duration
	MaxErrors       int
	DryRun          bool
	Glob            GlobMode

	// Presentation
	Progress  progress.Mode
	LogLevel  string
	LogFormat string
	LogFile   string

	// Azure
	Auth            azure.AuthMode
	TenantID        string
	EndpointSuffix  string
	CreateContainer bool
	ContentType     string
	AccessTier      string

	Sources []string
	Dest    string
}

// Defaults returns the configuration used when no options are given.
func Defaults() *Options {
	return &Options{
		Suffix:          backupSuffixDefault(),
		Jobs:            8,
		PartSize:        8 << 20,
		PartConcurrency: 4,
		Retries:         6,
		RetryDelay:      300 * time.Millisecond,
		RetryMaxDelay:   30 * time.Second,
		LogLevel:        "warn",
		LogFormat:       "text",
		Auth:            azure.AuthAuto,
	}
}

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
		return applyPreserve(&o.Preserve, list, true)
	case "no-preserve":
		return applyPreserve(&o.Preserve, f.Value, false)
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
		o.SELinux = true

	// --- extensions ---------------------------------------------------------
	case "jobs", "j":
		n, err := positiveInt(f.Value, "--jobs")
		if err != nil {
			return err
		}
		o.Jobs = n
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
	case "log-level":
		o.LogLevel = f.Value
	case "log-format":
		o.LogFormat = f.Value
	case "log-file":
		o.LogFile = f.Value
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

func applyPreserve(p *local.Preserve, list string, on bool) error {
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
		case "all":
			*p = local.Preserve{Mode: on, Ownership: on, Timestamps: on,
				Links: on, XAttr: on, Context: on}
		default:
			return fmt.Errorf("invalid attribute %q "+
				"(want mode, ownership, timestamps, links, xattr, context or all)", item)
		}
	}
	return nil
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
