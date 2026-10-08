// Package blackbox tests the real ghostbranch binary from outside: the way an
// agent's shell and git's hooks actually run it.
package blackbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

var (
	bin string
	exe = map[bool]string{true: ".exe", false: ""}[runtime.GOOS == "windows"]
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ghostbranch-blackbox-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bin = filepath.Join(dir, "ghostbranch"+exe)
	goTool, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintln(os.Stderr, "blackbox tests need the go tool on PATH:", err)
		os.Exit(1)
	}
	build := exec.Command(goTool, "build", "-o", bin, "github.com/ghostbranch-labs/ghostbranch/cmd/ghostbranch")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build the binary: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// gitEnv keeps the developer's git config out, and sets an identity.
func gitEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return append(append(os.Environ(), "GIT_CONFIG_GLOBAL="+cfg, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"), extra...)
}

func run(t *testing.T, env []string, dir, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func must(t *testing.T, env []string, dir, name string, args ...string) string {
	t.Helper()
	out, err := run(t, env, dir, name, args...)
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(out)
}

// workspace registers acme/widget in a fresh GHOSTBRANCH_HOME.
func workspace(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "workspaces", "default")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := `{"schema_version": "0.5", "pairs": [{"public": "acme/widget", "shadow": "acme/widget-shadow"}]}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "workspace.json"), []byte(ws), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

// An agent's shell: stdin is a pipe (here with `yes` in it) or the null
// device. The binary must refuse with exit 5 at once, never wait for a
// console nobody can see.
func TestAnAgentShellCannotGrantItself(t *testing.T) {
	home := workspace(t)
	stdins := map[string]func() (*os.File, error){
		"yes piped in": func() (*os.File, error) {
			r, w, err := os.Pipe()
			if err != nil {
				return nil, err
			}
			_, _ = w.WriteString("yes\nyes\nyes\n")
			return r, w.Close()
		},
		"null device": func() (*os.File, error) { return os.Open(os.DevNull) },
	}
	for name, open := range stdins {
		t.Run(name, func(t *testing.T) {
			in, err := open()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = in.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "agents", "allow", "--repo", "acme/widget", "--json")
			cmd.Stdin = in
			cmd.Env = append(os.Environ(), "GHOSTBRANCH_HOME="+home, "GHOSTBRANCH_WORKSPACE=default",
				"GHOSTBRANCH_TOKEN=unused", "GHOSTBRANCH_ACTOR=agent:blackbox")
			out, _ := cmd.Output()
			if ctx.Err() != nil {
				t.Fatal("the binary waited for a console")
			}
			var env struct {
				ExitCode int `json:"exit_code"`
				Error    struct {
					Code    string         `json:"code"`
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(out, &env); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, out)
			}
			if env.ExitCode != 5 || cmd.ProcessState.ExitCode() != 5 || env.Error.Code != "APPROVAL_REQUIRED" || env.Error.Details["reason"] != "no_console" {
				t.Fatalf("%s", out)
			}
			if _, err := os.Stat(filepath.Join(home, "workspaces", "default", "grants.json")); !os.IsNotExist(err) {
				t.Fatal("a grant was written")
			}
		})
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

// The guard as git runs it: real pushes, a public remote reached through an
// insteadOf rewrite, an existing pre-push hook, and a binary path with a
// space and an apostrophe.
func TestGuardHookEndToEnd(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := workspace(t)
	env := gitEnv(t, "GHOSTBRANCH_HOME="+home, "GHOSTBRANCH_WORKSPACE=default")

	// The public repo, and feature work that exists only in the shadow repo.
	public := testutil.BareRepo(t, filepath.Join(root, "public.git"))
	m := testutil.Commit(t, public, "main", "public main", map[string]string{"README.md": "hi\n"})
	shadow := testutil.BareRepo(t, filepath.Join(root, "shadow.git"))
	must(t, env, root, "git", "--git-dir", shadow.Dir, "fetch", "-q", public.Dir, "refs/heads/main:refs/heads/main")
	if err := shadow.UpdateRef(ctx, "refs/heads/feat-x", m); err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, shadow, "feat-x", "unreleased", map[string]string{"feature.txt": "secret\n"})

	// The engine's mirror for the pair, as sync and feature start leave it.
	mirror := testutil.BareRepo(t, filepath.Join(home, "workspaces", "default", "pairs", "acme__widget", "mirror.git"))
	must(t, env, root, "git", "--git-dir", mirror.Dir, "fetch", "-q", shadow.Dir,
		"refs/heads/main:refs/remotes/public/main", "refs/heads/feat-x:refs/remotes/shadow/feat-x")
	if err := mirror.UpdateRef(ctx, "refs/shadow-meta/features/feat-x", m); err != nil {
		t.Fatal(err)
	}

	// The user's checkout: origin is the public repo by its GitHub URL,
	// rewritten to the local path; shadow is a plain local remote.
	co := filepath.Join(root, "checkout")
	must(t, env, root, "git", "clone", "-q", public.Dir, co)
	must(t, env, co, "git", "remote", "set-url", "origin", "https://github.com/acme/widget.git")
	must(t, env, co, "git", "config", "url."+filepath.ToSlash(public.Dir)+".insteadOf", "https://github.com/acme/widget.git")
	must(t, env, co, "git", "remote", "add", "shadow", shadow.Dir)
	must(t, env, co, "git", "fetch", "-q", "shadow", "feat-x:feat-x")
	must(t, env, co, "git", "commit", "-q", "--allow-empty", "-m", "public fix")
	must(t, env, co, "git", "switch", "-q", "feat-x")
	must(t, env, co, "git", "commit", "-q", "--allow-empty", "-m", "more feature work")
	featTip := must(t, env, co, "git", "rev-parse", "HEAD")
	must(t, env, co, "git", "switch", "-q", "main")
	mainTip := must(t, env, co, "git", "rev-parse", "HEAD")

	// Review Focus 1: an existing pre-push hook, like Git LFS, that logs its input.
	logf := filepath.ToSlash(filepath.Join(root, "lfs.log"))
	lfs := "#!/bin/sh\necho \"lfs $1\" >> '" + logf + "'\ncat >> '" + logf + "'\n"
	if err := os.WriteFile(filepath.Join(co, ".git", "hooks", "pre-push"), []byte(lfs), 0o755); err != nil {
		t.Fatal(err)
	}

	// Review Focus 4: install from a binary whose path has a space and an apostrophe.
	odd := filepath.Join(root, "O'Brien tools", "ghostbranch"+exe)
	copyFile(t, bin, odd)
	must(t, env, co, odd, "guard", "install", "--json")

	push := func(args ...string) (string, error) {
		return run(t, env, co, "git", append([]string{"push", "-q"}, args...)...)
	}
	publicMain := func() string {
		return must(t, env, root, "git", "--git-dir", public.Dir, "rev-parse", "refs/heads/main")
	}

	// Review Focus 3: one push with public main and a feature is refused
	// whole, and the refusal names only the feature.
	byName := `refs/heads/feat-x: "feat-x" is a Ghostbranch feature`
	for _, c := range []struct {
		args []string
		why  string
	}{
		{[]string{"origin", "main", "feat-x"}, byName},
		{[]string{"origin", "feat-x"}, byName},
		{[]string{"origin", featTip + ":refs/heads/renamed"}, `refs/heads/renamed: it contains the commits of feature "feat-x"`},
	} {
		out, err := push(c.args...)
		if err == nil || !strings.Contains(out, "PUSH_BLOCKED") || !strings.Contains(out, c.why) {
			t.Fatalf("git push %v must be refused because %s:\n%s", c.args, c.why, out)
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "refs/heads/main:") {
				t.Fatalf("git push %v must not name public main:\n%s", c.args, out)
			}
		}
	}
	if publicMain() != m {
		t.Fatal("a refused push moved public main")
	}
	if out, err := push("origin", "main"); err != nil || publicMain() != mainTip {
		t.Fatalf("public main must go through: %v\n%s", err, out)
	}
	oldShadow := must(t, env, root, "git", "--git-dir", shadow.Dir, "rev-parse", "refs/heads/feat-x")
	if out, err := push("shadow", "feat-x"); err != nil {
		t.Fatalf("pushes to the shadow repo are untouched: %v\n%s", err, out)
	}
	log, _ := os.ReadFile(filepath.FromSlash(logf))
	if !strings.Contains(string(log), "lfs origin") || !strings.Contains(string(log), "lfs shadow") ||
		!strings.Contains(string(log), "refs/heads/main "+mainTip+" refs/heads/main "+m) ||
		!strings.Contains(string(log), "refs/heads/feat-x "+featTip+" refs/heads/feat-x "+oldShadow) {
		t.Fatalf("the chained hook must run with git's exact input:\n%s", log)
	}

	// The binary is gone (and not on PATH): public pushes are refused, not waved through.
	if err := os.Remove(odd); err != nil {
		t.Fatal(err)
	}
	var path []string
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(d, "ghostbranch"+exe)); err != nil {
			path = append(path, d)
		}
	}
	noBin := append(append([]string(nil), env...), "PATH="+strings.Join(path, string(os.PathListSeparator)))
	must(t, env, co, "git", "commit", "-q", "--allow-empty", "-m", "another fix")
	if out, err := run(t, noBin, co, "git", "push", "-q", "origin", "main"); err == nil || !strings.Contains(out, "cannot find the ghostbranch binary") {
		t.Fatalf("a missing binary must refuse the push:\n%s", out)
	}

	// Uninstall restores the original hook.
	must(t, env, co, bin, "guard", "uninstall", "--json")
	if b, _ := os.ReadFile(filepath.Join(co, ".git", "hooks", "pre-push")); string(b) != lfs {
		t.Fatalf("uninstall must restore the LFS hook:\n%s", b)
	}
}

// An agent's shell cannot waive a leak finding: exit 5 at once, before any
// network call, and nothing is written.
func TestAnAgentShellCannotWaive(t *testing.T) {
	home := workspace(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	_, _ = w.WriteString("yes\nyes\nyes\n")
	_ = w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "waivers", "add", "feat-x", "f-0123456789ab", "--reason", "fixture", "--repo", "acme/widget", "--json")
	cmd.Stdin = r
	cmd.Env = append(os.Environ(), "GHOSTBRANCH_HOME="+home, "GHOSTBRANCH_WORKSPACE=default",
		"GHOSTBRANCH_TOKEN=unused", "GHOSTBRANCH_ACTOR=agent:blackbox")
	out, _ := cmd.Output()
	if ctx.Err() != nil {
		t.Fatal("the binary waited for a console")
	}
	var env struct {
		ExitCode int `json:"exit_code"`
		Error    struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if env.ExitCode != 5 || cmd.ProcessState.ExitCode() != 5 || env.Error.Code != "APPROVAL_REQUIRED" || env.Error.Details["reason"] != "no_console" {
		t.Fatalf("%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, "workspaces", "default", "waivers.json")); !os.IsNotExist(err) {
		t.Fatal("a waiver was written")
	}
}
