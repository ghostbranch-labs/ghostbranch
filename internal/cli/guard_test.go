package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hermeticGit uses t.Setenv: its callers must stay serial (no t.Parallel).
func hermeticGit(t *testing.T) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// pairCheckout makes a clone-like checkout whose origin is acme/widget.
func pairCheckout(te *testEnv) string {
	te.t.Helper()
	dir := filepath.Join(te.root, "checkout")
	for _, args := range [][]string{{"init", "-q", "-b", "main", dir}, {"-C", dir, "remote", "add", "origin", "https://github.com/acme/widget.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			te.t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	te.env.Getwd = func() (string, error) { return dir, nil }
	return dir
}

// validData checks a successful envelope's data against guard.schema.json.
func validData(t *testing.T, env map[string]any) {
	t.Helper()
	raw, err := json.Marshal(env["data"])
	if err != nil {
		t.Fatal(err)
	}
	validate(t, "guard.schema.json", raw)
}

func TestGuardCommands(t *testing.T) {
	hermeticGit(t)
	te := newTestEnv(t)
	initOnly(te)
	delete(te.vars, "GHOSTBRANCH_TOKEN") // the guard needs no token
	dir := pairCheckout(te)
	// A remote for the shadow repo is never recorded as public.
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "shadow", "https://github.com/acme/widget-shadow.git").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	code, env := te.runJSON("guard", "install")
	data, _ := env["data"].(map[string]any)
	if code != 0 || data["action"] != "installed" {
		t.Fatalf("install: %d %v", code, env)
	}
	validData(t, env)
	hook, err := os.ReadFile(filepath.Join(dir, ".git", "hooks", "pre-push"))
	if err != nil || !strings.Contains(string(hook), "GB_REMOTES='origin'") || !strings.Contains(string(hook), "GB_PUBLIC='acme/widget'") {
		t.Fatalf("hook:\n%s %v", hook, err)
	}
	if code, env := te.runJSON("guard", "status"); code != 0 || env["data"].(map[string]any)["installed"] != true {
		t.Fatalf("status: %d %v", code, env)
	} else {
		validData(t, env)
	}
	// What the hook runs: a push of public main passes, a feature is refused.
	te.env.Stdin = strings.NewReader("refs/heads/main 1111111111111111111111111111111111111111 refs/heads/main 0000000000000000000000000000000000000000\n")
	if code, env := te.runJSON("guard", "check", "origin", "https://github.com/acme/widget.git"); code != 0 {
		t.Fatalf("check main: %d %v", code, env)
	} else {
		validData(t, env)
	}
	// It runs inside every public git push, so a pass prints nothing.
	te.env.Stdin = strings.NewReader("refs/heads/main 1111111111111111111111111111111111111111 refs/heads/main 0000000000000000000000000000000000000000\n")
	if code, out, errOut := te.run("guard", "check", "origin", "https://github.com/acme/widget.git"); code != 0 || out != "" || errOut != "" {
		t.Fatalf("check main (human): %d stdout %q stderr %q", code, out, errOut)
	}
	te.env.Stdin = strings.NewReader("refs/heads/x 1111111111111111111111111111111111111111 refs/heads/ghostbranch 0000000000000000000000000000000000000000\n")
	code, env = te.runJSON("guard", "check", "origin", "/some/rewritten/path")
	e, _ := env["error"].(map[string]any)
	if code != 1 || e["code"] != "PUSH_BLOCKED" {
		t.Fatalf("check overlay: %d %v", code, env)
	}
	details, _ := e["details"].(map[string]any)
	blocked, _ := details["blocked"].([]any)
	if len(blocked) != 1 || blocked[0].(map[string]any)["ref"] != "refs/heads/ghostbranch" {
		t.Fatalf("check overlay must name refs/heads/ghostbranch: %v", env)
	}
	// A remote the guard cannot place is refused, never waved through.
	te.env.Stdin = strings.NewReader("refs/heads/main 1111111111111111111111111111111111111111 refs/heads/main 0000000000000000000000000000000000000000\n")
	code, env = te.runJSON("guard", "check", "nosuch", "/local/path")
	if e, _ := env["error"].(map[string]any); code != 1 || e["code"] != "PUSH_BLOCKED" ||
		!strings.Contains(e["message"].(string), "cannot tell which public repo") ||
		!strings.Contains(e["message"].(string), "or remove the guard with ghostbranch guard uninstall") {
		t.Fatalf("check unknown remote: %d %v", code, env)
	}
	if code, env := te.runJSON("guard", "uninstall"); code != 0 || env["data"].(map[string]any)["action"] != "removed" {
		t.Fatalf("uninstall: %d %v", code, env)
	} else {
		validData(t, env)
	}
}

// The engine's mirror holds the recorded features. Only a mirror that does
// not exist yet means "no features"; one the guard cannot read refuses the
// push.
func TestGuardCheckMirror(t *testing.T) {
	mainLine := "refs/heads/main 1111111111111111111111111111111111111111 refs/heads/main 0000000000000000000000000000000000000000\n"
	cases := []struct {
		name    string
		mirror  func(t *testing.T, dir string)
		blocked bool
	}{
		{"no mirror yet", func(*testing.T, string) {}, false},
		{"mirror without HEAD", func(t *testing.T, dir string) {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"mirror is a file", func(t *testing.T, dir string) {
			if err := os.WriteFile(dir, []byte("not a repo\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hermeticGit(t)
			te := newTestEnv(t)
			initOnly(te)
			pairCheckout(te)
			mirror := filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default", "pairs", "acme__widget", "mirror.git")
			if err := os.RemoveAll(mirror); err != nil {
				t.Fatal(err)
			}
			c.mirror(t, mirror)
			te.env.Stdin = strings.NewReader(mainLine)
			code, env := te.runJSON("guard", "check", "origin", "https://github.com/acme/widget.git")
			if !c.blocked {
				if code != 0 {
					t.Fatalf("a push of public main with no mirror yet must pass: %d %v", code, env)
				}
				return
			}
			e, _ := env["error"].(map[string]any)
			if code != 1 || e["code"] != "PUSH_BLOCKED" ||
				!strings.Contains(e["message"].(string), "could not read the features of acme/widget") {
				t.Fatalf("an unreadable mirror must refuse the push: %d %v", code, env)
			}
		})
	}
}

// A remote counts as public when any of its URLs is the public repo: a
// pushurl too, for install's GB_REMOTES and for guard check's fallback.
func TestGuardCountsEveryURLOfARemote(t *testing.T) {
	hermeticGit(t)
	te := newTestEnv(t)
	initOnly(te)
	dir := pairCheckout(te)
	for _, args := range [][]string{
		{"-C", dir, "remote", "add", "mirror", "https://github.com/acme/mirror.git"},
		{"-C", dir, "config", "remote.mirror.pushurl", "https://github.com/acme/widget.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if code, env := te.runJSON("guard", "install"); code != 0 {
		t.Fatalf("install: %d %v", code, env)
	}
	hook, err := os.ReadFile(filepath.Join(dir, ".git", "hooks", "pre-push"))
	if err != nil || !strings.Contains(string(hook), "GB_REMOTES='mirror origin'") {
		t.Fatalf("hook:\n%s %v", hook, err)
	}
	te.env.Stdin = strings.NewReader("refs/heads/main 1111111111111111111111111111111111111111 refs/heads/main 0000000000000000000000000000000000000000\n")
	if code, env := te.runJSON("guard", "check", "mirror", "/some/rewritten/path"); code != 0 {
		t.Fatalf("check through mirror's pushurl: %d %v", code, env)
	}
}

// status and uninstall make install's pair check, but a guard that is
// installed stays visible and removable: guard check tells a person to
// uninstall a guard it can't place.
func TestGuardStatusAndUninstallCheckThePair(t *testing.T) {
	hermeticGit(t)
	te := newTestEnv(t)
	initOnly(te)
	dir := pairCheckout(te)
	setOrigin := func(url string) {
		t.Helper()
		if out, err := exec.Command("git", "-C", dir, "remote", "set-url", "origin", url).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	setOrigin("https://github.com/someone/else.git")
	for _, cmd := range [][]string{{"guard", "status"}, {"guard", "uninstall"}} {
		if code, env := te.runJSON(cmd...); code != 2 || env["error"].(map[string]any)["code"] != "NOT_A_PAIR_CHECKOUT" {
			t.Fatalf("%v outside a pair: %d %v", cmd, code, env)
		}
	}
	setOrigin("https://github.com/acme/widget.git")
	te.mustRun("guard", "install")
	setOrigin("https://github.com/someone/else.git")
	if code, env := te.runJSON("guard", "status"); code != 0 || env["data"].(map[string]any)["installed"] != true {
		t.Fatalf("status of an installed guard: %d %v", code, env)
	}
	if code, env := te.runJSON("guard", "uninstall"); code != 0 || env["data"].(map[string]any)["action"] != "removed" {
		t.Fatalf("uninstall of an installed guard: %d %v", code, env)
	}
}

func TestGuardOutsideAPairCheckout(t *testing.T) {
	hermeticGit(t)
	te := newTestEnv(t)
	initOnly(te)
	dir := pairCheckout(te)
	if out, err := exec.Command("git", "-C", dir, "remote", "set-url", "origin", "https://github.com/someone/else.git").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	code, env := te.runJSON("guard", "install")
	if e, _ := env["error"].(map[string]any); code != 2 || e["code"] != "NOT_A_PAIR_CHECKOUT" {
		t.Fatalf("%d %v", code, env)
	}
}
