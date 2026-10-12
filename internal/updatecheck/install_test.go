package updatecheck_test

import (
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/updatecheck"
)

func TestUpgradeCommandPerInstallMethod(t *testing.T) {
	t.Parallel()
	const page = "https://github.com/ghostbranch-labs/ghostbranch/releases/tag/v0.13.0"
	const goInstall = "go install github.com/ghostbranch-labs/ghostbranch/cmd/ghostbranch@latest"
	vars := map[string]string{}
	env := updatecheck.InstallEnv{Getenv: func(k string) string { return vars[k] }, Home: "/home/ana", Module: "github.com/ghostbranch-labs/ghostbranch"}
	for exe, want := range map[string]string{
		// npm and friends: checked first, since Homebrew's and Scoop's node keep global packages under their prefixes (R9)
		"/usr/local/lib/node_modules/ghostbranch/bin/ghostbranch":                                       "npm update -g ghostbranch",
		"/opt/homebrew/lib/node_modules/@ghostbranch-labs/darwin-arm64/bin/ghostbranch":                 "npm update -g ghostbranch",
		`C:\Users\Ana\AppData\Roaming\npm\node_modules\@ghostbranch-labs\win32-x64\ghostbranch.exe`:     "npm update -g ghostbranch",
		`C:\Users\Ana\scoop\apps\nodejs\current\bin\node_modules\ghostbranch\bin\ghostbranch.exe`:       "npm update -g ghostbranch",
		"/home/ana/.local/share/pnpm/global/5/node_modules/@ghostbranch-labs/linux-x64/bin/ghostbranch": "pnpm update -g ghostbranch",
		"/home/ana/.bun/install/global/node_modules/@ghostbranch-labs/linux-x64/bin/ghostbranch":        "bun update -g ghostbranch",
		"/home/ana/.config/yarn/global/node_modules/@ghostbranch-labs/linux-x64/bin/ghostbranch":        "yarn global upgrade ghostbranch",
		"/home/ana/.npm/_npx/1a2b3c/node_modules/@ghostbranch-labs/linux-x64/bin/ghostbranch":           "npx ghostbranch@latest",
		// Homebrew
		"/opt/homebrew/Cellar/ghostbranch/0.13.0/bin/ghostbranch":              "brew upgrade ghostbranch",
		"/usr/local/Cellar/ghostbranch/0.13.0/bin/ghostbranch":                 "brew upgrade ghostbranch",
		"/home/linuxbrew/.linuxbrew/Cellar/ghostbranch/0.13.0/bin/ghostbranch": "brew upgrade ghostbranch",
		"/opt/homebrew/bin/ghostbranch":                                        "brew upgrade ghostbranch",
		// Scoop, winget
		`C:\Users\Ana\scoop\apps\ghostbranch\0.13.0\ghostbranch.exe`:                                         "scoop update ghostbranch",
		`C:\Users\Ana\AppData\Local\Microsoft\WinGet\Packages\GhostbranchLabs.Ghostbranch_x\ghostbranch.exe`: "winget upgrade --id " + updatecheck.WingetID,
		`C:\Users\Ana\AppData\Local\Microsoft\WinGet\Links\ghostbranch.exe`:                                  "winget upgrade --id " + updatecheck.WingetID,
		// go install, default GOPATH
		"/home/ana/go/bin/ghostbranch": goInstall,
		// anywhere else: the release page
		"/usr/local/bin/ghostbranch":               page,
		`D:\Dev\tools\ghostbranch\ghostbranch.exe`: page,
		"": page,
	} {
		if got := updatecheck.UpgradeCommand(exe, env, page); got != want {
			t.Errorf("%q: %q, want %q", exe, got, want)
		}
	}
}

func TestUpgradeCommandHonoursGOBINAndGOPATH(t *testing.T) {
	t.Parallel()
	const goInstall = "go install example.com/renamed/ghostbranch/cmd/ghostbranch@latest"
	for _, c := range []struct {
		vars map[string]string
		exe  string
	}{
		{map[string]string{"GOBIN": "/opt/gobin"}, "/opt/gobin/ghostbranch"},
		{map[string]string{"GOPATH": "/work/go"}, "/work/go/bin/ghostbranch"},
		{map[string]string{"GOBIN": `C:\Go\Bin`}, `c:\go\bin\ghostbranch.exe`},
	} {
		env := updatecheck.InstallEnv{Getenv: func(k string) string { return c.vars[k] }, Home: "/home/ana", Module: "example.com/renamed/ghostbranch"}
		if got := updatecheck.UpgradeCommand(c.exe, env, "page"); got != goInstall {
			t.Errorf("%v %q: %q", c.vars, c.exe, got)
		}
	}
	// With GOPATH set, ~/go/bin is no longer Go's bin directory.
	env := updatecheck.InstallEnv{Getenv: func(k string) string { return map[string]string{"GOPATH": "/work/go"}[k] }, Home: "/home/ana", Module: "m"}
	if got := updatecheck.UpgradeCommand("/home/ana/go/bin/ghostbranch", env, "page"); got != "page" {
		t.Errorf("stale default GOPATH matched: %q", got)
	}
}

// Under go test the main module is this repo's module, so Module returns its
// path whether it came from the build info or the fallback.
func TestModuleIsTheRepoModulePath(t *testing.T) {
	t.Parallel()
	if m, want := updatecheck.Module(), "github.com/"+updatecheck.Repo; m != want {
		t.Fatalf("Module() = %q, want %q", m, want)
	}
}
