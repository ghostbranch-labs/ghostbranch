package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 6.4b-2: Homebrew links <prefix>/bin/ghostbranch to Cellar. The brew check
// no longer claims the bare prefix, so it relies on upgradeCommand
// resolving the link first. A pin: EvalSymlinks was already there.
func TestUpgradeCommandResolvesABrewLinkIntoCellar(t *testing.T) {
	t.Parallel()
	prefix := t.TempDir()
	target := filepath.Join(prefix, "Cellar", "ghostbranch", "0.1.0", "bin", "ghostbranch")
	link := filepath.Join(prefix, "bin", "ghostbranch")
	for _, d := range []string{filepath.Dir(target), filepath.Dir(link)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(target, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("can't make a symlink here: %v", err)
	}
	home := t.TempDir()
	a := &app{env: Env{
		Getenv:     func(string) string { return "" },
		Executable: func() (string, error) { return link, nil },
		UserHome:   func() (string, error) { return home, nil },
	}}
	if got := a.upgradeCommand("page"); got != "brew upgrade ghostbranch" {
		t.Fatalf("got %q, want brew upgrade ghostbranch", got)
	}
}

// The install script's folder is this OS's: the CLI passes runtime.GOOS.
func TestUpgradeCommandKnowsThisOSsInstallScriptFolder(t *testing.T) {
	t.Parallel()
	home, local := t.TempDir(), t.TempDir()
	exe := filepath.Join(home, ".local", "bin", "ghostbranch")
	want := "curl -fsSL https://ghostbranch.site/install.sh | sh"
	if runtime.GOOS == "windows" {
		exe = filepath.Join(local, "Programs", "ghostbranch", "ghostbranch.exe")
		want = "irm https://ghostbranch.site/install.ps1 | iex"
	}
	vars := map[string]string{"LOCALAPPDATA": local}
	a := &app{env: Env{
		Getenv:     func(k string) string { return vars[k] },
		Executable: func() (string, error) { return exe, nil },
		UserHome:   func() (string, error) { return home, nil },
	}}
	if got := a.upgradeCommand("page"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
