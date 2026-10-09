package guard_test

import (
	"context"
	"errors"
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

// slugCases are shared by TestSlug and TestScriptMatchesSlug, so the hook
// script and Go agree on every form.
var slugCases = map[string]string{
	"https://github.com/Acme/Widget.git":           "acme/widget",
	"https://github.com/acme/widget/":              "acme/widget",
	"git@github.com:acme/widget.git":               "acme/widget",
	"ssh://git@github.com/acme/widget":             "acme/widget",
	"https://someone@github.com/acme/widget":       "acme/widget",
	"ssh://git@ssh.github.com:443/acme/widget.git": "acme/widget",
	"ssh://git@github.com:22/acme/widget":          "acme/widget",
	"https://github.com:443/acme/widget":           "acme/widget",
	"HTTP://GitHub.com:80/Acme/Widget/":            "acme/widget",
	"https://user:123@github.com/acme/widget.git":  "acme/widget",
	"git@github.com:12345/repo":                    "12345/repo",
	"git+ssh://git@github.com:22/acme/widget.git":  "acme/widget",
	"ssh+git://git@github.com:22/acme/widget":      "acme/widget",
	"git+ssh://git@github.com/acme/widget":         "acme/widget",
	"git://github.com:9418/acme/widget":            "acme/widget",
	"git+ssh://git@ssh.github.com:443/acme/widget": "acme/widget",
	"https://gitlab.com/acme/widget":               "",
	"https://gitlab.com:443/acme/widget":           "",
	"https://github.com/acme":                      "",
	"C:/Users/x/public.git":                        "",
}

func TestSlug(t *testing.T) {
	for in, want := range slugCases {
		got, ok := guard.Slug(in)
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
}

// pathWithout is PATH minus every directory that holds a ghostbranch binary.
func pathWithout(t *testing.T) string {
	t.Helper()
	var keep []string
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		found := false
		for _, n := range []string{"ghostbranch", "ghostbranch.exe"} {
			if _, err := os.Stat(filepath.Join(d, n)); err == nil {
				found = true
			}
		}
		if !found {
			keep = append(keep, d)
		}
	}
	return strings.Join(keep, string(os.PathListSeparator))
}

// TestScriptMatchesSlug runs the generated hook under sh: a push the script
// sees as public is refused (the binary does not exist), any other push goes
// through. The script must agree with Slug on every URL form, and also match
// by the remote names recorded at install time.
func TestScriptMatchesSlug(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not on PATH")
	}
	dir := t.TempDir()
	h := hook
	h.Binary = filepath.ToSlash(filepath.Join(dir, "no such dir", "ghostbranch.exe"))
	p := filepath.Join(dir, "pre-push")
	if err := os.WriteFile(p, []byte(guard.Script(h)), 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PATH="+pathWithout(t))
	runHook := func(remote, url string) (int, string) {
		cmd := exec.Command(sh, p, remote, url)
		cmd.Env = env
		cmd.Stdin = strings.NewReader("")
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), string(out)
		}
		if err != nil {
			t.Fatalf("run hook: %v", err)
		}
		return 0, string(out)
	}
	check := func(remote, url string, public bool) {
		t.Helper()
		code, out := runHook(remote, url)
		if public && (code != 1 || !strings.Contains(out, "cannot find the ghostbranch binary")) {
			t.Errorf("%s %q: want a refused public push, got exit %d: %s", remote, url, code, out)
		}
		if !public && code != 0 {
			t.Errorf("%s %q: want a non-public push to go through, got exit %d: %s", remote, url, code, out)
		}
	}
	for url, want := range slugCases {
		check("elsewhere", url, want == "acme/widget")
	}
	check("origin", "https://example.com/rewritten.git", true)
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
	res, err := guard.Install(ctx, c, hook, false)
	if err != nil || res.Action != "installed" {
		t.Fatalf("hooks dir outside the tree: %+v %v", res, err)
	}
	// git reports the resolved path; on macOS t.TempDir() sits under the
	// /var -> /private/var symlink, so compare both sides resolved.
	want, err := filepath.EvalSymlinks(filepath.FromSlash(outside))
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(res.Status.HooksDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("hooks dir = %q, want %q", got, want)
	}
}

