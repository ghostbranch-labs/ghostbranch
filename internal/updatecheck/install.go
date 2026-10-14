package updatecheck

import (
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
)

const (
	// WingetID is the winget package ID (submitted after launch).
	WingetID = "GhostbranchLabs.Ghostbranch"
	// NPMPackage is the npm package that installs the binary.
	NPMPackage = "ghostbranch"
)

// InstallEnv is what UpgradeCommand reads besides the path: the environment
// (GOBIN, GOPATH, LOCALAPPDATA), the home directory, the module path for go
// install, and the OS (runtime.GOOS), which picks the install script.
type InstallEnv struct {
	Getenv func(string) string
	Home   string
	Module string
	GOOS   string
}

func (e InstallEnv) getenv(k string) string {
	if e.Getenv == nil {
		return ""
	}
	return e.Getenv(k)
}

// InstallScriptDir is the folder the install scripts put the binary in by
// default: %LOCALAPPDATA%\Programs\ghostbranch on Windows, ~/.local/bin
// elsewhere; "" when that isn't known. A binary installed with
// GHOSTBRANCH_INSTALL_DIR somewhere else gets the release page instead.
func InstallScriptDir(env InstallEnv) string {
	if env.GOOS == "windows" {
		if l := env.getenv("LOCALAPPDATA"); l != "" {
			return strings.TrimRight(l, `\/`) + `\Programs\ghostbranch`
		}
		return ""
	}
	if env.Home == "" {
		return ""
	}
	return strings.TrimRight(env.Home, "/") + "/.local/bin"
}

// InstallScriptCommand re-runs the install script, which upgrades it.
func InstallScriptCommand(goos string) string {
	if goos == "windows" {
		return "irm https://ghostbranch.site/install.ps1 | iex"
	}
	return "curl -fsSL https://ghostbranch.site/install.sh | sh"
}

// norm makes a path comparable across shapes: forward slashes, lower case,
// no trailing slash. Windows and macOS paths are case-insensitive; on Linux
// lowering only risks a false match between two directories that differ by
// case, which would show a wrong but harmless hint.
func norm(p string) string {
	return strings.TrimRight(strings.ToLower(strings.ReplaceAll(p, `\`, "/")), "/")
}

// UpgradeCommand works out how this binary was installed from its path
// (os.Executable with symlinks resolved) and returns the command that
// upgrades it, or releasePage when no method is recognised.
func UpgradeCommand(exe string, env InstallEnv, releasePage string) string {
	p := norm(exe)
	if p == "" {
		return releasePage
	}
	switch {
	case strings.Contains(p, "/node_modules/"):
		return npmCommand(p)
	// Homebrew's own folders only: its bin/ links resolve into Cellar, and a
	// binary copied by hand under the prefix isn't Homebrew's (6.4b-2).
	case strings.Contains(p, "/cellar/ghostbranch/"), strings.Contains(p, "/caskroom/ghostbranch/"):
		return "brew upgrade ghostbranch"
	case strings.Contains(p, "/scoop/apps/ghostbranch/"):
		return "scoop update ghostbranch"
	case strings.Contains(p, "/winget/packages/"), strings.Contains(p, "/winget/links/"):
		return "winget upgrade --id " + WingetID
	case inGoBin(p, env):
		return "go install " + env.Module + "/cmd/ghostbranch@latest"
	case inDir(p, InstallScriptDir(env)):
		return InstallScriptCommand(env.GOOS)
	}
	return releasePage
}

// npmCommand is the upgrade command for a binary under node_modules, by the
// package manager that put it there. npx, Volta, bun, pnpm and yarn are
// told apart by their folders; then a global npm install, and last a
// project's own node_modules, which upgrades without -g.
func npmCommand(p string) string {
	switch {
	case strings.Contains(p, "/_npx/"):
		return "npx " + NPMPackage + "@latest"
	case strings.Contains(p, "/.volta/"), strings.Contains(p, "/volta/tools/image/"):
		return "volta install " + NPMPackage
	case strings.Contains(p, "/.bun/"):
		return "bun update -g " + NPMPackage
	case strings.Contains(p, "/pnpm/global/"):
		return "pnpm update -g " + NPMPackage
	case strings.Contains(p, "/.pnpm/"):
		return "pnpm update " + NPMPackage
	case strings.Contains(p, "/yarn/global/"), strings.Contains(p, "/yarn/data/global/"):
		return "yarn global upgrade " + NPMPackage
	case npmGlobal(p):
		return "npm update -g " + NPMPackage
	}
	return "npm update " + NPMPackage
}

// npmGlobal reports whether p's first node_modules is npm's global one. npm
// keeps global packages in <prefix>/lib/node_modules on macOS and Linux, and
// in <prefix>/node_modules on Windows, where the prefix is %APPDATA%\npm,
// the Node folder (nodejs, or Scoop's nodejs\current\bin), or a version
// manager's per-version folder (nvm, fnm's "installation"). A project's
// node_modules sits in the project's folder instead.
func npmGlobal(p string) bool {
	parent := p[:strings.Index(p, "/node_modules/")]
	switch path.Base(parent) {
	case "lib", "npm", "nodejs", "bin", "installation":
		return true
	}
	return strings.Contains(parent, "/nvm/")
}

// inDir reports whether p sits directly in dir; an empty dir matches nothing.
func inDir(p, dir string) bool {
	if dir == "" {
		return false
	}
	return path.Dir(p) == path.Clean(norm(dir))
}

// inGoBin reports whether p sits directly in $GOBIN, or in a $GOPATH entry's
// bin, or (with no GOPATH) in ~/go/bin.
func inGoBin(p string, env InstallEnv) bool {
	var dirs []string
	if b := env.getenv("GOBIN"); b != "" {
		dirs = append(dirs, b)
	}
	if gp := env.getenv("GOPATH"); gp != "" {
		for _, d := range filepath.SplitList(gp) {
			if d != "" {
				dirs = append(dirs, norm(d)+"/bin")
			}
		}
	} else if env.Home != "" {
		dirs = append(dirs, norm(env.Home)+"/go/bin")
	}
	for _, d := range dirs {
		if inDir(p, d) {
			return true
		}
	}
	return false
}

// Module is this binary's module path from its build info, so the go install
// line follows a rename; the current path when build info is missing.
func Module() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Path != "" {
		return bi.Main.Path
	}
	return "github.com/" + Repo
}
