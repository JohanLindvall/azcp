// Package cli defines the command's interface: the option table, the
// configuration it produces, and the help text.
package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/JohanLindvall/azcp/internal/cpflags"
)

// Program identity, used in messages and in the SDK's user-agent string.
const Program = "azcp"

// Version is stamped at build time by the Makefile from `git describe`. The
// fallback is what `go install` and a plain `go build` produce.
var Version = "dev"

// N.B. the short options here are exactly GNU cp's, and no extension is given a
// short form that cp uses. -j is the one extension with a short form, because
// cp does not define it and every parallel tool spells it that way.
var specs = []cpflags.Spec{
	// --- GNU cp options -----------------------------------------------------
	{Long: "archive", Short: 'a', Arg: cpflags.NoArg,
		Help: "same as -dR --preserve=all"},
	{Long: "attributes-only", Arg: cpflags.NoArg,
		Help: "don't copy file data, just the attributes"},
	{Long: "backup", Short: 'b', Arg: cpflags.OptionalArg, Meta: "CONTROL",
		Help: "make a backup of each existing destination file"},
	{Long: "copy-contents", Arg: cpflags.NoArg,
		Help: "copy contents of special files when recursive"},
	{Short: 'd', Arg: cpflags.NoArg,
		Help: "same as --no-dereference --preserve=links"},
	{Long: "force", Short: 'f', Arg: cpflags.NoArg,
		Help: "remove an unwritable destination and try again"},
	{Long: "interactive", Short: 'i', Arg: cpflags.NoArg,
		Help: "prompt before overwrite"},
	{Short: 'H', Arg: cpflags.NoArg,
		Help: "follow symbolic links named on the command line"},
	{Long: "link", Short: 'l', Arg: cpflags.NoArg,
		Help: "hard link files instead of copying"},
	{Long: "dereference", Short: 'L', Arg: cpflags.NoArg,
		Help: "always follow symbolic links in SOURCE"},
	{Long: "no-clobber", Short: 'n', Arg: cpflags.NoArg,
		Help: "do not overwrite an existing file"},
	{Long: "no-dereference", Short: 'P', Arg: cpflags.NoArg,
		Help: "never follow symbolic links in SOURCE"},
	{Short: 'p', Arg: cpflags.NoArg,
		Help: "same as --preserve=mode,ownership,timestamps"},
	{Long: "preserve", Arg: cpflags.OptionalArg, Meta: "ATTR_LIST",
		Help: "preserve the given attributes (default: mode,ownership,timestamps)"},
	{Long: "no-preserve", Arg: cpflags.RequiredArg, Meta: "ATTR_LIST",
		Help: "don't preserve the given attributes"},
	{Long: "parents", Arg: cpflags.NoArg,
		Help: "use full source file name under DIRECTORY"},
	{Long: "recursive", Short: 'R', Alt: 'r', Arg: cpflags.NoArg,
		Help: "copy directories recursively"},
	{Short: 'r', Arg: cpflags.NoArg},
	{Long: "reflink", Arg: cpflags.OptionalArg, Meta: "WHEN",
		Help: "control copy-on-write cloning: always, auto, never"},
	{Long: "remove-destination", Arg: cpflags.NoArg,
		Help: "remove each destination file before opening it"},
	{Long: "sparse", Arg: cpflags.RequiredArg, Meta: "WHEN",
		Help: "control creation of sparse files: always, auto, never"},
	{Long: "strip-trailing-slashes", Arg: cpflags.NoArg,
		Help: "remove any trailing slashes from each SOURCE"},
	{Long: "symbolic-link", Short: 's', Arg: cpflags.NoArg,
		Help: "make symbolic links instead of copying"},
	{Long: "suffix", Short: 'S', Arg: cpflags.RequiredArg, Meta: "SUFFIX",
		Help: "override the usual backup suffix"},
	{Long: "target-directory", Short: 't', Arg: cpflags.RequiredArg, Meta: "DIRECTORY",
		Help: "copy all SOURCE arguments into DIRECTORY"},
	{Long: "no-target-directory", Short: 'T', Arg: cpflags.NoArg,
		Help: "treat DEST as a normal file"},
	{Long: "update", Short: 'u', Arg: cpflags.OptionalArg, Meta: "UPDATE",
		Help: "control which existing files are replaced: all, none, none-fail, older"},
	{Long: "verbose", Short: 'v', Arg: cpflags.NoArg,
		Help: "explain what is being done"},
	{Long: "one-file-system", Short: 'x', Arg: cpflags.NoArg,
		Help: "stay on this file system"},
	{Short: 'Z', Arg: cpflags.NoArg,
		Help: "set the SELinux security context of the destination"},
	{Long: "context", Arg: cpflags.OptionalArg, Meta: "CTX",
		Help: "like -Z, or set the context to CTX"},

	// --- extensions ---------------------------------------------------------
	{Long: "jobs", Short: 'j', Arg: cpflags.RequiredArg, Meta: "N",
		Help: "transfer up to N files at once " +
			"(default: scaled to the machine for network copies, 4 for local)"},
	{Long: "part-size", Arg: cpflags.RequiredArg, Meta: "SIZE",
		Help: "block size for multi-part blob transfers (default 8MiB)"},
	{Long: "part-concurrency", Arg: cpflags.RequiredArg, Meta: "N",
		Help: "blocks of one file to move at once (default 4)"},
	{Long: "retries", Arg: cpflags.RequiredArg, Meta: "N",
		Help: "attempts per network request before giving up (default 6)"},
	{Long: "retry-delay", Arg: cpflags.RequiredArg, Meta: "DUR",
		Help: "initial backoff between attempts (default 300ms)"},
	{Long: "retry-max-delay", Arg: cpflags.RequiredArg, Meta: "DUR",
		Help: "longest backoff between attempts (default 30s)"},
	{Long: "bwlimit", Arg: cpflags.RequiredArg, Meta: "RATE",
		Help: "cap throughput, in bytes per second (e.g. 10M); " +
			"does not apply to a server-side blob-to-blob copy"},
	{Long: "timeout", Arg: cpflags.RequiredArg, Meta: "DUR",
		Help: "bound on a single network request (default: none)"},
	{Long: "max-errors", Arg: cpflags.RequiredArg, Meta: "N",
		Help: "give up after N failed files; 0 means keep going (default 0)"},
	{Long: "progress", Arg: cpflags.RequiredArg, Meta: "WHEN",
		Help: "show the live display: auto, always, never"},
	{Long: "no-progress", Arg: cpflags.NoArg,
		Help: "same as --progress=never"},
	{Long: "progress-interval", Arg: cpflags.RequiredArg, Meta: "DUR",
		Help: "how often the live display repaints (default 1s)"},
	{Long: "log-level", Arg: cpflags.RequiredArg, Meta: "LEVEL",
		Help: "error, warn, info or debug (default warn)"},
	{Long: "log-format", Arg: cpflags.RequiredArg, Meta: "FORMAT",
		Help: "text or json (default text)"},
	{Long: "log-file", Arg: cpflags.RequiredArg, Meta: "PATH",
		Help: "append log records to PATH instead of stderr"},
	{Long: "exclude", Arg: cpflags.RequiredArg, Meta: "PATTERN",
		Help: "skip entries matching PATTERN; repeatable. A pattern without " +
			"a slash matches the name at any depth, one with a slash matches " +
			"the path relative to the copy root, and an excluded directory is " +
			"not descended into"},
	{Long: "include", Arg: cpflags.RequiredArg, Meta: "PATTERN",
		Help: "copy only entries matching PATTERN; repeatable. --exclude wins " +
			"where both match"},
	{Long: "dry-run", Arg: cpflags.NoArg,
		Help: "report what would be copied without copying it"},
	{Long: "glob", Arg: cpflags.RequiredArg, Meta: "WHEN",
		Help: "expand wildcards in arguments: auto, always, never"},
	{Long: "auth", Arg: cpflags.RequiredArg, Meta: "MODE",
		Help: "credential discovery: auto, identity, browser, device, anonymous"},
	{Long: "tenant", Arg: cpflags.RequiredArg, Meta: "ID",
		Help: "Microsoft Entra tenant to authenticate against"},
	{Long: "endpoint-suffix", Arg: cpflags.RequiredArg, Meta: "SUFFIX",
		Help: "storage endpoint suffix (default blob.core.windows.net)"},
	{Long: "create-container", Arg: cpflags.NoArg,
		Help: "create the destination container if it does not exist"},
	{Long: "content-type", Arg: cpflags.RequiredArg, Meta: "TYPE",
		Help: "set the blob content type instead of guessing it"},
	{Long: "put-md5", Arg: cpflags.NoArg,
		Help: "record a checksum on each uploaded blob, so it can be verified later"},
	{Long: "check-md5", Arg: cpflags.RequiredArg, Meta: "WHEN",
		Help: "verify a downloaded blob against its recorded checksum: " +
			"off, warn, fail (default), require"},
	{Long: "access-tier", Arg: cpflags.RequiredArg, Meta: "TIER",
		Help: "set the blob access tier: Hot, Cool, Cold or Archive"},

	{Long: "help", Arg: cpflags.NoArg, Help: "show this help and exit"},
	{Long: "version", Arg: cpflags.NoArg, Help: "show version information and exit"},
}

