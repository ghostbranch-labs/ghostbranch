package updatecheck

import (
	"path/filepath"
	"runtime/debug"
	"strings"
)

// Placeholders until 6.4b publishes the packages; 6.4b fills them in.
const (
	// WingetID is the winget package ID. Placeholder (6.4b).
	WingetID = "GhostbranchLabs.Ghostbranch"
	// NPMPackage is the npm package that installs the binary. Placeholder (6.4b).
	NPMPackage = "ghostbranch"
	// InstallScriptDir is where the install script puts the binary; ""
	// matches nothing. Placeholder (6.4b).
	InstallScriptDir = ""
	// InstallScriptCommand upgrades an install-script install. Placeholder (6.4b).
	InstallScriptCommand = ""
)

// InstallEnv is what UpgradeCommand reads besides the path: the environment
// (GOBIN, GOPATH), the home directory, and the module path for go install.
type InstallEnv struct {
	Getenv func(string) string
	Home   string
	Module string
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
// upgrades it, or releasePage when no method is recognised (spec § 3.7, R9).
func UpgradeCommand(exe string, env InstallEnv, releasePage string) string {
	p := norm(exe)
	if p == "" {
		return releasePage
	}
	switch {
	case strings.Contains(p, "/node_modules/"):
		switch {
		case strings.Contains(p, "/_npx/"):
			return "npx " + NPMPackage + "@latest"
		case strings.Contains(p, "/.bun/"):
			return "bun update -g " + NPMPackage
		case strings.Contains(p, "/pnpm/"):
			return "pnpm update -g " + NPMPackage
		case strings.Contains(p, "/yarn/global/"):
			return "yarn global upgrade " + NPMPackage
		}
		return "npm update -g " + NPMPackage
	case strings.Contains(p, "/cellar/ghostbranch/"), strings.Contains(p, "/caskroom/ghostbranch/"),
		strings.HasPrefix(p, "/opt/homebrew/"), strings.HasPrefix(p, "/home/linuxbrew/.linuxbrew/"):
		return "brew upgrade ghostbranch"
	case strings.Contains(p, "/scoop/apps/ghostbranch/"):
		return "scoop update ghostbranch"
	case strings.Contains(p, "/winget/packages/"), strings.Contains(p, "/winget/links/"):
		return "winget upgrade --id " + WingetID
	case inGoBin(p, env):
		return "go install " + env.Module + "/cmd/ghostbranch@latest"
	case InstallScriptDir != "" && strings.HasPrefix(p, norm(InstallScriptDir)+"/"):
		return InstallScriptCommand
	}
	return releasePage
}

// inGoBin reports whether p sits directly in $GOBIN, or in a $GOPATH entry's
// bin, or (with no GOPATH) in ~/go/bin.
func inGoBin(p string, env InstallEnv) bool {
	getenv := env.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	var dirs []string
	if b := getenv("GOBIN"); b != "" {
		dirs = append(dirs, b)
	}
	if gp := getenv("GOPATH"); gp != "" {
		for _, d := range filepath.SplitList(gp) {
			dirs = append(dirs, d+"/bin")
		}
	} else if env.Home != "" {
		dirs = append(dirs, env.Home+"/go/bin")
	}
	dir := p[:strings.LastIndex(p, "/")+1]
	for _, d := range dirs {
		if dir == norm(d)+"/" {
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
	return "github.com/ghostbranch-labs/ghostbranch"
}
