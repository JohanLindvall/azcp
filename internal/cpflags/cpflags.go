// Package cpflags parses arguments the way GNU getopt_long does, because a
// drop-in replacement for cp has to accept everything a script might already be
// passing: clustered short options, attached and detached values, unambiguous
// long-option abbreviations, "--", and options written after the operands.
package cpflags

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// ArgKind says whether an option takes a value.
type ArgKind int

const (
	// NoArg options are switches.
	NoArg ArgKind = iota
	// RequiredArg values may be attached ("-j4", "--jobs=4") or the next word.
	RequiredArg
	// OptionalArg values must be attached ("-bt", "--backup=simple"). A
	// detached word is an operand, matching getopt_long.
	OptionalArg
)

// Spec declares one option.
type Spec struct {
	Long  string
	Short rune // zero when the option has no short form
	Arg   ArgKind
	// Help is the one-line description shown by --help. An empty Help hides
	// the option from the listing without making it unrecognised.
	Help string
	// Meta names the value in help output, e.g. "N" or "WHEN".
	Meta string
	// Alt is a second short form shown in help, for options cp spells two
	// ways (-R and -r). Parsing treats the two specs separately; this only
	// affects how they are listed.
	Alt rune
}

// Flag is one parsed occurrence of an option.
type Flag struct {
	Spec     Spec
	Value    string
	HasValue bool
}

// Name renders the option the way the user most likely wrote it, for error
// messages.
func (f Flag) Name() string {
	if f.Spec.Long != "" {
		return "--" + f.Spec.Long
	}
	return "-" + string(f.Spec.Short)
}

// Result holds the parse.
type Result struct {
	// Flags are in the order given, so a later --preserve can override an
	// earlier --no-preserve exactly as cp does.
	Flags    []Flag
	Operands []string
}

// Parse interprets argv (not including the program name).
func Parse(specs []Spec, argv []string) (*Result, error) {
	byShort := make(map[rune]Spec, len(specs))
	for _, s := range specs {
		if s.Short != 0 {
			byShort[s.Short] = s
		}
	}

	res := &Result{}
	// POSIXLY_CORRECT stops option parsing at the first operand; without it,
	// GNU permutes so that "cp a b -v" works.
	_, posix := os.LookupEnv("POSIXLY_CORRECT")

	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--":
			res.Operands = append(res.Operands, argv[i+1:]...)
			return res, nil

		case strings.HasPrefix(a, "--"):
			consumed, err := parseLong(specs, argv, i, res)
			if err != nil {
				return nil, err
			}
			i += consumed

		case len(a) > 1 && a[0] == '-':
			consumed, err := parseShort(byShort, argv, i, res)
			if err != nil {
				return nil, err
			}
			i += consumed

		default:
			// A bare "-" is an operand (cp reads it as a file name), as is
			// anything that does not start with a dash.
			res.Operands = append(res.Operands, a)
			if posix {
				res.Operands = append(res.Operands, argv[i+1:]...)
				return res, nil
			}
		}
	}
	return res, nil
}

// parseLong handles one --option, returning how many extra argv entries it ate.
func parseLong(specs []Spec, argv []string, i int, res *Result) (int, error) {
	name, val, hasVal := strings.Cut(argv[i][2:], "=")
	sp, err := matchLong(specs, name)
	if err != nil {
		return 0, err
	}
	extra := 0
	switch sp.Arg {
	case NoArg:
		if hasVal {
			return 0, fmt.Errorf("option '--%s' doesn't allow an argument", sp.Long)
		}
	case RequiredArg:
		if !hasVal {
			if i+1 >= len(argv) {
				return 0, fmt.Errorf("option '--%s' requires an argument", sp.Long)
			}
			val, hasVal, extra = argv[i+1], true, 1
		}
	case OptionalArg:
		// Only the attached form provides a value.
	}
	res.Flags = append(res.Flags, Flag{Spec: sp, Value: val, HasValue: hasVal})
	return extra, nil
}

// matchLong resolves a long option name, accepting any unambiguous prefix.
func matchLong(specs []Spec, name string) (Spec, error) {
	var partial []Spec
	for _, s := range specs {
		if s.Long == "" {
			continue
		}
		if s.Long == name {
			return s, nil
		}
		if strings.HasPrefix(s.Long, name) {
			partial = append(partial, s)
		}
	}
	switch len(partial) {
	case 1:
		return partial[0], nil
	case 0:
		return Spec{}, fmt.Errorf("unrecognized option '--%s'", name)
	default:
		names := make([]string, len(partial))
		for i, s := range partial {
			names[i] = "'--" + s.Long + "'"
		}
		return Spec{}, fmt.Errorf("option '--%s' is ambiguous; possibilities: %s",
			name, strings.Join(names, " "))
	}
}

// parseShort handles a cluster of short options.
func parseShort(byShort map[rune]Spec, argv []string, i int, res *Result) (int, error) {
	body := argv[i][1:]
	for k := 0; k < len(body); {
		r, size := utf8.DecodeRuneInString(body[k:])
		sp, ok := byShort[r]
		if !ok {
			return 0, fmt.Errorf("invalid option -- '%c'", r)
		}
		k += size
		switch sp.Arg {
		case NoArg:
			res.Flags = append(res.Flags, Flag{Spec: sp})
		case RequiredArg:
			if k < len(body) {
				res.Flags = append(res.Flags, Flag{Spec: sp, Value: body[k:], HasValue: true})
				return 0, nil
			}
			if i+1 >= len(argv) {
				return 0, fmt.Errorf("option requires an argument -- '%c'", r)
			}
			res.Flags = append(res.Flags, Flag{Spec: sp, Value: argv[i+1], HasValue: true})
			return 1, nil
		case OptionalArg:
			if k < len(body) {
				res.Flags = append(res.Flags, Flag{Spec: sp, Value: body[k:], HasValue: true})
				return 0, nil
			}
			res.Flags = append(res.Flags, Flag{Spec: sp})
		}
	}
	return 0, nil
}