const usageHead = `Usage: azcp [OPTION]... [-T] SOURCE DEST
  or:  azcp [OPTION]... SOURCE... DIRECTORY
  or:  azcp [OPTION]... -t DIRECTORY SOURCE...

Copy files, locally or to and from Azure Blob Storage. Either side may be an
azure:// URL:

  azure://ACCOUNT.blob.core.windows.net/CONTAINER/PATH
  azure://ACCOUNT/CONTAINER/PATH                        (suffix filled in)

Credentials are found automatically: a SAS token in the URL, the AZURE_STORAGE_*
environment variables, a managed identity, or an existing "az login" session.

Arguments containing wildcards are expanded by azcp itself, which matters for
azure:// URLs the shell cannot see into. "**" descends through directories,
and extended patterns such as !(*.tmp) and @(a|b) are understood.
`

const usageTail = `
Sizes accept K, M, G and T suffixes (powers of 1024). Durations accept Go
syntax, e.g. 500ms, 30s, 2m.

Exit status is 0 on success, 1 if any file could not be copied, and 2 if the
command line itself was wrong.
`

// PrintUsage writes the help text.
func PrintUsage(w io.Writer) {
	fmt.Fprint(w, usageHead)
	fmt.Fprintln(w)

	var cpOpts, extOpts []cpflags.Spec
	ext := false
	for _, s := range specs {
		if s.Long == "jobs" {
			ext = true
		}
		if s.Help == "" {
			continue
		}
		if ext {
			extOpts = append(extOpts, s)
		} else {
			cpOpts = append(cpOpts, s)
		}
	}
	fmt.Fprintln(w, "Options compatible with cp:")
	writeOpts(w, cpOpts)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Additional options:")
	writeOpts(w, extOpts)
	fmt.Fprint(w, usageTail)
}

