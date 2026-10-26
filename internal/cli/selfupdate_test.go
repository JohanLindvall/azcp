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
		{"", true, true}, {"--no-auto-update", true, false}, {"--no-version-check", false, true},
	} {
		args := []string{"src", "dst"}
		if tt.flag != "" {
			args = append([]string{tt.flag}, args...)
		}
		o := mustParse(t, args...)
		if o.VersionCheck != tt.check || o.AutoUpdate != tt.update {
			t.Fatalf("%s: check %v update %v", tt.flag, o.VersionCheck, o.AutoUpdate)
		}
	}
	for _, value := range []string{"1", "true", "yes", "TRUE", "disable"} {
		t.Setenv("AZCP_NO_AUTO_UPDATE", value)
		t.Setenv("AZCP_NO_VERSION_CHECK", value)
		o := mustParse(t, "src", "dst")
		if o.AutoUpdate || o.VersionCheck {
			t.Fatalf("opt-out ignored: %q", value)
		}
	}
	for _, value := range []string{"", "0", "false", "no", "FALSE"} {
		t.Setenv("AZCP_NO_AUTO_UPDATE", value)
		t.Setenv("AZCP_NO_VERSION_CHECK", value)
		o := mustParse(t, "src", "dst")
		if !o.AutoUpdate || !o.VersionCheck {
			t.Fatalf("false opt-out: %q", value)
		}
	}
	o := mustParse(t, "--tui", "--no-auto-update", "--no-version-check")
	if o.AutoUpdate || o.VersionCheck {
		t.Fatal("TUI lost opt-outs")
	}
	if o := mustParse(t, "--no-auto-update", "--update=older", "src", "dst"); o.Update != UpdateOlder {
		t.Fatal("cp --update changed")
	}
}
