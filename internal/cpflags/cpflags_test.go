package cpflags

import (
	"reflect"
	"strings"
	"testing"
)

var testSpecs = []Spec{
	{Long: "recursive", Short: 'r', Arg: NoArg},
	{Long: "verbose", Short: 'v', Arg: NoArg},
	{Long: "force", Short: 'f', Arg: NoArg},
	{Long: "target-directory", Short: 't', Arg: RequiredArg},
	{Long: "no-target-directory", Short: 'T', Arg: NoArg},
	{Long: "backup", Short: 'b', Arg: OptionalArg},
	{Long: "suffix", Short: 'S', Arg: RequiredArg},
	{Long: "preserve", Arg: OptionalArg},
	{Long: "no-preserve", Arg: RequiredArg},
	{Long: "no-clobber", Short: 'n', Arg: NoArg},
	{Long: "jobs", Short: 'j', Arg: RequiredArg},
}

func names(r *Result) []string {
	var out []string
	for _, f := range r.Flags {
		s := f.Spec.Long
		if f.HasValue {
			s += "=" + f.Value
		}
		out = append(out, s)
	}
	return out
}

func TestParse(t *testing.T) {
	cases := []struct {
		argv     []string
		flags    []string
		operands []string
	}{
		{[]string{"a", "b"}, nil, []string{"a", "b"}},
		{[]string{"-r", "a", "b"}, []string{"recursive"}, []string{"a", "b"}},
		{[]string{"-rvf", "a"}, []string{"recursive", "verbose", "force"}, []string{"a"}},
		{[]string{"a", "-v", "b"}, []string{"verbose"}, []string{"a", "b"}},
		{[]string{"-t", "dir", "a"}, []string{"target-directory=dir"}, []string{"a"}},
		{[]string{"-tdir", "a"}, []string{"target-directory=dir"}, []string{"a"}},
		{[]string{"-rt", "dir", "a"}, []string{"recursive", "target-directory=dir"}, []string{"a"}},
		{[]string{"--target-directory", "d", "a"}, []string{"target-directory=d"}, []string{"a"}},
		{[]string{"--target-directory=d", "a"}, []string{"target-directory=d"}, []string{"a"}},
		{[]string{"-b", "a"}, []string{"backup"}, []string{"a"}},
		{[]string{"-bsimple", "a"}, []string{"backup=simple"}, []string{"a"}},
		{[]string{"--backup", "a"}, []string{"backup"}, []string{"a"}},
		{[]string{"--backup=numbered", "a"}, []string{"backup=numbered"}, []string{"a"}},
		{[]string{"--recur", "a"}, []string{"recursive"}, []string{"a"}},
		{[]string{"--no-c", "a"}, []string{"no-clobber"}, []string{"a"}},
		{[]string{"--", "-r", "a"}, nil, []string{"-r", "a"}},
		{[]string{"-", "b"}, nil, []string{"-", "b"}},
		{[]string{"-j8", "a"}, []string{"jobs=8"}, []string{"a"}},
		{[]string{"--preserve", "a"}, []string{"preserve"}, []string{"a"}},
		{[]string{"--preserve=mode,ownership", "a"}, []string{"preserve=mode,ownership"}, []string{"a"}},
		{[]string{"--no-preserve", "mode", "a"}, []string{"no-preserve=mode"}, []string{"a"}},
	}
	for _, c := range cases {
		r, err := Parse(testSpecs, c.argv)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.argv, err)
			continue
		}
		if got := names(r); !reflect.DeepEqual(got, c.flags) {
			t.Errorf("Parse(%q) flags = %v, want %v", c.argv, got, c.flags)
		}
		if !reflect.DeepEqual(r.Operands, c.operands) {
			t.Errorf("Parse(%q) operands = %v, want %v", c.argv, r.Operands, c.operands)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string][]string{
		"invalid option -- 'q'":              {"-q"},
		"unrecognized option '--nope'":       {"--nope"},
		"requires an argument":               {"--target-directory"},
		"doesn't allow an argument":          {"--verbose=1"},
		"option '--no-' is ambiguous":        {"--no-"},
		"option requires an argument -- 't'": {"-t"},
	}
	for want, argv := range cases {
		_, err := Parse(testSpecs, argv)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) error = %v, want it to contain %q", argv, err, want)
		}
	}
}

func TestParseOrderPreserved(t *testing.T) {
	r, err := Parse(testSpecs, []string{"--no-preserve=mode", "--preserve=all", "a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	got := names(r)
	want := []string{"no-preserve=mode", "preserve=all"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}