// Help is laid out in two columns at a fixed gutter, wrapping descriptions that
// would run past the right margin.
const (
	helpGutter = 30
	helpWidth  = 80
)

func writeOpts(w io.Writer, list []cpflags.Spec) {
	const gutter = helpGutter
	for _, s := range list {
		var left strings.Builder
		left.WriteString("  ")
		if s.Short != 0 {
			left.WriteString("-" + string(s.Short))
			if s.Alt != 0 {
				left.WriteString(", -" + string(s.Alt))
			}
			if s.Long != "" {
				left.WriteString(", ")
			}
		} else {
			left.WriteString("    ")
		}
		if s.Long != "" {
			left.WriteString("--" + s.Long)
			switch s.Arg {
			case cpflags.RequiredArg:
				left.WriteString("=" + meta(s))
			case cpflags.OptionalArg:
				left.WriteString("[=" + meta(s) + "]")
			}
		} else if s.Arg == cpflags.RequiredArg {
			left.WriteString(" " + meta(s))
		}
		indent := strings.Repeat(" ", gutter)
		lines := wrapText(s.Help, helpWidth-gutter)
		pad := gutter - left.Len()
		if pad < 1 {
			// The option itself fills the left column; start the description
			// on the next line rather than pushing the whole row out.
			fmt.Fprintf(w, "%s\n", left.String())
		} else {
			fmt.Fprintf(w, "%s%s%s\n", left.String(), strings.Repeat(" ", pad), lines[0])
			lines = lines[1:]
		}
		for _, l := range lines {
			fmt.Fprintf(w, "%s%s\n", indent, l)
		}
	}
}

// wrapText breaks s on spaces so no line exceeds width. It always returns at
// least one line.
func wrapText(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{""}
	}
	var out []string
	line := words[0]
	for _, word := range words[1:] {
		if len(line)+1+len(word) > width {
			out = append(out, line)
			line = word
			continue
		}
		line += " " + word
	}
	return append(out, line)
}

func meta(s cpflags.Spec) string {
	if s.Meta != "" {
		return s.Meta
	}
	return "VALUE"
}

// VersionText is printed by --version.
func VersionText() string {
	return fmt.Sprintf("%s %s\nA cp-compatible copier with Azure Blob Storage support.\n",
		Program, Version)
}
