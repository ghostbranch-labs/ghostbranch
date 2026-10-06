package guard_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/guard"
)

// hermetic keeps the developer's own git config out of these tests.
func hermetic(t *testing.T) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newCheckout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	gitIn(t, dir, "remote", "add", "origin", "https://github.com/acme/widget.git")
	return dir
}

var hook = guard.Hook{Remotes: []string{"origin"}, Publics: []string{"acme/widget"}, Workspace: "default", Binary: "C:/Tools/O'Brien dir/ghostbranch.exe"}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/Acme/Widget.git":     "acme/widget",
		"https://github.com/acme/widget/":        "acme/widget",
		"git@github.com:acme/widget.git":         "acme/widget",
		"ssh://git@github.com/acme/widget":       "acme/widget",
		"https://someone@github.com/acme/widget": "acme/widget",
		"https://gitlab.com/acme/widget":         "",
		"https://github.com/acme":                "",
		"C:/Users/x/public.git":                  "",
	} {
		got, ok := guard.Slug(in)
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
}

// Review Focus 4: paths with spaces and apostrophes survive sh quoting.
func TestQuoting(t *testing.T) {
	for _, s := range []string{"plain", "with space", "O'Brien", "a'b'c d", ""} {
		if guard.UnSQ(guard.SQ(s)) != s {
			t.Errorf("round trip %q", s)
		}
		sh, err := exec.LookPath("sh")
		if err != nil {
			continue
		}
		out, err := exec.Command(sh, "-c", "printf '%s' "+guard.SQ(s)).Output()
		if err != nil || string(out) != s {
			t.Errorf("sh sees %q for %q (%v)", out, s, err)
		}
	}
	if !strings.Contains(guard.Script(hook), `GB_BIN='C:/Tools/O'\''Brien dir/ghostbranch.exe'`) {
		t.Fatal("binary path not quoted in the script")
	}
}

func TestOpenAndRemotes(t *testing.T) {
	hermetic(t)
	ctx := context.Background()
	if _, err := guard.Open(ctx, t.TempDir()); contract.From(err) == nil || contract.From(err).Code != contract.NotAPairCheckout {
		t.Fatalf("outside a checkout: %v", err)
	}
	dir := newCheckout(t)
	gitIn(t, dir, "config", "url./elsewhere.insteadOf", "https://github.com/acme/widget.git")
	c, err := guard.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.HooksDir != filepath.Join(c.GitDir, "hooks") || !filepath.IsAbs(c.HooksDir) {
		t.Fatalf("hooks dir = %q (git dir %q)", c.HooksDir, c.GitDir)
	}
	rs, err := c.Remotes(ctx)
	if err != nil || rs["origin"] != "https://github.com/acme/widget.git" {
		t.Fatalf("remotes must be the configured URLs, before insteadOf: %v %v", rs, err)
	}
}

// Review Focus 1 and 5: an existing hook is chained, kept on reinstall, never
// replaced by our own script, and restored on uninstall.
func TestInstallChainsAndUninstallRestores(t *testing.T) {
	hermetic(t)
	ctx := context.Background()
	dir := newCheckout(t)
	c, err := guard.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	pre := filepath.Join(c.HooksDir, "pre-push")
	chained := filepath.Join(c.HooksDir, guard.ChainedName)
	lfs := "#!/bin/sh\ngit lfs pre-push \"$@\"\n"
	if err := os.MkdirAll(c.HooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pre, []byte(lfs), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := guard.Install(ctx, c, hook, true)
	if err != nil || res.Action != "installed" || len(res.Preview.Ops) != 2 || res.Preview.Ops[0]["op"] != "move_file" {
		t.Fatalf("dry run = %+v %v", res, err)
	}
	if b, _ := os.ReadFile(pre); string(b) != lfs {
		t.Fatal("--dry-run changed the hook")
	}
	res, err = guard.Install(ctx, c, hook, false)
	if err != nil || res.Action != "installed" || !res.Status.Installed || !res.Status.Chained ||
		res.Status.Binary != hook.Binary || strings.Join(res.Status.Remotes, ",") != "origin" || strings.Join(res.Status.Publics, ",") != "acme/widget" {
		t.Fatalf("install = %+v %v", res, err)
	}
	if b, _ := os.ReadFile(chained); string(b) != lfs {
		t.Fatal("the existing hook must be kept as the chained hook")
	}
	if res, _ := guard.Install(ctx, c, hook, false); res.Action != "unchanged" {
		t.Fatalf("reinstall = %s", res.Action)
	}
	moved := hook
	moved.Binary = "/opt/new/ghostbranch"
	if res, _ := guard.Install(ctx, c, moved, false); res.Action != "updated" || res.Status.Binary != "/opt/new/ghostbranch" {
		t.Fatalf("upgrade = %+v", res)
	}
	if b, _ := os.ReadFile(chained); string(b) != lfs {
		t.Fatal("reinstalling must never chain the guard to itself")
	}
	res, err = guard.Uninstall(c, false)
	if err != nil || res.Action != "removed" || res.Status.Installed {
		t.Fatalf("uninstall = %+v %v", res, err)
	}
	if b, _ := os.ReadFile(pre); string(b) != lfs {
		t.Fatal("uninstall must restore the chained hook")
	}
	if _, err := os.Stat(chained); !os.IsNotExist(err) {
		t.Fatal("the chained copy must be gone")
	}
	if res, _ := guard.Uninstall(c, false); res.Action != "absent" {
		t.Fatalf("second uninstall = %s", res.Action)
	}
}

func TestInstallRefusesACommittableHooksDir(t *testing.T) {
	hermetic(t)
	ctx := context.Background()
	dir := newCheckout(t)
	gitIn(t, dir, "config", "core.hooksPath", "hooks")
	c, err := guard.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Install(ctx, c, hook, false); contract.From(err) == nil || contract.From(err).Code != contract.GuardUnsupported {
		t.Fatalf("tracked hooks dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("hooks/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := guard.Install(ctx, c, hook, false); err != nil || res.Action != "installed" {
		t.Fatalf("ignored hooks dir: %+v %v", res, err)
	}
	outside := filepath.ToSlash(t.TempDir())
	gitIn(t, dir, "config", "core.hooksPath", outside)
	c, _ = guard.Open(ctx, dir)
	if res, err := guard.Install(ctx, c, hook, false); err != nil || res.Status.HooksDir != filepath.Clean(filepath.FromSlash(outside)) {
		t.Fatalf("hooks dir outside the tree: %+v %v", res, err)
	}
}
