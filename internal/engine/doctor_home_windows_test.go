//go:build windows

package engine

import (
	"slices"
	"strings"
	"testing"
)

// F-28 on Windows (decision 3): a GHOSTBRANCH_HOME outside the profile may
// inherit a drive ACL that lets every local user read it. The check reads
// only the environment and the home's absolute path, never a real ACL, so
// its message says the warning stays after the fix.
func TestWindowsHomeWarning(t *testing.T) {
	t.Parallel()
	base := map[string]string{"USERPROFILE": `C:\Users\octo`, "USERDOMAIN": "PC", "USERNAME": "octo"}
	for _, c := range []struct {
		name, env, home string
		warn            bool
	}{
		{"the default home", "", `C:\Users\octo\AppData\Local\ghostbranch`, false},
		{"a home under the profile", `C:\Users\octo\gb-home`, `C:\Users\octo\gb-home`, false},
		{"the profile itself, other case", `c:\users\OCTO`, `c:\users\OCTO`, false},
		{"a relative home that resolves under the profile", `gb`, `C:\Users\octo\work\gb`, false},
		{"another drive", `D:\gb-home`, `D:\gb-home`, true},
		{"a relative home that resolves to another drive", `gb`, `D:\work\gb`, true},
		{"a folder whose name starts like the profile", `C:\Users\octo2\gb`, `C:\Users\octo2\gb`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			vars := map[string]string{"GHOSTBRANCH_HOME": c.env}
			for k, v := range base {
				vars[k] = v
			}
			msg, fix := windowsHomeWarning(func(k string) string { return vars[k] }, c.home)
			if (msg != "") != c.warn {
				t.Fatalf("warn = %q", msg)
			}
			if !c.warn {
				return
			}
			line := `icacls "` + c.home + `" /inheritance:r /grant:r "PC\octo:(OI)(CI)F"`
			if !strings.Contains(msg, line) || !strings.Contains(msg, "move it under your profile") ||
				!strings.Contains(msg, "doctor can't read the folder's permissions, so this warning stays while the home is outside your profile") ||
				!slices.Equal(fix, []string{"icacls", c.home, "/inheritance:r", "/grant:r", `PC\octo:(OI)(CI)F`}) {
				t.Fatalf("message %q, fix %q", msg, fix)
			}
		})
	}
	// Without USERPROFILE the check can't tell, so it says nothing.
	if msg, _ := windowsHomeWarning(func(k string) string {
		return map[string]string{"GHOSTBRANCH_HOME": `D:\x`}[k]
	}, `D:\x`); msg != "" {
		t.Fatalf("no profile: %q", msg)
	}
}