func wantUnsupported(t *testing.T, what string, err error, mention string) {
	t.Helper()
	ce := contract.From(err)
	if ce == nil || ce.Code != contract.GuardUnsupported || !strings.Contains(err.Error(), mention) {
		t.Fatalf("%s: want GUARD_UNSUPPORTED mentioning %q, got %v", what, mention, err)
	}
}

// Husky runs hooks by their own file name and rewrites them on install, so
// chaining one would silently switch the user's checks off.
func TestInstallRefusesHusky(t *testing.T) {
	hermetic(t)
	ctx := context.Background()

	dir := newCheckout(t)
	c, err := guard.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	pre := filepath.Join(c.HooksDir, "pre-push")
	husky := "#!/usr/bin/env sh\n. \"$(dirname -- \"$0\")/_/HUSKY.sh\"\nnpm test\n"
	if err := os.MkdirAll(c.HooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pre, []byte(husky), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		_, err := guard.Install(ctx, c, hook, dry)
		wantUnsupported(t, "a hook that mentions husky", err, "Husky")
	}
	if b, _ := os.ReadFile(pre); string(b) != husky {
		t.Fatal("a refused install must leave the hook alone")
	}
	if _, err := os.Stat(filepath.Join(c.HooksDir, guard.ChainedName)); !os.IsNotExist(err) {
		t.Fatal("a refused install must not chain anything")
	}

	dir = newCheckout(t)
	gitIn(t, dir, "config", "core.hooksPath", ".husky/_")
	if err := os.MkdirAll(filepath.Join(dir, ".husky", "_"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".husky", "_", ".gitignore"), []byte("*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err = guard.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = guard.Install(ctx, c, hook, false)
	wantUnsupported(t, "a .husky hooks dir", err, "Husky")
	if _, err := os.Stat(filepath.Join(c.HooksDir, "pre-push")); !os.IsNotExist(err) {
		t.Fatal("a refused install must not write a hook")
	}
}

// A chained file with no pre-push hook is stale: installing would make an
// unknown file run as the user's hook.
func TestInstallRefusesAStaleChainedHook(t *testing.T) {
	hermetic(t)
	ctx := context.Background()
	c, err := guard.Open(ctx, newCheckout(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(c.HooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.HooksDir, guard.ChainedName), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = guard.Install(ctx, c, hook, false)
	wantUnsupported(t, "a stale chained hook", err, guard.ChainedName)
	if _, err := os.Stat(filepath.Join(c.HooksDir, "pre-push")); !os.IsNotExist(err) {
		t.Fatal("a refused install must not write a hook")
	}
}

// The checkout is read with plain git, as guard.Objects.
func TestCheckoutObjects(t *testing.T) {
	hermetic(t)
	ctx := context.Background()
	dir := newCheckout(t)
	first := gitIn(t, dir, "rev-parse", "HEAD")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "second")
	second := gitIn(t, dir, "rev-parse", "HEAD")
	c, err := guard.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	var objs guard.Objects = c
	if sha, ok, err := objs.Resolve(ctx, "HEAD"); err != nil || !ok || sha != second {
		t.Fatalf("present: %q %v %v", sha, ok, err)
	}
	if sha, ok, err := objs.Resolve(ctx, "1234567890123456789012345678901234567890"); err != nil || ok || sha != "" {
		t.Fatalf("missing: %q %v %v", sha, ok, err)
	}
	if in, err := objs.IsAncestor(ctx, first, second); err != nil || !in {
		t.Fatalf("first in second: %v %v", in, err)
	}
	if in, err := objs.IsAncestor(ctx, second, first); err != nil || in {
		t.Fatalf("second in first: %v %v", in, err)
	}
	if _, err := objs.IsAncestor(ctx, "1234567890123456789012345678901234567890", second); err == nil {
		t.Fatal("a missing object is an error for is-ancestor, not a no")
	}
}
