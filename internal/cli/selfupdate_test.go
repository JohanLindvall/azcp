// SPDX-License-Identifier: MIT

package cli

import "testing"

func TestUpdateOptOuts(t *testing.T) {
	t.Setenv("AZCP_NO_AUTO_UPDATE", "")
	t.Setenv("AZCP_NO_VERSION_CHECK", "")
	for _, tt := range []struct {
		flag          string
		check, update bool
	}{
		{"", true, false}, {"--no-auto-update", true, false}, {"--no-version-check", false, false},
	} {
		args := []string{"src", "dst"}
		if tt.flag != "" {
			args = append([]string{tt.flag}, args...)
		}
		o := mustParse(t, args...)
		if o.VersionCheck != tt.check || o.SelfUpdate != tt.update {
			t.Fatalf("%s: check %v update %v", tt.flag, o.VersionCheck, o.SelfUpdate)
		}
	}
	for _, value := range []string{"1", "true", "yes", "TRUE", "disable"} {
		t.Setenv("AZCP_NO_AUTO_UPDATE", value)
		t.Setenv("AZCP_NO_VERSION_CHECK", value)
		o := mustParse(t, "src", "dst")
		if o.SelfUpdate || o.VersionCheck {
			t.Fatalf("opt-out ignored: %q", value)
		}
	}
	for _, value := range []string{"", "0", "false", "no", "FALSE"} {
		t.Setenv("AZCP_NO_AUTO_UPDATE", value)
		t.Setenv("AZCP_NO_VERSION_CHECK", value)
		o := mustParse(t, "src", "dst")
		if o.SelfUpdate || !o.VersionCheck {
			t.Fatalf("false opt-out: %q", value)
		}
	}
	o := mustParse(t, "--tui", "--no-auto-update", "--no-version-check")
	if o.SelfUpdate || o.VersionCheck {
		t.Fatal("TUI lost opt-outs")
	}
	if o := mustParse(t, "--no-auto-update", "--update=older", "src", "dst"); o.Update != UpdateOlder {
		t.Fatal("cp --update changed")
	}
}

func TestSelfUpdateIsASeparateCommand(t *testing.T) {
	t.Setenv("AZCP_NO_VERSION_CHECK", "1")
	if o := mustParse(t, "--self-update"); !o.SelfUpdate || len(o.Sources) != 0 || o.Dest != "" {
		t.Fatalf("self-update parsed as a copy: %+v", o)
	}
	for _, args := range [][]string{
		{"--self-update", "src"}, {"--self-update", "src", "dst"},
		{"--self-update", "--dry-run"}, {"--tui", "--self-update"},
		{"--self-update", "--update"}, {"--self-update", "--files-from=-"},
		{"--self-update", "--bad"},
	} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, args := range [][]string{{"self-update", "dst"}, {"--", "--self-update", "dst"}} {
		if o := mustParse(t, args...); o.SelfUpdate || len(o.Sources) != 1 {
			t.Fatalf("ordinary operand became an update command: %v", args)
		}
	}
}
