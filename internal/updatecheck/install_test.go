package updatecheck_test

import (
	"path/filepath"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/updatecheck"
)

func TestUpgradeCommandPerInstallMethod(t *testing.T) {
	t.Parallel()
	const page = "https://github.com/ghostbranch-labs/ghostbranch/releases/tag/v0.13.0"
	const goInstall = "go install github.com/ghostbranch-labs/ghostbranch/cmd/ghostbranch@latest"
	vars := map[string]string{}
	env := updatecheck.InstallEnv{Getenv: func(k string) string { return vars[k] }, Home: "/home/ana", Module: "github.com/ghostbranch-labs/ghostbranch", GOOS: "linux"}
	for exe, want := range map[string]string{
		// npm and friends: checked first, since Homebrew's and Scoop's node keep global packages under their prefixes
		"/usr/local/lib/node_modules/ghostbranch/bin/ghostbranch":                                              "npm update -g ghostbranch",
		"/opt/homebrew/lib/node_modules/@ghostbranch-labs/darwin-arm64/bin/ghostbranch":                        "npm update -g ghostbranch",
		`C:\Users\Ana\AppData\Roaming\npm\node_modules\@ghostbranch-labs\win32-x64\ghostbranch.exe`:            "npm update -g ghostbranch",
		`C:\Users\Ana\scoop\apps\nodejs\current\bin\node_modules\ghostbranch\bin\ghostbranch.exe`:              "npm update -g ghostbranch",
		"/home/ana/.local/share/pnpm/global/5/node_modules/@ghostbranch-labs/linux-x64/bin/ghostbranch":        "pnpm update -g ghostbranch",
		"/home/ana/.bun/install/global/node_modules/@ghostbranch-labs/linux-x64/bin/ghostbranch":               "bun update -g ghostbranch",
		"/home/ana/.config/yarn/global/node_modules/@ghostbranch-labs/linux-x64/bin/ghostbranch":               "yarn global upgrade ghostbranch",
		"/home/ana/.npm/_npx/1a2b3c/node_modules/@ghostbranch-labs/linux-x64/bin/ghostbranch":                  "npx ghostbranch@latest",
		`C:\Users\Ana\AppData\Local\Yarn\Data\global\node_modules\@ghostbranch-labs\win32-x64\ghostbranch.exe`: "yarn global upgrade ghostbranch",
		// Homebrew
		"/opt/homebrew/Cellar/ghostbranch/0.13.0/bin/ghostbranch":              "brew upgrade ghostbranch",
		"/usr/local/Cellar/ghostbranch/0.13.0/bin/ghostbranch":                 "brew upgrade ghostbranch",
		"/home/linuxbrew/.linuxbrew/Cellar/ghostbranch/0.13.0/bin/ghostbranch": "brew upgrade ghostbranch",
		"/opt/homebrew/Caskroom/ghostbranch/0.13.0/ghostbranch":                "brew upgrade ghostbranch",
		// Under the prefix but not Homebrew's: copied there by hand (6.4b-2).
		// A real brew link resolves into Cellar before this is asked.
		"/opt/homebrew/bin/ghostbranch":               page,
		"/home/linuxbrew/.linuxbrew/bin/ghostbranch":  page,
		"/opt/homebrew/opt/something/bin/ghostbranch": page,
		// Scoop, winget
		`C:\Users\Ana\scoop\apps\ghostbranch\0.13.0\ghostbranch.exe`:                                         "scoop update ghostbranch",
		`C:\Users\Ana\AppData\Local\Microsoft\WinGet\Packages\GhostbranchLabs.Ghostbranch_x\ghostbranch.exe`: "winget upgrade --id " + updatecheck.WingetID,
		`C:\Users\Ana\AppData\Local\Microsoft\WinGet\Links\ghostbranch.exe`:                                  "winget upgrade --id " + updatecheck.WingetID,
		// Scoop's global install (6.4b-2)
		`C:\ProgramData\scoop\apps\ghostbranch\current\ghostbranch.exe`: "scoop update ghostbranch",
		// winget, machine scope (6.4b-2)
		`C:\Program Files\WinGet\Packages\GhostbranchLabs.Ghostbranch_Microsoft.Winget.Source_8wekyb3d8bbwe\ghostbranch.exe`: "winget upgrade --id " + updatecheck.WingetID,
		// go install, default GOPATH
		"/home/ana/go/bin/ghostbranch": goInstall,
		// the install script's default folder (6.4b-2)
		"/home/ana/.local/bin/ghostbranch":     "curl -fsSL https://ghostbranch.site/install.sh | sh",
		"/home/ana/.local/bin/sub/ghostbranch": page,
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
	sep := string(filepath.ListSeparator)
	for _, c := range []struct {
		vars map[string]string
		exe  string
	}{
		{map[string]string{"GOBIN": "/opt/gobin"}, "/opt/gobin/ghostbranch"},
		{map[string]string{"GOPATH": "/work/go"}, "/work/go/bin/ghostbranch"},
		{map[string]string{"GOPATH": "/work/go/"}, "/work/go/bin/ghostbranch"},
		{map[string]string{"GOPATH": "/a" + sep + sep + "/work/go"}, "/work/go/bin/ghostbranch"},
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

func TestUpgradeCommandIgnoresEmptyGOPATHEntries(t *testing.T) {
	t.Parallel()
	sep := string(filepath.ListSeparator)
	vars := map[string]string{"GOPATH": "/a" + sep + sep + "/b"}
	env := updatecheck.InstallEnv{Getenv: func(k string) string { return vars[k] }, Home: "/home/ana", Module: "m"}
	if got := updatecheck.UpgradeCommand("/bin/ghostbranch", env, "page"); got != "page" {
		t.Errorf("empty GOPATH entry matched /bin: %q", got)
	}
}

func TestUpgradeCommandPerNPMManager(t *testing.T) {
	t.Parallel()
	env := updatecheck.InstallEnv{Home: "/home/ana", Module: "m", GOOS: "linux"}
	for exe, want := range map[string]string{
		// global npm: <prefix>/lib/node_modules, or <prefix>/node_modules on Windows
		"/usr/local/lib/node_modules/ghostbranch/node_modules/@ghostbranch/cli-linux-x64/bin/ghostbranch":                                  "npm update -g ghostbranch",
		"/home/ana/.nvm/versions/node/v22.11.0/lib/node_modules/ghostbranch/node_modules/@ghostbranch/cli-linux-x64/bin/ghostbranch":       "npm update -g ghostbranch",
		`C:\Program Files\nodejs\node_modules\ghostbranch\node_modules\@ghostbranch\cli-win32-x64\bin\ghostbranch.exe`:                     "npm update -g ghostbranch",
		`C:\Users\Ana\AppData\Local\nvm\v22.20.0\node_modules\@ghostbranch\cli-win32-x64\bin\ghostbranch.exe`:                              "npm update -g ghostbranch",
		`C:\Users\Ana\AppData\Roaming\fnm\node-versions\v22.11.0\installation\node_modules\@ghostbranch\cli-win32-x64\bin\ghostbranch.exe`: "npm update -g ghostbranch",
		// npm hoists the platform package next to ghostbranch in the global folder
		`C:\Users\Ana\AppData\Roaming\npm\node_modules\@ghostbranch\cli-win32-x64\bin\ghostbranch.exe`: "npm update -g ghostbranch",
		// a project's dependency: no -g, and @latest so a 0.x minor release (outside ^0.y.z) upgrades
		"/home/ana/src/app/node_modules/@ghostbranch/cli-linux-x64/bin/ghostbranch": "npm install ghostbranch@latest",
		`D:\work\app\node_modules\@ghostbranch\cli-win32-x64\bin\ghostbranch.exe`:   "npm install ghostbranch@latest",
		// pnpm: the global store, or a project's .pnpm folder
		"/home/ana/.local/share/pnpm/global/5/node_modules/.pnpm/@ghostbranch+cli-linux-x64@0.1.0/node_modules/@ghostbranch/cli-linux-x64/bin/ghostbranch":         "pnpm update -g ghostbranch",
		`C:\Users\Ana\AppData\Local\pnpm\global\5\node_modules\.pnpm\@ghostbranch+cli-win32-x64@0.1.0\node_modules\@ghostbranch\cli-win32-x64\bin\ghostbranch.exe`: "pnpm update -g ghostbranch",
		"/home/ana/src/app/node_modules/.pnpm/@ghostbranch+cli-linux-x64@0.1.0/node_modules/@ghostbranch/cli-linux-x64/bin/ghostbranch":                            "pnpm add ghostbranch@latest",
		// Volta, on Unix and Windows
		"/home/ana/.volta/tools/image/packages/ghostbranch/lib/node_modules/ghostbranch/node_modules/@ghostbranch/cli-linux-x64/bin/ghostbranch":                 "volta install ghostbranch",
		`C:\Users\Ana\AppData\Local\Volta\tools\image\packages\ghostbranch\node_modules\ghostbranch\node_modules\@ghostbranch\cli-win32-x64\bin\ghostbranch.exe`: "volta install ghostbranch",
		// unchanged: npx, bun, yarn
		"/home/ana/.npm/_npx/1a2b3c/node_modules/@ghostbranch/cli-linux-x64/bin/ghostbranch":    "npx ghostbranch@latest",
		"/home/ana/.bun/install/global/node_modules/@ghostbranch/cli-linux-x64/bin/ghostbranch": "bun update -g ghostbranch",
		"/home/ana/.config/yarn/global/node_modules/@ghostbranch/cli-linux-x64/bin/ghostbranch": "yarn global upgrade ghostbranch",
	} {
		if got := updatecheck.UpgradeCommand(exe, env, "page"); got != want {
			t.Errorf("%q: %q, want %q", exe, got, want)
		}
	}
}

func TestUpgradeCommandOnWindows(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"LOCALAPPDATA": `C:\Users\Ana\AppData\Local`}
	env := updatecheck.InstallEnv{Getenv: func(k string) string { return vars[k] }, Home: `C:\Users\Ana`, Module: "github.com/ghostbranch-labs/ghostbranch", GOOS: "windows"}
	for exe, want := range map[string]string{
		// install.ps1's default folder
		`C:\Users\Ana\AppData\Local\Programs\ghostbranch\ghostbranch.exe`: "irm https://ghostbranch.site/install.ps1 | iex",
		`c:\users\ana\appdata\local\programs\ghostbranch\ghostbranch.exe`: "irm https://ghostbranch.site/install.ps1 | iex",
		// the state folder is not the install folder
		`C:\Users\Ana\AppData\Local\ghostbranch\ghostbranch.exe`: "page",
		// Windows Home's default GOPATH, %USERPROFILE%\go (6.4b-2)
		`C:\Users\Ana\go\bin\ghostbranch.exe`: "go install github.com/ghostbranch-labs/ghostbranch/cmd/ghostbranch@latest",
		// ~/.local/bin is a Unix folder
		`C:\Users\Ana\.local\bin\ghostbranch.exe`: "page",
	} {
		if got := updatecheck.UpgradeCommand(exe, env, "page"); got != want {
			t.Errorf("%q: %q, want %q", exe, got, want)
		}
	}
}

func TestInstallScriptDirAndCommand(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"LOCALAPPDATA": `C:\Users\Ana\AppData\Local\`}
	get := func(k string) string { return vars[k] }
	for _, c := range []struct {
		env      updatecheck.InstallEnv
		dir, cmd string
	}{
		{updatecheck.InstallEnv{Home: "/home/ana", GOOS: "linux"}, "/home/ana/.local/bin", "curl -fsSL https://ghostbranch.site/install.sh | sh"},
		{updatecheck.InstallEnv{Home: "/Users/ana/", GOOS: "darwin"}, "/Users/ana/.local/bin", "curl -fsSL https://ghostbranch.site/install.sh | sh"},
		{updatecheck.InstallEnv{GOOS: "linux"}, "", "curl -fsSL https://ghostbranch.site/install.sh | sh"},
		{updatecheck.InstallEnv{Getenv: get, Home: `C:\Users\Ana`, GOOS: "windows"}, `C:\Users\Ana\AppData\Local\Programs\ghostbranch`, "irm https://ghostbranch.site/install.ps1 | iex"},
		{updatecheck.InstallEnv{Home: `C:\Users\Ana`, GOOS: "windows"}, "", "irm https://ghostbranch.site/install.ps1 | iex"},
	} {
		if got := updatecheck.InstallScriptDir(c.env); got != c.dir {
			t.Errorf("%+v: dir %q, want %q", c.env, got, c.dir)
		}
		if got := updatecheck.InstallScriptCommand(c.env.GOOS); got != c.cmd {
			t.Errorf("%s: command %q, want %q", c.env.GOOS, got, c.cmd)
		}
	}
	// With no home, the script's folder is unknown and matches nothing.
	if got := updatecheck.UpgradeCommand("/ghostbranch", updatecheck.InstallEnv{GOOS: "linux"}, "page"); got != "page" {
		t.Errorf("no home: %q", got)
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
