//go:build windows

package engine

import (
	"slices"
	"strings"
	"testing"
)

// F-28 on Windows (decision 3): a GHOSTBRANCH_HOME outside the profile may
// inherit a drive ACL that lets every local user read it. The check reads
// only the environment, never a real ACL.
func TestWindowsHomeWarning(t *testing.T) {
	t.Parallel()
	base := map[string]string{"USERPROFILE": `C:\Users\octo`, "USERDOMAIN": "PC", "USERNAME": "octo"}
	for _, c := range []struct {
		name, home string
		warn       bool
	}{
		{"the default home", "", false},
		{"a home under the profile", `C:\Users\octo\gb-home`, false},
		{"the profile itself, other case", `c:\users\OCTO`, false},
		{"another drive", `D:\Dev\projects\sandbox\gb-home`, true},
		{"a folder whose name starts like the profile", `C:\Users\octo2\gb`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			vars := map[string]string{"GHOSTBRANCH_HOME": c.home}
			for k, v := range base {
				vars[k] = v
			}
			msg, fix := windowsHomeWarning(func(k string) string { return vars[k] })
			if (msg != "") != c.warn {
				t.Fatalf("warn = %q", msg)
			}
			if !c.warn {
				return
			}
			line := `icacls "` + c.home + `" /inheritance:r /grant:r "PC\octo:(OI)(CI)F"`
			if !strings.Contains(msg, line) || !strings.Contains(msg, "move it under your profile") ||
				!slices.Equal(fix, []string{"icacls", c.home, "/inheritance:r", "/grant:r", `PC\octo:(OI)(CI)F`}) {
				t.Fatalf("message %q, fix %q", msg, fix)
			}
		})
	}
	// Without USERPROFILE the check can't tell, so it says nothing.
	if msg, _ := windowsHomeWarning(func(k string) string {
		return map[string]string{"GHOSTBRANCH_HOME": `D:\x`}[k]
	}); msg != "" {
		t.Fatalf("no profile: %q", msg)
	}
}
